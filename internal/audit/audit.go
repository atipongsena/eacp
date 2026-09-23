// Package audit appends to and verifies the per-tenant, hash-chained,
// append-only audit journal (ADR-003 §7). The database assigns every chain
// field; this package supplies payloads and recomputes the chain.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ActorKind identifies who performed an audited change.
type ActorKind string

const (
	ActorPrincipal ActorKind = "principal"
	ActorAgent     ActorKind = "agent"
	ActorSystem    ActorKind = "system"
)

// ErrChainBroken means the journal no longer matches its hash chain: an
// event was edited, removed or reordered.
var ErrChainBroken = errors.New("audit: hash chain broken")

// Event is one audited change. Append it in the same transaction as the
// change, as that transaction's last statement.
type Event struct {
	ActorKind   ActorKind
	ActorID     uuid.UUID // uuid.Nil only for ActorSystem
	Action      string    // e.g. "agent.registered"
	SubjectType string
	SubjectID   uuid.UUID
	Reason      string
	Data        any // JSON-encodable detail
}

// Record is the chain link the database assigned to an appended event.
type Record struct {
	Seq        int64
	RecordedAt time.Time
	PrevHash   []byte
	Hash       []byte
}

// Result summarises a verified chain.
type Result struct {
	Count int64
	Head  []byte
}

type payload struct {
	V       int     `json:"v"`
	Actor   actor   `json:"actor"`
	Action  string  `json:"action"`
	Subject subject `json:"subject"`
	Reason  string  `json:"reason"`
	Data    any     `json:"data,omitempty"`
}

type actor struct {
	Kind ActorKind `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

type subject struct {
	Type string    `json:"type"`
	ID   uuid.UUID `json:"id"`
}

func (e Event) validate() error {
	switch e.ActorKind {
	case ActorSystem:
	case ActorPrincipal, ActorAgent:
		if e.ActorID == uuid.Nil {
			return errors.New("audit: actor id required")
		}
	default:
		return fmt.Errorf("audit: unknown actor kind %q", e.ActorKind)
	}
	switch {
	case e.Action == "":
		return errors.New("audit: action required")
	case e.SubjectType == "":
		return errors.New("audit: subject type required")
	case e.Reason == "":
		return errors.New("audit: reason required")
	}
	return nil
}

// Append adds e to the tenant's chain within tx (whose tenant context is
// set). The event commits or rolls back with the rest of tx.
func Append(ctx context.Context, tx pgx.Tx, e Event) (Record, error) {
	if err := e.validate(); err != nil {
		return Record{}, err
	}
	body, err := json.Marshal(payload{
		V: 1, Actor: actor{e.ActorKind, e.ActorID}, Action: e.Action,
		Subject: subject{e.SubjectType, e.SubjectID}, Reason: e.Reason, Data: e.Data,
	})
	if err != nil {
		return Record{}, fmt.Errorf("audit: encode: %w", err)
	}
	var r Record
	err = tx.QueryRow(ctx, `
		INSERT INTO eacp.audit_events (tenant_id, payload)
		VALUES (eacp.current_tenant_id(), $1)
		RETURNING seq, recorded_at, prev_hash, hash`, body,
	).Scan(&r.Seq, &r.RecordedAt, &r.PrevHash, &r.Hash)
	if err != nil {
		return Record{}, fmt.Errorf("audit: append: %w", err)
	}
	return r, nil
}

// ErrNeedsSnapshot is returned when Verify runs outside a REPEATABLE READ
// (or SERIALIZABLE) transaction; use storage.InTenantReadTx.
var ErrNeedsSnapshot = errors.New("audit: Verify needs a REPEATABLE READ transaction")

// Verify recomputes the tenant's whole chain within tx and checks it ends
// exactly at the recorded chain head. Any edit, deletion (including of the
// tail or the head), reordering or rewinding yields ErrChainBroken.
//
// tx must be REPEATABLE READ so that the head and the events come from one
// snapshot; otherwise an append committing between the two reads would be
// indistinguishable from tampering.
func Verify(ctx context.Context, tx pgx.Tx) (Result, error) {
	var iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
		return Result{}, fmt.Errorf("audit: isolation level: %w", err)
	}
	if iso != "repeatable read" && iso != "serializable" {
		return Result{}, ErrNeedsSnapshot
	}

	var headSeq int64
	headHash := make([]byte, sha256.Size)
	err := tx.QueryRow(ctx, `SELECT seq, hash FROM eacp.audit_chain_heads`).Scan(&headSeq, &headHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("audit: read chain head: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT tenant_id, seq, recorded_at, payload, prev_hash, hash
		FROM eacp.audit_events ORDER BY seq`)
	if err != nil {
		return Result{}, fmt.Errorf("audit: read journal: %w", err)
	}
	defer rows.Close()

	prev := make([]byte, sha256.Size)
	var n int64
	for rows.Next() {
		var (
			tenant                 uuid.UUID
			seq                    int64
			at                     time.Time
			body, prevHash, stored []byte
		)
		if err := rows.Scan(&tenant, &seq, &at, &body, &prevHash, &stored); err != nil {
			return Result{}, fmt.Errorf("audit: scan: %w", err)
		}
		n++
		if seq != n {
			return Result{}, fmt.Errorf("%w: expected seq %d, found %d", ErrChainBroken, n, seq)
		}
		if !bytes.Equal(prevHash, prev) {
			return Result{}, fmt.Errorf("%w: seq %d does not link to its predecessor", ErrChainBroken, seq)
		}
		if want := linkHash(prev, tenant, seq, at, body); !bytes.Equal(stored, want) {
			return Result{}, fmt.Errorf("%w: seq %d hash mismatch", ErrChainBroken, seq)
		}
		prev = stored
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("audit: read journal: %w", err)
	}
	if n != headSeq || !bytes.Equal(prev, headHash) {
		return Result{}, fmt.Errorf("%w: journal ends at seq %d but the head records seq %d", ErrChainBroken, n, headSeq)
	}
	return Result{Count: n, Head: prev}, nil
}

func linkHash(prev []byte, tenant uuid.UUID, seq int64, at time.Time, body []byte) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(tenant[:])
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seq))
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(at.UnixMicro()))
	h.Write(buf[:])
	h.Write(body)
	return h.Sum(nil)
}
