package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/atipongsena/eacp/internal/messaging"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type schema struct {
	f     *registrytest.Fixture
	agent registrytest.Agent
}

func newSchema(t *testing.T) schema {
	t.Helper()
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "purchase")
	s := schema{f: f, agent: f.ActiveAgent(t, "buyer", tool.Tool)}
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	return s
}

// admin runs sql as the superuser with triggers off, to set up states the
// guards would never allow (backdating, foreign topics).
func (s schema) admin(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, s.f.DB.AdminDSN)
	must(t, err)
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `SET session_replication_role = replica`)
	must(t, err)
	_, err = conn.Exec(ctx, sql, args...)
	must(t, err)
}

type outboxRow struct {
	ID          uuid.UUID
	Topic       string
	Payload     map[string]any
	Traceparent *string
}

func (s schema) outbox(t *testing.T, action uuid.UUID) []outboxRow {
	t.Helper()
	ctx := context.Background()
	var out []outboxRow
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, topic, payload::text, traceparent FROM eacp.outbox_events
			WHERE aggregate_id = $1 ORDER BY created_at, topic`, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r outboxRow
			var payload string
			if err := rows.Scan(&r.ID, &r.Topic, &payload, &r.Traceparent); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(payload), &r.Payload); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}))
	return out
}

// ADR-014 §2: every state change writes one dashboard event in the same
// transaction, carrying only the action id, the states and the time.
func TestTransitionsWriteDashboardEvents(t *testing.T) {
	s := newSchema(t)
	id := s.f.QueuedAction(t, s.agent.Version, "carol", "erp.purchase")
	var transitions [][2]any
	queued := 0
	for _, r := range s.outbox(t, id) {
		switch r.Topic {
		case messaging.TopicQueued:
			queued++
		case messaging.TopicTransition:
			keys := make([]string, 0, len(r.Payload))
			for k := range r.Payload {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"action_id", "at", "from", "to"}) || r.Payload["action_id"] != id.String() {
				t.Fatalf("event payload = %v", r.Payload)
			}
			transitions = append(transitions, [2]any{r.Payload["from"], r.Payload["to"]})
		default:
			t.Fatalf("unexpected topic %q", r.Topic)
		}
	}
	want := [][2]any{{nil, "RECEIVED"}, {"RECEIVED", "AUTHORIZED"}, {"AUTHORIZED", "QUEUED"}}
	if queued != 1 || !slices.Equal(transitions, want) {
		t.Fatalf("queued hints = %d, transitions = %v, want 1 and %v", queued, transitions, want)
	}

	// A cancellation's free-text reason never reaches the event.
	must(t, s.f.Exec("otto", `UPDATE eacp.actions SET state = 'CANCELLED', state_reason = 'secret-ish free text'
		WHERE id = $1`, id))
	rows := s.outbox(t, id)
	last := rows[len(rows)-1]
	if last.Topic != messaging.TopicTransition || last.Payload["to"] != "CANCELLED" || len(last.Payload) != 4 {
		t.Fatalf("cancel event = %+v", last)
	}
}

// ADR-014 §2: outbox rows come only from the action triggers.
func TestOutboxRowsComeOnlyFromTheActionTriggers(t *testing.T) {
	s := newSchema(t)
	id := s.f.QueuedAction(t, s.agent.Version, "carol", "erp.purchase")
	insert := `INSERT INTO eacp.outbox_events (tenant_id, topic, aggregate_id, payload)
		VALUES (eacp.current_tenant_id(), 'action.queued', $1, '{}')`
	wantCode(t, s.f.ExecAgent(s.agent.Version, insert, id), "42501")
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent, insert, id), "42501")
	wantCode(t, s.f.Exec("otto", insert, id), "42501")
}

// ADR-014 §3: only the outbox actor publishes, once; the database stamps
// the time; nothing else about the row may change.
func TestOnlyTheOutboxActorPublishesARowOnce(t *testing.T) {
	s := newSchema(t)
	id := s.f.QueuedAction(t, s.agent.Version, "carol", "erp.purchase")
	row := s.outbox(t, id)[0].ID
	publish := `UPDATE eacp.outbox_events SET published_at = '2000-01-01' WHERE id = $1`
	wantCode(t, s.f.ExecAgent(s.agent.Version, publish, row), "42501")
	wantCode(t, s.f.ExecSystem("sweeper", publish, row), "42501")
	wantCode(t, s.f.ExecSystem(messaging.InboxComponent, publish, row), "42501")
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent,
		`UPDATE eacp.outbox_events SET payload = '{}' WHERE id = $1`, row), "42501")
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent,
		`UPDATE eacp.outbox_events SET published_at = NULL WHERE id = $1`, row), "23514")
	must(t, s.f.ExecSystem(messaging.OutboxComponent, publish, row))
	var recent bool
	must(t, storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT published_at > now() - interval '1 minute'
			FROM eacp.outbox_events WHERE id = $1`, row).Scan(&recent)
	}))
	if !recent {
		t.Fatal("published_at was taken from the caller, not stamped by the database")
	}
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent, publish, row), "55000")
}

