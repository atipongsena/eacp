package messaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/atipongsena/eacp/internal/storage"
)

var consumerPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Inbox records the messages a consumer has processed (MASTER_PLAN §63), so
// a message delivered again — a redelivery, or a republish after the
// stream's duplicate window — is ACKed and not processed twice.
type Inbox struct {
	pool      *pgxpool.Pool
	consumer  string
	retention time.Duration
}

// NewInbox returns the inbox of consumer. Records older than retention
// (default 24h, at least 1h) are pruned as new ones arrive.
func NewInbox(pool *pgxpool.Pool, consumer string, retention time.Duration) (*Inbox, error) {
	if !consumerPattern.MatchString(consumer) {
		return nil, fmt.Errorf("messaging: invalid consumer name %q", consumer)
	}
	if retention == 0 {
		retention = 24 * time.Hour
	}
	if retention < time.Hour {
		return nil, errors.New("messaging: inbox retention must be at least 1h")
	}
	return &Inbox{pool: pool, consumer: consumer, retention: retention}, nil
}

// Record stores messageID for tenant and reports whether it is new. A
// false result is a duplicate: ACK it and do nothing else.
func (i *Inbox) Record(ctx context.Context, tenant, messageID uuid.UUID) (bool, error) {
	var fresh bool
	err := storage.InTenantTx(ctx, i.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, InboxComponent); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO eacp.inbox_messages (tenant_id, consumer, message_id)
			VALUES (eacp.current_tenant_id(), $1, $2) ON CONFLICT DO NOTHING`, i.consumer, messageID)
		if err != nil {
			return err
		}
		fresh = tag.RowsAffected() == 1
		if !fresh {
			return nil
		}
		// Amortized retention: each new record removes a few expired ones.
		_, err = tx.Exec(ctx, `DELETE FROM eacp.inbox_messages WHERE ctid IN (
			SELECT ctid FROM eacp.inbox_messages
			WHERE consumer = $1 AND processed_at < now() - make_interval(secs => $2)
			LIMIT 10)`, i.consumer, i.retention.Seconds())
		return err
	})
	return fresh, err
}

// HintOptions configure the worker's hint consumer.
type HintOptions struct {
	// FetchWait bounds one pull request (default 5s).
	FetchWait time.Duration
	// Rebind is the wait before binding again after the consumer or the
	// connection failed (default 2s).
	Rebind time.Duration
	// InboxRetention is how long processed hints are remembered (default 24h).
	InboxRetention time.Duration
	Log            *slog.Logger
}

// Hints is the execution worker's work-hint consumer (ADR-014 §5). It pulls
// from the shared durable consumer, validates each hint strictly, records
// it in the inbox and wakes the worker's claim loop. It never claims or
// executes anything itself.
type Hints struct {
	js    jetstream.JetStream
	inbox *Inbox
	o     HintOptions
	wake  chan struct{}
}

// NewHints returns a hint consumer over pool (the application role) and js.
func NewHints(pool *pgxpool.Pool, js jetstream.JetStream, o HintOptions) (*Hints, error) {
	if o.FetchWait <= 0 {
		o.FetchWait = 5 * time.Second
	}
	if o.Rebind <= 0 {
		o.Rebind = 2 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	inbox, err := NewInbox(pool, WorkConsumer, o.InboxRetention)
	if err != nil {
		return nil, err
	}
	return &Hints{js: js, inbox: inbox, o: o, wake: make(chan struct{}, 1)}, nil
}

// Wake fires after a new hint; pass it as worker.Options.Wake.
func (h *Hints) Wake() <-chan struct{} { return h.wake }

// Run consumes hints until ctx is cancelled. A missing consumer (the relay
// has not provisioned it yet, or NATS lost its storage) or a broken
// connection is retried every Rebind; meanwhile the worker keeps polling.
func (h *Hints) Run(ctx context.Context) {
	failing := false
	fail := func(err error) {
		if !failing && ctx.Err() == nil {
			h.o.Log.WarnContext(ctx, "work hints unavailable; the worker polls", "err", err)
			failing = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.o.Rebind):
		}
	}
	for ctx.Err() == nil {
		c, err := h.js.Consumer(ctx, WorkStream, WorkConsumer)
		if err != nil {
			fail(err)
			continue
		}
		for ctx.Err() == nil {
			fctx, cancel := context.WithTimeout(ctx, h.o.FetchWait)
			batch, err := c.Fetch(16, jetstream.FetchContext(fctx))
			if err == nil {
				for msg := range batch.Messages() {
					h.handle(ctx, msg)
				}
				err = batch.Error()
			}
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				fail(err)
				break
			}
			if failing {
				h.o.Log.InfoContext(ctx, "work hints available")
				failing = false
			}
		}
	}
}

// handle processes one hint: Term if malformed or permanently unrecordable,
// NAK (redelivered later) on a transient database error, otherwise record
// it, wake the claim loop if it is new, and ACK.
func (h *Hints) handle(ctx context.Context, msg jetstream.Msg) {
	tenant, action, id, ok := parseHint(msg)
	if !ok {
		_ = msg.TermWithReason("malformed work hint")
		h.o.Log.WarnContext(ctx, "malformed work hint terminated", "subject", msg.Subject())
		return
	}
	carrier := propagation.MapCarrier{"traceparent": msg.Headers().Get("traceparent")}
	ctx, span := otel.Tracer("github.com/atipongsena/eacp/messaging").Start(
		otel.GetTextMapPropagator().Extract(ctx, carrier), "work hint")
	defer span.End()

	fresh, err := h.inbox.Record(ctx, tenant, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23")) {
			_ = msg.TermWithReason("work hint cannot be recorded")
			h.o.Log.WarnContext(ctx, "work hint terminated", "tenant", tenant.String(), "err", err)
			return
		}
		_ = msg.NakWithDelay(h.o.Rebind)
		h.o.Log.WarnContext(ctx, "work hint not recorded; redelivered later", "err", err)
		return
	}
	if fresh {
		select { // the loop claims whatever is claimable; one pending wake-up is enough
		case h.wake <- struct{}{}:
		default:
		}
	} else {
		h.o.Log.DebugContext(ctx, "duplicate work hint acknowledged", "action_id", action.String())
	}
	_ = msg.Ack()
}

// parseHint accepts exactly subject eacp.work.<tenant>.action.queued,
// Nats-Msg-Id <uuid> and body {"action_id":"<uuid>"}, all in canonical
// lower-case form, as the relay publishes them.
func parseHint(msg jetstream.Msg) (tenant, action, id uuid.UUID, ok bool) {
	parts := strings.Split(msg.Subject(), ".")
	if len(parts) != 5 || parts[0] != "eacp" || parts[1] != "work" || parts[3]+"."+parts[4] != TopicQueued {
		return tenant, action, id, false
	}
	var ok1, ok2, ok3 bool
	tenant, ok1 = canonicalUUID(parts[2])
	id, ok2 = canonicalUUID(msg.Headers().Get(jetstream.MsgIDHeader))
	body := string(msg.Data())
	const prefix, suffix = `{"action_id":"`, `"}`
	if strings.HasPrefix(body, prefix) && strings.HasSuffix(body, suffix) {
		action, ok3 = canonicalUUID(body[len(prefix) : len(body)-len(suffix)])
	}
	return tenant, action, id, ok1 && ok2 && ok3
}

func canonicalUUID(s string) (uuid.UUID, bool) {
	u, err := uuid.Parse(s)
	return u, err == nil && u.String() == s
}
