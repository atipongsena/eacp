// Package messaging carries EACP's signals over NATS JetStream (ADR-014,
// MASTER_PLAN §60, §62, §63, §84).
//
// PostgreSQL is the only execution authority. The relay publishes the
// transactional outbox (work hints and dashboard events), marking a row
// published only after the stream's PubAck. The worker's hint consumer
// dedups each message in the inbox and wakes the claim loop, which claims
// and fences through PostgreSQL as always. Messages may be duplicated,
// lost, delayed or reordered; without NATS the system is correct, only
// slower.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Outbox topics, streams and the shared work-hint consumer (ADR-014 §4).
const (
	TopicQueued     = "action.queued"
	TopicTransition = "action.transition"

	WorkStream   = "EACP_WORK"
	EventStream  = "EACP_EVENTS"
	WorkConsumer = "execution-worker"

	// OutboxComponent and InboxComponent are the messaging actors
	// (storage.SetSystem). eacp.actor_context() rejects both, so neither
	// can change an action.
	OutboxComponent = "outbox"
	InboxComponent  = "inbox"
)

// streams maps each topic the relay publishes to its subject prefix; the
// subject is prefix + tenant + "." + topic. Any other topic is never
// published (fail closed).
var streams = map[string]string{
	TopicQueued:     "eacp.work.",
	TopicTransition: "eacp.events.",
}

// Topics lists the topics the relay publishes.
func Topics() []string { return []string{TopicQueued, TopicTransition} }

// Subject returns the subject of topic for tenant, or false for a topic the
// relay does not publish.
func Subject(topic string, tenant uuid.UUID) (string, bool) {
	prefix, ok := streams[topic]
	if !ok {
		return "", false
	}
	return prefix + tenant.String() + "." + topic, true
}

// Topology holds the tunable parts of the streams and the work consumer.
// The zero value means the defaults.
type Topology struct {
	// Duplicates is each stream's duplicate window (default 2m): a row
	// republished within it (the same Nats-Msg-Id) is dropped by the stream.
	Duplicates time.Duration
	// AckWait is how long the work consumer waits for an ACK before
	// redelivering a hint (default 30s).
	AckWait time.Duration
}

func (t Topology) withDefaults() Topology {
	if t.Duplicates <= 0 {
		t.Duplicates = 2 * time.Minute
	}
	if t.AckWait <= 0 {
		t.AckWait = 30 * time.Second
	}
	return t
}

// Provision creates or updates both streams and the shared work consumer.
// It is idempotent; the relay calls it at startup and after any publish
// error.
func (t Topology) Provision(ctx context.Context, js jetstream.JetStream) error {
	t = t.withDefaults()
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        WorkStream,
		Description: "EACP work-available hints (action ids only; PostgreSQL is the authority)",
		Subjects:    []string{"eacp.work.>"},
		Retention:   jetstream.WorkQueuePolicy,
		Storage:     jetstream.FileStorage,
		MaxAge:      time.Hour,
		MaxMsgs:     1_000_000,
		Discard:     jetstream.DiscardOld,
		Duplicates:  t.Duplicates,
	}); err != nil {
		return fmt.Errorf("messaging: provision %s: %w", WorkStream, err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        EventStream,
		Description: "EACP action events for dashboards (best effort; the Action API is authoritative)",
		Subjects:    []string{"eacp.events.>"},
		Retention:   jetstream.LimitsPolicy,
		Storage:     jetstream.FileStorage,
		MaxAge:      24 * time.Hour,
		MaxBytes:    1 << 30,
		Discard:     jetstream.DiscardOld,
		Duplicates:  t.Duplicates,
	}); err != nil {
		return fmt.Errorf("messaging: provision %s: %w", EventStream, err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, WorkStream, jetstream.ConsumerConfig{
		Durable:       WorkConsumer,
		Description:   "execution workers: a hint only wakes the PostgreSQL claim loop",
		FilterSubject: "eacp.work.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       t.AckWait,
		MaxDeliver:    5,
	}); err != nil {
		return fmt.Errorf("messaging: provision consumer %s: %w", WorkConsumer, err)
	}
	return nil
}

// Connect opens a NATS connection that never gives up: it retries a
// failed first connection and reconnects forever, because NATS is not a
// dependency of correctness. Publishes while disconnected fail at once
// (no reconnect buffer), so the relay never has a hint in flight that it
// has not seen acknowledged. caFile adds a CA for tls:// URLs.
func Connect(rawURL, caFile, name string, log *slog.Logger) (*nats.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") {
		return nil, errors.New("messaging: EACP_NATS_URL must be a nats:// or tls:// URL")
	}
	opts := []nats.Option{
		nats.Name(name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ReconnectBufSize(-1),
		nats.Timeout(5 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("NATS disconnected; workers fall back to polling", "err", err)
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("NATS reconnected") }),
	}
	if caFile != "" {
		opts = append(opts, nats.RootCAs(caFile))
	}
	nc, err := nats.Connect(rawURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("messaging: connect: %w", err)
	}
	return nc, nil
}
