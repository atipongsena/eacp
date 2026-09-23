package messaging_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"eacp/internal/messaging"
	"eacp/internal/storage/pgtest"
)

func (v *env) hints() *messaging.Hints {
	v.t.Helper()
	h, err := messaging.NewHints(v.f.App, v.js, messaging.HintOptions{FetchWait: 200 * time.Millisecond,
		Rebind: 100 * time.Millisecond, Log: quiet()})
	if err != nil {
		v.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	v.t.Cleanup(func() { cancel(); <-done })
	return h
}

func woke(h *messaging.Hints, d time.Duration) bool {
	select {
	case <-h.Wake():
		return true
	case <-time.After(d):
		return false
	}
}

func (v *env) consumerSettled() bool {
	c, err := v.js.Consumer(context.Background(), messaging.WorkStream, messaging.WorkConsumer)
	if err != nil {
		return false
	}
	info, err := c.Info(context.Background())
	return err == nil && info.NumAckPending == 0 && info.NumPending == 0
}

// ADR-014 §5: a hint is recorded in the inbox and wakes the worker; the same
// message id delivered again after the stream's duplicate window is ACKed
// without waking anyone (MASTER_PLAN §63).
func TestHintWakesOnceAndADuplicateIsAckedWithoutWaking(t *testing.T) {
	v := newEnv(t)
	h := v.hints()
	a := v.submit()
	v.runRelay(v.relay(v.js))
	if !woke(h, 5*time.Second) {
		t.Fatal("the hint did not wake the worker")
	}
	hintID := v.outboxID(a.ID, messaging.TopicQueued)
	if n := v.count(`SELECT count(*) FROM eacp.inbox_messages WHERE consumer = $1 AND message_id = $2`,
		messaging.WorkConsumer, hintID); n != 1 {
		t.Fatalf("inbox rows = %d", n)
	}
	if !eventually(5*time.Second, v.consumerSettled) {
		t.Fatal("the hint was not acknowledged")
	}

	time.Sleep(3 * testTopology.Duplicates) // past the stream's duplicate window
	dup := nats.NewMsg("eacp.work." + pgtest.TenantA + ".action.queued")
	dup.Data = []byte(`{"action_id":"` + a.ID.String() + `"}`)
	ack, err := v.js.PublishMsg(context.Background(), dup, jetstream.WithMsgID(hintID.String()))
	if err != nil || ack.Duplicate {
		t.Fatalf("republish = %+v, %v; want a new stream message", ack, err)
	}
	if woke(h, time.Second) {
		t.Fatal("a duplicate hint woke the worker")
	}
	if !eventually(5*time.Second, v.consumerSettled) {
		t.Fatal("the duplicate was not acknowledged")
	}
	if n := v.count(`SELECT count(*) FROM eacp.inbox_messages`); n != 1 {
		t.Fatalf("inbox rows = %d, want 1", n)
	}
}

// ADR-014 §5: malformed hints are terminated, never retried, never recorded
// and never wake the worker.
func TestMalformedHintsAreTerminatedWithoutWaking(t *testing.T) {
	v := newEnv(t)
	v.runRelay(v.relay(v.js)) // provisions the topology
	h := v.hints()
	tenant := "eacp.work." + pgtest.TenantA + ".action.queued"
	action := `{"action_id":"` + uuid.NewString() + `"}`
	for name, m := range map[string]struct {
		subject, id, body string
	}{
		"tenant is not a uuid":  {"eacp.work.tenant-a.action.queued", uuid.NewString(), action},
		"unknown topic":         {"eacp.work." + pgtest.TenantA + ".action.run", uuid.NewString(), action},
		"no message id":         {tenant, "", action},
		"message id not a uuid": {tenant, "hint-1", action},
		"action id not a uuid":  {tenant, uuid.NewString(), `{"action_id":"x"}`},
		"extra field":           {tenant, uuid.NewString(), `{"action_id":"` + uuid.NewString() + `","run":true}`},
		"trailing data":         {tenant, uuid.NewString(), action + `{}`},
		"duplicate key":         {tenant, uuid.NewString(), `{"action_id":"` + uuid.NewString() + `","action_id":"` + uuid.NewString() + `"}`},
		"unknown tenant":        {"eacp.work." + uuid.NewString() + ".action.queued", uuid.NewString(), action},
	} {
		msg := nats.NewMsg(m.subject)
		msg.Data = []byte(m.body)
		var opts []jetstream.PublishOpt
		if m.id != "" {
			opts = append(opts, jetstream.WithMsgID(m.id))
		}
		if _, err := v.js.PublishMsg(context.Background(), msg, opts...); err != nil {
			t.Fatalf("%s: publish: %v", name, err)
		}
	}
	if woke(h, 2*time.Second) {
		t.Fatal("a malformed hint woke the worker")
	}
	if !eventually(5*time.Second, v.consumerSettled) {
		t.Fatal("malformed hints were left pending")
	}
	c, err := v.js.Consumer(context.Background(), messaging.WorkStream, messaging.WorkConsumer)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil || info.NumRedelivered != 0 || info.Delivered.Consumer != 9 {
		t.Fatalf("consumer = %+v, %v; want 9 deliveries and no redelivery", info, err)
	}
	if n := v.count(`SELECT count(*) FROM eacp.inbox_messages`); n != 0 {
		t.Fatalf("inbox rows = %d", n)
	}
}

// A worker that starts before the relay has provisioned the topology keeps
// rebinding and receives hints once it exists.
func TestHintConsumerBindsOnceTheTopologyExists(t *testing.T) {
	v := newEnv(t)
	h := v.hints()
	time.Sleep(300 * time.Millisecond) // a few failed binds
	v.submit()
	v.runRelay(v.relay(v.js))
	if !woke(h, 5*time.Second) {
		t.Fatal("the hint consumer never bound")
	}
}

// ADR-014 §1, invariant 4: a hint only wakes the claim loop. A worker whose
// poll interval is a minute executes a hinted action within seconds, once.
func TestHintedWorkerExecutesLongBeforeItsPollInterval(t *testing.T) {
	v := newEnv(t)
	h := v.hints()
	w := v.worker(time.Minute, h.Wake())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	time.Sleep(200 * time.Millisecond) // the loop's first claim finds nothing

	r := v.relay(v.js)
	a := v.submit()
	start := time.Now()
	v.runRelay(r)
	if !eventually(10*time.Second, func() bool { return v.get(a.ID).State == "SUCCEEDED" }) {
		t.Fatalf("state = %s after %v", v.get(a.ID).State, time.Since(start))
	}
	// The hinted run's own transitions produce more events and no second call.
	v.runRelay(r)
	time.Sleep(300 * time.Millisecond)
	if n := v.calls.Load(); n != 1 {
		t.Fatalf("connector calls = %d, want 1", n)
	}
}

// ADR-014 §1: without NATS the system stays correct, only slower — the
// worker's poll executes the action and the relay catches up afterwards.
func TestWithoutNATSTheWorkerStillExecutes(t *testing.T) {
	v := newEnv(t)
	h := v.hints()
	r := v.relay(v.js)
	v.runRelay(r)
	v.nats.Stop()
	w := v.worker(100*time.Millisecond, h.Wake())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	defer func() { cancel(); <-stopped }()

	a := v.submit()
	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("relay published with NATS down")
	}
	if !eventually(10*time.Second, func() bool { return v.get(a.ID).State == "SUCCEEDED" }) {
		t.Fatalf("state = %s with NATS down", v.get(a.ID).State)
	}
	v.nats.Restart()
	if !eventually(15*time.Second, func() bool {
		r.RunOnce(context.Background())
		return v.count(`SELECT count(*) FROM eacp.outbox_events WHERE published_at IS NULL`) == 0
	}) {
		t.Fatal("the outbox did not drain after NATS recovered")
	}
	time.Sleep(500 * time.Millisecond) // the late hint is consumed and claims nothing
	if n := v.calls.Load(); n != 1 {
		t.Fatalf("connector calls = %d, want 1", n)
	}
}