// ADR-014 §3: pruning deletes only rows published an hour ago or never
// published for a day, and only as the outbox actor.
func TestOutboxPruningIsGuarded(t *testing.T) {
	s := newSchema(t)
	id := s.f.QueuedAction(t, s.agent.Version, "carol", "erp.purchase")
	rows := s.outbox(t, id)
	fresh, published, stale := rows[0].ID, rows[1].ID, rows[2].ID
	del := `DELETE FROM eacp.outbox_events WHERE id = $1`
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent, del, fresh), "55000")
	wantCode(t, s.f.ExecAgent(s.agent.Version, del, fresh), "42501")

	s.admin(t, `UPDATE eacp.outbox_events SET published_at = now() - interval '2 hours' WHERE id = $1`, published)
	s.admin(t, `UPDATE eacp.outbox_events SET created_at = now() - interval '25 hours' WHERE id = $1`, stale)
	wantCode(t, s.f.ExecSystem("sweeper", del, published), "42501")
	must(t, s.f.ExecSystem(messaging.OutboxComponent, del, published))
	must(t, s.f.ExecSystem(messaging.OutboxComponent, del, stale))
	if n := len(s.outbox(t, id)); n != len(rows)-2 {
		t.Fatalf("rows left = %d, want %d", n, len(rows)-2)
	}

	// The prunable hint lists exactly the rows the guard would let go.
	s.admin(t, `UPDATE eacp.outbox_events SET published_at = now() - interval '30 minutes' WHERE id = $1`, fresh)
	n, err := messaging.PruneOutbox(context.Background(), s.f.App, 100)
	if err != nil || n != 0 {
		t.Fatalf("pruned %d, err = %v; want 0 (published half an hour ago)", n, err)
	}
	s.admin(t, `UPDATE eacp.outbox_events SET published_at = now() - interval '61 minutes'`)
	n, err = messaging.PruneOutbox(context.Background(), s.f.App, 100)
	if err != nil || n != len(rows)-2 {
		t.Fatalf("pruned %d, err = %v; want %d", n, err, len(rows)-2)
	}
}

// ADR-014 §5: the inbox is a tenant table only the inbox actor writes;
// rows are immutable and leave only after an hour.
func TestInboxIsGuardedAndTenantIsolated(t *testing.T) {
	s := newSchema(t)
	msg := uuid.New()
	insert := `INSERT INTO eacp.inbox_messages (tenant_id, consumer, message_id)
		VALUES (eacp.current_tenant_id(), 'execution-worker', $1)`
	wantCode(t, s.f.ExecAgent(s.agent.Version, insert, msg), "42501")
	wantCode(t, s.f.ExecSystem(messaging.OutboxComponent, insert, msg), "42501")
	must(t, s.f.ExecSystem(messaging.InboxComponent, insert, msg))
	wantCode(t, s.f.ExecSystem(messaging.InboxComponent, insert, msg), "23505")
	wantCode(t, s.f.ExecSystem(messaging.InboxComponent, `INSERT INTO eacp.inbox_messages
		(tenant_id, consumer, message_id) VALUES (eacp.current_tenant_id(), 'Bad Name', $1)`, uuid.New()), "23514")
	wantCode(t, s.f.ExecSystem(messaging.InboxComponent,
		`UPDATE eacp.inbox_messages SET processed_at = now() WHERE message_id = $1`, msg), "42501")
	del := `DELETE FROM eacp.inbox_messages WHERE message_id = $1`
	wantCode(t, s.f.ExecSystem(messaging.InboxComponent, del, msg), "55000")

	ctx := context.Background()
	var n int
	must(t, storage.InTenantTx(ctx, s.f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.inbox_messages`).Scan(&n)
	}))
	if n != 0 {
		t.Fatalf("tenant B sees %d inbox rows", n)
	}

	s.admin(t, `UPDATE eacp.inbox_messages SET processed_at = now() - interval '2 hours'`)
	wantCode(t, s.f.ExecAgent(s.agent.Version, del, msg), "42501")
	must(t, s.f.ExecSystem(messaging.InboxComponent, del, msg))
}
