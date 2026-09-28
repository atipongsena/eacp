package approval_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/approval"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/atipongsena/eacp/internal/storage/pgtest"
)

func TestConsumeBindsDigestAndCanSucceedOnce(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, s.request); err != nil {
			t.Fatal(err)
		}
	}
	var good, wrong [32]byte
	copy(good[:], bytes.Repeat([]byte{0x22}, 32))
	copy(wrong[:], bytes.Repeat([]byte{0x33}, 32))
	consume := func(digest [32]byte) error {
		return storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
			return consumeViaService(context.Background(), tx, s, digest)
		})
	}
	if err := consume(wrong); !errors.Is(err, approval.ErrGrantUnavailable) {
		t.Fatalf("wrong digest consume = %v, want ErrGrantUnavailable", err)
	}
	if err := consume(good); err != nil {
		t.Fatalf("valid consume: %v", err)
	}
	if err := consume(good); !errors.Is(err, approval.ErrGrantUnavailable) {
		t.Fatalf("second consume = %v, want ErrGrantUnavailable", err)
	}
}

// consumeViaService is the release boundary with approval.Consume: the
// agent locks its action, consumes the grant, records revalidation evidence
// and queues the action in the caller's transaction.
func consumeViaService(ctx context.Context, tx pgx.Tx, s approvalSetup, digest [32]byte) error {
	if err := storage.SetAgent(ctx, tx, s.agent.Version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM eacp.actions WHERE id = $1 FOR UPDATE`, s.action); err != nil {
		return err
	}
	if _, err := approval.Consume(ctx, tx, s.action, digest, 1); err != nil {
		return err
	}
	var ev uuid.UUID
	if err := tx.QueryRow(ctx, registrytest.ReleaseEvidenceSQL, s.action, "escalate").Scan(&ev); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, registrytest.QueueSQL, s.action, ev)
	return err
}

func TestVoteServiceReturnsDurableState(t *testing.T) {
	s := setupApproval(t, "carol")
	svc := approval.New(s.f.App)
	for i, name := range []string{"amy", "ben"} {
		got, err := svc.Vote(context.Background(), registry.Actor{
			TenantID: s.f.Tenant, PrincipalID: s.f.P[name],
		}, s.request, approval.Approve, "reviewed")
		if err != nil {
			t.Fatal(err)
		}
		want := "PENDING"
		if i == 1 {
			want = "GRANTED"
		}
		if got.State != want {
			t.Fatalf("vote %d state = %s, want %s", i+1, got.State, want)
		}
	}
	if state := requestState(t, s); state != "GRANTED" {
		t.Fatalf("stored state = %s", state)
	}
}

func TestConcurrentConsumeSucceedsOnce(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, s.request); err != nil {
			t.Fatal(err)
		}
	}
	var digest [32]byte
	copy(digest[:], bytes.Repeat([]byte{0x22}, 32))
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- storage.InTenantTx(context.Background(), s.f.App, pgtest.TenantA, func(tx pgx.Tx) error {
				return consumeViaService(context.Background(), tx, s, digest)
			})
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	success, unavailable := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, approval.ErrGrantUnavailable):
			unavailable++
		default:
			t.Fatalf("concurrent consume: %v", err)
		}
	}
	if success != 1 || unavailable != 1 {
		t.Fatalf("consume success/unavailable = %d/%d, want 1/1", success, unavailable)
	}
}

func TestPolicyActivationWaitsForGrantConsumption(t *testing.T) {
	s := setupApproval(t, "carol")
	vote := `INSERT INTO eacp.approval_votes (tenant_id, request_id, decision, reason)
		VALUES (eacp.current_tenant_id(), $1, 'APPROVE', 'reviewed')`
	for _, name := range []string{"amy", "ben"} {
		if err := s.f.Exec(name, vote, s.request); err != nil {
			t.Fatal(err)
		}
	}
	v2 := s.f.ID(t, "bob", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, policyJSON)
	ctx := context.Background()
	tx, err := s.f.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA); err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	copy(digest[:], bytes.Repeat([]byte{0x22}, 32))
	if err := consumeViaService(ctx, tx, s, digest); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		store := governance.NewStore(s.f.App)
		done <- store.ActivatePolicy(ctx, registry.Actor{
			TenantID: s.f.Tenant, PrincipalID: s.f.P["alice"],
		}, v2, "new policy")
	}()
	select {
	case err := <-done:
		t.Fatalf("activation completed while consume transaction held the policy: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not complete after consumption committed")
	}
}

func TestRevokedPolicyHidesApprovalFromQueue(t *testing.T) {
	s := setupApproval(t, "carol")
	svc := approval.New(s.f.App)
	actor := registry.Actor{TenantID: s.f.Tenant, PrincipalID: s.f.P["amy"]}
	before, err := svc.ListEligible(context.Background(), actor)
	if err != nil || len(before) != 1 || before[0].ID != s.request {
		t.Fatalf("queue before revocation = %+v, err = %v", before, err)
	}
	if err := s.f.Exec("bob", `UPDATE eacp.policy_bundles
		SET revoked_at = now(), revoke_reason = 'unsafe' WHERE id = $1`, s.policy); err != nil {
		t.Fatal(err)
	}
	after, err := svc.ListEligible(context.Background(), actor)
	if err != nil || len(after) != 0 {
		t.Fatalf("queue after revocation = %+v, err = %v", after, err)
	}
	if _, err := svc.GetEligible(context.Background(), actor, s.request); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("detail after revocation = %v, want ErrNotFound", err)
	}
}
