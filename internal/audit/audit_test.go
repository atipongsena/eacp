package audit_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/audit"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

func event(action string) audit.Event {
	return audit.Event{
		ActorKind: audit.ActorSystem, ActorID: uuid.Nil,
		Action: action, SubjectType: "test", SubjectID: uuid.New(),
		Reason: "unit test", Data: map[string]any{"k": "v"},
	}
}

func appendN(t *testing.T, pool *pgxpool.Pool, tenant string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		err := storage.InTenantTx(context.Background(), pool, tenant, func(tx pgx.Tx) error {
			_, err := audit.Append(context.Background(), tx, event("test.appended"))
			return err
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func verify(t *testing.T, pool *pgxpool.Pool, tenant string) (audit.Result, error) {
	t.Helper()
	var res audit.Result
	err := storage.InTenantReadTx(context.Background(), pool, tenant, func(tx pgx.Tx) error {
		var err error
		res, err = audit.Verify(context.Background(), tx)
		return err
	})
	return res, err
}

func TestAppendBuildsGaplessChainPerTenant(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 3)
	appendN(t, pool, pgtest.TenantB, 2)

	a, err := verify(t, pool, pgtest.TenantA)
	if err != nil || a.Count != 4 {
		t.Fatalf("tenant A: count=%d err=%v, want policy-pointer seed plus 3 valid events", a.Count, err)
	}
	b, err := verify(t, pool, pgtest.TenantB)
	if err != nil || b.Count != 3 {
		t.Fatalf("tenant B: count=%d err=%v, want its own seed plus 2 events", b.Count, err)
	}
}

func TestAppendReturnsDatabaseAssignedLink(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	var first, second audit.Record
	storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
		first, _ = audit.Append(context.Background(), tx, event("one"))
		second, _ = audit.Append(context.Background(), tx, event("two"))
		return nil
	})
	if first.Seq != 2 || second.Seq != 3 {
		t.Fatalf("seq = %d,%d, want 2,3 after policy-pointer seed", first.Seq, second.Seq)
	}
	if string(second.PrevHash) != string(first.Hash) {
		t.Fatal("second event does not link to the first")
	}
	var seedHash []byte
	if err := storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT hash FROM eacp.audit_events WHERE seq = 1`).Scan(&seedHash)
	}); err != nil || string(first.PrevHash) != string(seedHash) {
		t.Fatalf("first test event does not link to policy-pointer seed: %v", err)
	}
}

func TestConcurrentAppendsStayGapless(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	const workers, each = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				errs <- storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
					_, err := audit.Append(context.Background(), tx, event("concurrent"))
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := verify(t, pool, pgtest.TenantA)
	if err != nil || res.Count != 1+workers*each {
		t.Fatalf("count=%d err=%v, want seed plus %d", res.Count, err, workers*each)
	}
}

func TestRolledBackAppendLeavesNoTrace(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 1)
	boom := errors.New("business change failed")
	_ = storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
		if _, err := audit.Append(context.Background(), tx, event("rolled.back")); err != nil {
			return err
		}
		return boom
	})
	appendN(t, pool, pgtest.TenantA, 1)
	res, err := verify(t, pool, pgtest.TenantA)
	if err != nil || res.Count != 3 {
		t.Fatalf("count=%d err=%v, want seed plus 2 (rolled-back event absent, no gap)", res.Count, err)
	}
}

func TestClientCannotForgeChainFields(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 1)
	err := storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO eacp.audit_events (tenant_id, seq, payload, prev_hash, hash)
			VALUES (eacp.current_tenant_id(), 99, '{"forged":true}', '\x00', '\x00')`)
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := verify(t, pool, pgtest.TenantA)
	if err != nil || res.Count != 3 {
		t.Fatalf("count=%d err=%v: database must assign seq and hashes itself", res.Count, err)
	}
}

func requireSQLState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want a PostgreSQL error %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want one of %v", pgErr.Code, pgErr.Message, codes)
}

func TestAppRoleCannotRewriteHistory(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 2)
	for name, stmt := range map[string]string{
		"update event": `UPDATE eacp.audit_events SET payload = '{}'`,
		"delete event": `DELETE FROM eacp.audit_events`,
		"update head":  `UPDATE eacp.audit_chain_heads SET seq = 0`,
		"insert head":  `INSERT INTO eacp.audit_chain_heads (tenant_id, seq, hash) VALUES (eacp.current_tenant_id(), 0, '\x00')`,
		"truncate":     `TRUNCATE eacp.audit_events`,
	} {
		t.Run(name, func(t *testing.T) {
			err := storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), stmt)
				return err
			})
			requireSQLState(t, err, "42501") // insufficient_privilege
		})
	}
}

