package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/storage"
)

// ScanCandidate is an MCP server due for a scan (ADR-023 §2).
type ScanCandidate struct {
	TenantID    uuid.UUID
	ConnectorID uuid.UUID
	Generation  int64 // the lease generation the hint saw
}

// ScanLease is a claimed scan: the scanner holds generation Generation of
// the server's scan lease.
type ScanLease struct {
	ScanCandidate
	// Protocol is the connector's protocol (mcp or a2a): it picks the
	// discoverer.
	Endpoint, SecretRef, Protocol string
}

// ScansDue returns MCP servers due for a scan whose secret the worker holds
// (bindings), across tenants (the SECURITY DEFINER hint eacp.mcp_scans_due).
func (s *Store) ScansDue(ctx context.Context, bindings []Binding, limit int) ([]ScanCandidate, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(bindings)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT tenant_id, connector_id, lease_generation FROM eacp.mcp_scans_due($1::jsonb, $2)`,
		string(b), limit)
	if err != nil {
		return nil, fmt.Errorf("worker: scans due: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ScanCandidate, error) {
		var c ScanCandidate
		err := r.Scan(&c.TenantID, &c.ConnectorID, &c.Generation)
		return c, err
	})
}

// ClaimScan takes the next generation of c's scan lease for lease. It
// reports false when another scanner took the server first.
func (s *Store) ClaimScan(ctx context.Context, c ScanCandidate, lease time.Duration) (ScanLease, bool, error) {
	l := ScanLease{ScanCandidate: c}
	l.Generation = c.Generation + 1
	ok := false
	err := storage.InTenantTx(ctx, s.pool, c.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetScanner(ctx, tx, s.id, l.Generation); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE eacp.mcp_servers m
			SET lease_worker = $2, lease_until = now() + make_interval(secs => $3), lease_generation = $4
			FROM eacp.connectors c
			WHERE m.connector_id = $1 AND m.lease_generation = $5 AND c.tenant_id = m.tenant_id AND c.id = m.connector_id
			RETURNING c.endpoint, c.secret_ref, c.protocol`,
			c.ConnectorID, s.id, lease.Seconds(), l.Generation, c.Generation).Scan(&l.Endpoint, &l.SecretRef, &l.Protocol)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		ok = err == nil
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55000" {
		return ScanLease{}, false, nil // the lease is live: someone else scans
	}
	if err != nil || !ok {
		return ScanLease{}, false, err
	}
	return l, true, nil
}

var errorClass = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// RecordScan records the result of the scan held under l and releases the
// lease: the discovery d, or the failure derr. PostgreSQL refuses it unless
// this scanner still holds generation l.Generation of an unexpired lease.
// The next scan is due after interval (sooner after a failure).
func (s *Store) RecordScan(ctx context.Context, l ScanLease, d Discovery, derr error, interval time.Duration) error {
	result := map[string]any{"interval_s": int(interval.Seconds())}
	if derr != nil {
		class := DiscoveryClass(derr)
		if !errorClass.MatchString(class) {
			class = "discovery_error"
		}
		result["outcome"], result["error_class"] = "failed", class
	} else {
		type tool struct {
			RemoteName string `json:"remote_name"`
			Definition string `json:"definition"`
			Display    string `json:"display"`
		}
		tools := make([]tool, 0, len(d.Tools))
		for _, t := range d.Tools {
			tools = append(tools, tool{t.RemoteName, t.Definition, t.Display})
		}
		rejected := d.Rejected
		if rejected == nil {
			rejected = []RejectedTool{}
		}
		result["outcome"], result["protocol_version"], result["tools"], result["rejected"] =
			"ok", d.ProtocolVersion, tools, rejected
		if len(d.ServerInfo) > 0 && json.Valid(d.ServerInfo) {
			result["server_info"] = d.ServerInfo
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return storage.InTenantTx(ctx, s.pool, l.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetScanner(ctx, tx, s.id, l.Generation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.mcp_record_scan($1, $2::jsonb)`, l.ConnectorID, string(b))
		return err
	})
}
