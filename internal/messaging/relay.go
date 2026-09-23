package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"eacp/internal/storage"
)

// RelayOptions configure a Relay.
type RelayOptions struct {
	// Batch bounds the rows handled per pass (default 100, at most 1000).
	Batch int
	// PublishTimeout bounds the publishes of one tenant batch, PubAcks
	// included (default 5s).
	PublishTimeout time.Duration
	Topology       Topology
	Log            *slog.Logger
}

// Relay publishes the transactional outbox to JetStream (ADR-014 §3). Rows
// are locked per tenant with SKIP LOCKED, published in created_at order
// with their id as Nats-Msg-Id, and marked published only after the PubAck.
// Delivery is at least once; the stream's duplicate window and the inbox
// drop repeats.
type Relay struct {
	// Batch bounds the rows handled per pass.
	Batch int

	pool        *pgxpool.Pool
	js          jetstream.JetStream
	o           RelayOptions
	provisioned bool
}

// NewRelay returns a Relay over pool (the application role) and js.
func NewRelay(pool *pgxpool.Pool, js jetstream.JetStream, o RelayOptions) *Relay {
	if o.Batch <= 0 {
		o.Batch = 100
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = 5 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Relay{Batch: min(o.Batch, 1000), pool: pool, js: js, o: o}
}

type outboxRow struct {
	id          uuid.UUID
	topic       string
	payload     []byte
	traceparent *string
}

// RunOnce publishes up to Batch unpublished rows and returns how many it
// marked published. It provisions the topology first when needed; an error
// stops the pass, leaving the remaining rows for the next one.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	if !r.provisioned {
		pctx, cancel := context.WithTimeout(ctx, r.o.PublishTimeout)
		err := r.o.Topology.Provision(pctx, r.js)
		cancel()
		if err != nil {
			return 0, err
		}
		r.provisioned = true
	}
	order, byTenant, err := rowsByTenant(ctx, r.pool, `SELECT tenant_id, id FROM eacp.outbox_pending($1, $2)`,
		Topics(), r.Batch)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, tenant := range order {
		n, err := r.publishTenant(ctx, tenant, byTenant[tenant])
		published += n
		if err != nil {
			r.provisioned = false // a lost stream is recreated on the next pass
			return published, err
		}
	}
	return published, nil
}

// publishTenant locks, publishes and marks the tenant's rows in one
// transaction. It commits the rows acknowledged before any failure. All
// publishes of the batch share one PublishTimeout, so a slow NATS holds the
// transaction (and its outbox row locks) for at most that long.
func (r *Relay) publishTenant(ctx context.Context, tenant uuid.UUID, ids []uuid.UUID) (int, error) {
	var marked int
	var pubErr error
	err := storage.InTenantTx(ctx, r.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, OutboxComponent); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, topic, payload::text, traceparent FROM eacp.outbox_events
			WHERE id = ANY ($1) AND published_at IS NULL
			ORDER BY created_at, id FOR UPDATE SKIP LOCKED`, ids)
		if err != nil {
			return err
		}
		locked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxRow, error) {
			var o outboxRow
			var payload string
			err := row.Scan(&o.id, &o.topic, &payload, &o.traceparent)
			o.payload = []byte(payload)
			return o, err
		})
		if err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, r.o.PublishTimeout)
		defer cancel()
		var acked []uuid.UUID
		for _, o := range locked {
			if pubErr = r.publish(pctx, tenant, o); pubErr != nil {
				break
			}
			acked = append(acked, o.id)
		}
		if len(acked) > 0 {
			tag, err := tx.Exec(ctx, `UPDATE eacp.outbox_events SET published_at = now() WHERE id = ANY ($1)`, acked)
			if err != nil {
				return err
			}
			marked = int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return marked, pubErr
}

// publish sends one row and waits for the stream's PubAck. A duplicate
// PubAck (the row was published before its mark committed) counts as
// published.
func (r *Relay) publish(ctx context.Context, tenant uuid.UUID, o outboxRow) error {
	subject, ok := Subject(o.topic, tenant)
	if !ok {
		return fmt.Errorf("messaging: outbox topic %q is not published", o.topic)
	}
	var body bytes.Buffer
	if err := json.Compact(&body, o.payload); err != nil {
		return fmt.Errorf("messaging: outbox row %s: %w", o.id, err)
	}
	msg := nats.NewMsg(subject)
	msg.Data = body.Bytes()
	if o.traceparent != nil {
		msg.Header.Set("traceparent", *o.traceparent)
	}
	ack, err := r.js.PublishMsg(ctx, msg, jetstream.WithMsgID(o.id.String()), jetstream.WithRetryAttempts(0))
	if err != nil {
		return fmt.Errorf("messaging: publish %s: %w", o.id, err)
	}
	if ack.Duplicate {
		r.o.Log.InfoContext(ctx, "outbox row was already in the stream", "outbox_id", o.id.String(), "topic", o.topic)
	}
	return nil
}

// Run relays every interval until ctx is cancelled. A pass that published
// a full batch runs again at once.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	failing := false
	for ctx.Err() == nil {
		n, err := r.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil && !failing:
			r.o.Log.WarnContext(ctx, "outbox relay failed; rows stay unpublished", "err", err)
			failing = true
		case err == nil && failing:
			r.o.Log.InfoContext(ctx, "outbox relay recovered")
			failing = false
		}
		if err == nil && n >= r.Batch {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// PruneOutbox deletes up to limit expired outbox rows (published an hour
// ago, or never published for a day) and returns how many it deleted. It
// runs whether or not NATS is configured, so the outbox stays bounded.
func PruneOutbox(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	order, byTenant, err := rowsByTenant(ctx, pool, `SELECT tenant_id, id FROM eacp.outbox_prunable($1)`, limit)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, tenant := range order {
		ids := byTenant[tenant]
		err := storage.InTenantTx(ctx, pool, tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetSystem(ctx, tx, OutboxComponent); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `DELETE FROM eacp.outbox_events
				WHERE id = ANY ($1) AND eacp.outbox_expired(published_at, created_at)`, ids)
			if err == nil {
				deleted += int(tag.RowsAffected())
			}
			return err
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return deleted, errors.Join(errs...)
}

// rowsByTenant runs a cross-tenant hint returning (tenant_id, id) pairs and
// groups the ids by tenant, keeping the order in which tenants appear.
func rowsByTenant(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]uuid.UUID, map[uuid.UUID][]uuid.UUID, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var order []uuid.UUID
	byTenant := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var tenant, id uuid.UUID
		if err := rows.Scan(&tenant, &id); err != nil {
			return nil, nil, err
		}
		if _, seen := byTenant[tenant]; !seen {
			order = append(order, tenant)
		}
		byTenant[tenant] = append(byTenant[tenant], id)
	}
	return order, byTenant, rows.Err()
}

// RunPruner prunes the outbox every interval until ctx is cancelled.
func RunPruner(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for {
			n, err := PruneOutbox(ctx, pool, 1000)
			if err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "outbox pruning failed", "err", err)
			}
			if err != nil || n < 1000 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