func TestOwnerCannotRewriteHistoryEither(t *testing.T) {
	db := pgtest.Migrated(t)
	appendN(t, pgtest.Pool(t, db.AppDSN), pgtest.TenantA, 2)
	owner := pgtest.Pool(t, db.OwnerDSN)
	for name, stmt := range map[string]string{
		"update":   `UPDATE eacp.audit_events SET payload = '{}'`,
		"delete":   `DELETE FROM eacp.audit_events`,
		"truncate": `TRUNCATE eacp.audit_events`,
	} {
		t.Run(name, func(t *testing.T) {
			err := storage.InTenantTx(context.Background(), owner, pgtest.TenantA, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), stmt)
				return err
			})
			requireSQLState(t, err, "P0001") // raised by the append-only trigger
		})
	}
}

// tamper edits the journal as the superuser with triggers disabled, i.e. an
// attacker with direct database access. Verify must detect every variant.
func tamper(t *testing.T, db pgtest.DB, stmt string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := map[string]string{
		"edited payload": `UPDATE eacp.audit_events SET payload = convert_to('{"action":"benign"}', 'UTF8') WHERE seq = 2`,
		"deleted middle": `DELETE FROM eacp.audit_events WHERE seq = 2`,
		"deleted tail":   `DELETE FROM eacp.audit_events WHERE seq = 3`,
		"deleted head":   `DELETE FROM eacp.audit_chain_heads`,
		"rewound head":   `UPDATE eacp.audit_chain_heads SET seq = 2, hash = (SELECT hash FROM eacp.audit_events WHERE seq = 2)`,
		"edited time":    `UPDATE eacp.audit_events SET recorded_at = recorded_at - interval '1 day' WHERE seq = 1`,
		"rehashed but unlinked": `UPDATE eacp.audit_events
			SET payload = convert_to('{"x":1}', 'UTF8'),
			    hash = sha256(prev_hash || uuid_send(tenant_id) || int8send(seq) || int8send(eacp.epoch_us(recorded_at)) || convert_to('{"x":1}', 'UTF8'))
			WHERE seq = 2`,
		"swapped order": `UPDATE eacp.audit_events SET seq = 100 WHERE seq = 1;
			UPDATE eacp.audit_events SET seq = 1 WHERE seq = 2;
			UPDATE eacp.audit_events SET seq = 2 WHERE seq = 100`,
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			db := pgtest.Migrated(t)
			pool := pgtest.Pool(t, db.AppDSN)
			appendN(t, pool, pgtest.TenantA, 3)
			if _, err := verify(t, pool, pgtest.TenantA); err != nil {
				t.Fatalf("clean chain failed verification: %v", err)
			}
			tamper(t, db, stmt)
			if _, err := verify(t, pool, pgtest.TenantA); !errors.Is(err, audit.ErrChainBroken) {
				t.Fatalf("Verify after %s: err = %v, want ErrChainBroken", name, err)
			}
		})
	}
}

func TestEventsAreTenantIsolated(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 2)
	var n int
	storage.InTenantTx(context.Background(), pool, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = 'test.appended'`).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("tenant B sees %d of tenant A's events", n)
	}
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	bad := map[string]audit.Event{
		"no action":     {ActorKind: audit.ActorSystem, SubjectType: "x", Reason: "r"},
		"no actor kind": {Action: "a", SubjectType: "x", Reason: "r"},
		"no reason":     {ActorKind: audit.ActorSystem, Action: "a", SubjectType: "x"},
	}
	for name, e := range bad {
		t.Run(name, func(t *testing.T) {
			err := storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
				_, err := audit.Append(context.Background(), tx, e)
				return err
			})
			if err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
}

// Verify must not report a broken chain just because appends commit while
// it runs (review finding 5).
func TestVerifyIsStableUnderConcurrentAppends(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	appendN(t, pool, pgtest.TenantA, 5)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
					_, err := audit.Append(context.Background(), tx, event("load"))
					return err
				})
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 40; i++ {
		if _, err := verify(t, pool, pgtest.TenantA); err != nil {
			t.Fatalf("verify %d under load: %v", i, err)
		}
	}
}

func TestVerifyRefusesReadCommitted(t *testing.T) {
	db := pgtest.Migrated(t)
	pool := pgtest.Pool(t, db.AppDSN)
	err := storage.InTenantTx(context.Background(), pool, pgtest.TenantA, func(tx pgx.Tx) error {
		_, err := audit.Verify(context.Background(), tx)
		return err
	})
	if !errors.Is(err, audit.ErrNeedsSnapshot) {
		t.Fatalf("err = %v, want ErrNeedsSnapshot", err)
	}
}
