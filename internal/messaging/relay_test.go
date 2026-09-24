package messaging_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"eacp/internal/messaging"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func (v *env) runRelay(r *messaging.Relay) int {
	v.t.Helper()
	n, err := r.RunOnce(context.Background())
	if err != nil {
		v.t.Fatalf("relay: %v", err)
	}
	return n
}

// ADR-014 §3, §4: each row is published once to its tenant's subject, with
// the row id as Nats-Msg-Id, the traceparent and the stored payload, and
// is marked published only after the PubAck.
func TestRelayPublishesEachRowOnceWithItsIDAndTraceparent(t *testing.T) {
	v := newEnv(t)
	a := v.submit()
	rows := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE aggregate_id = $1`, a.ID)
	r := v.relay(v.js)
	if n := v.runRelay(r); n != rows {
		t.Fatalf("published %d, want %d", n, rows)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`); n != 0 {
		t.Fatalf("%d rows still unpublished", n)
	}
	if n := v.runRelay(r); n != 0 {
		t.Fatalf("second pass published %d", n)
	}

	ctx := context.Background()
	hintID := v.outboxID(a.ID, messaging.TopicQueued)
	work, err := v.js.Stream(ctx, messaging.WorkStream)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := work.GetLastMsgForSubject(ctx, "eacp.work."+pgtest.TenantA+".action.queued")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(msg.Data, &body); err != nil || len(body) != 1 || body["action_id"] != a.ID.String() {
		t.Fatalf("hint body = %s, err = %v", msg.Data, err)
	}
	if got := msg.Header.Get(jetstream.MsgIDHeader); got != hintID.String() {
		t.Fatalf("Nats-Msg-Id = %q, want %s", got, hintID)
	}
	if got := msg.Header.Get("traceparent"); got != traceparent {
		t.Fatalf("traceparent = %q", got)
	}
	transitions := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE topic = 'action.transition'`)
	if got := v.streamMsgs(messaging.EventStream); got != uint64(transitions) || v.streamMsgs(messaging.WorkStream) != 1 {
		t.Fatalf("events = %d (want %d), hints = %d", got, transitions, v.streamMsgs(messaging.WorkStream))
	}
}

func TestKillChangeIsATransactionalBroadcastSignal(t *testing.T) {
	v := newEnv(t)
	a := v.submit()
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, v.f.P["otto"]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.set_kill('action', $1, true, 'incident')`, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE topic = 'kill.changed'`); n != 1 {
		t.Fatalf("kill outbox rows = %d", n)
	}
	v.runRelay(v.relay(v.js))
	stream, err := v.js.Stream(ctx, messaging.EventStream)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := stream.GetLastMsgForSubject(ctx, "eacp.events."+pgtest.TenantA+".kill.changed")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(msg.Data, &body); err != nil || body["scope"] != "action" ||
		body["target_id"] != a.ID.String() || body["epoch"] != float64(1) || len(body) != 3 {
		t.Fatalf("kill signal = %s, err = %v", msg.Data, err)
	}
}

func (v *env) outboxID(action uuid.UUID, topic string) uuid.UUID {
	v.t.Helper()
	ctx := context.Background()
	var ids []uuid.UUID
	if err := storage.InTenantTx(ctx, v.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM eacp.outbox_events WHERE aggregate_id = $1 AND topic = $2`, action, topic)
		if err == nil {
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		return err
	}); err != nil || len(ids) != 1 {
		v.t.Fatalf("%s rows for %s = %v, err = %v", topic, action, ids, err)
	}
	return ids[0]
}

// A row republished after a crash between PubAck and commit is dropped by
// the stream's duplicate window, and the row is still marked.
func TestRepublishedRowIsDeduplicatedByTheStream(t *testing.T) {
	v := newEnv(t)
	a := v.submit()
	r := messaging.NewRelay(v.f.App, v.js, messaging.RelayOptions{
		PublishTimeout: time.Second, Topology: messaging.Topology{Duplicates: time.Minute, AckWait: time.Second}, Log: quiet(),
	})
	v.runRelay(r)
	before := v.streamMsgs(messaging.WorkStream) + v.streamMsgs(messaging.EventStream)
	s := schema{f: v.f}
	s.admin(t, `UPDATE eacp.outbox_events SET published_at = NULL WHERE aggregate_id = $1`, a.ID)
	if n := v.runRelay(r); n == 0 {
		t.Fatal("the reset rows were not marked again")
	}
	if after := v.streamMsgs(messaging.WorkStream) + v.streamMsgs(messaging.EventStream); after != before {
		t.Fatalf("stream messages %d -> %d: the republish was not deduplicated", before, after)
	}
}

// Concurrent relays lock rows with SKIP LOCKED: every row is published by
// exactly one of them.
func TestConcurrentRelaysPublishEachRowOnce(t *testing.T) {
	v := newEnv(t)
	for range 10 {
		v.submit()
	}
	total := v.count(`SELECT count(*) FROM eacp.outbox_events`)
	_, js2 := v.nats.Connect(t)
	relays := []*messaging.Relay{v.relay(v.js), v.relay(js2)}
	for _, r := range relays {
		r.Batch = 3
	}
	var mu sync.Mutex
	sum := 0
	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Go(func() {
			for {
				n, err := r.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				sum += n
				mu.Unlock()
				if n == 0 && v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`) == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if sum != total {
		t.Fatalf("relays marked %d rows, want %d", sum, total)
	}
	if got := v.streamMsgs(messaging.WorkStream) + v.streamMsgs(messaging.EventStream); got != uint64(total) {
		t.Fatalf("stream messages = %d, want %d", got, total)
	}
}

// Chaos (MASTER_PLAN §73, "NATS disconnect"): an outage leaves the rows
// unpublished, bounded by the publish timeout, and they drain afterwards.
func TestNATSOutageLeavesRowsUnpublishedUntilItRecovers(t *testing.T) {
	v := newEnv(t)
	r := v.relay(v.js)
	v.runRelay(r) // provisions the topology
	v.nats.Stop()
	a := v.submit()
	start := time.Now()
	n, err := r.RunOnce(context.Background())
	if err == nil || n != 0 {
		t.Fatalf("relay during outage = %d, %v", n, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("relay pass took %v during the outage", time.Since(start))
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL AND aggregate_id = $1`, a.ID); n == 0 {
		t.Fatal("rows were marked published without a PubAck")
	}
	v.nats.Restart()
	if !eventually(15*time.Second, func() bool {
		r.RunOnce(context.Background())
		return v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`) == 0
	}) {
		t.Fatal("the outbox did not drain after NATS recovered")
	}
	if v.streamMsgs(messaging.WorkStream) != 1 {
		t.Fatalf("hints = %d, want 1", v.streamMsgs(messaging.WorkStream))
	}
}

// ADR-014 §4: a topic the relay does not know is never published and never
// stalls the relay.
func TestUnknownTopicsAreNeverPublished(t *testing.T) {
	v := newEnv(t)
	a := v.submit()
	s := schema{f: v.f}
	s.admin(t, `INSERT INTO eacp.outbox_events (tenant_id, topic, aggregate_id, payload, created_at)
		VALUES ($1, 'kill.switch', $2, '{"everything":"stop"}', now() - interval '1 minute')`, pgtest.TenantA, a.ID)
	r := v.relay(v.js)
	r.Batch = 1
	for v.runRelay(r) > 0 {
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`); n != 1 {
		t.Fatalf("unpublished = %d, want only the foreign topic", n)
	}
	if n := v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL AND topic = 'kill.switch'`); n != 1 {
		t.Fatal("the foreign topic was published")
	}
}

// ADR-014 §4: a dashboard reads its tenant's events in order from the
// event stream; another tenant's filter sees none of them.
func TestDashboardEventsArrivePerTenant(t *testing.T) {
	v := newEnv(t)
	a, b := v.submit(), v.submit()
	v.runRelay(v.relay(v.js))
	ctx := context.Background()
	read := func(tenant string) []string {
		c, err := v.js.OrderedConsumer(ctx, messaging.EventStream, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{"eacp.events." + tenant + ".>"},
		})
		if err != nil {
			t.Fatal(err)
		}
		batch, err := c.FetchNoWait(100)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for m := range batch.Messages() {
			if !strings.HasPrefix(m.Subject(), "eacp.events."+tenant+".") {
				t.Fatalf("subject %s outside tenant %s", m.Subject(), tenant)
			}
			var e struct {
				ActionID uuid.UUID `json:"action_id"`
				From     *string   `json:"from"`
				To       string    `json:"to"`
				At       time.Time `json:"at"`
			}
			if err := json.Unmarshal(m.Data(), &e); err != nil || e.At.IsZero() {
				t.Fatalf("event %s: %v", m.Data(), err)
			}
			from := "-"
			if e.From != nil {
				from = *e.From
			}
			out = append(out, e.ActionID.String()[:8]+":"+from+">"+e.To)
		}
		return out
	}
	got := read(pgtest.TenantA)
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		var chain []string
		for _, e := range got {
			if strings.HasPrefix(e, id.String()[:8]) {
				chain = append(chain, e[9:])
			}
		}
		if strings.Join(chain, " ") != "->RECEIVED RECEIVED>AUTHORIZED AUTHORIZED>QUEUED" {
			t.Fatalf("events for %s = %v", id, chain)
		}
	}
	if other := read(pgtest.TenantB); len(other) != 0 {
		t.Fatalf("tenant B sees %v", other)
	}
}
