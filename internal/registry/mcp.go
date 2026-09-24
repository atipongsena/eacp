package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tool is a registered or discovered tool and whether it may execute now
// (ADR-023 §6, §7). Executable means: not quarantined, and an active,
// unrevoked contract whose fingerprint matches the tool's current one.
type Tool struct {
	ID               uuid.UUID   `json:"id"`
	ConnectorID      uuid.UUID   `json:"connector_id"`
	Name             string      `json:"name"`
	RemoteName       string      `json:"remote_name,omitempty"`
	Origin           string      `json:"origin"`
	ActiveContractID *uuid.UUID  `json:"active_contract_id,omitempty"`
	ContractMatches  bool        `json:"contract_matches"`
	Executable       bool        `json:"executable"`
	Definition       *Definition `json:"definition,omitempty"`
	MissingSince     *time.Time  `json:"missing_since,omitempty"`
	QuarantinedAt    *time.Time  `json:"quarantined_at,omitempty"`
	QuarantineReason string      `json:"quarantine_reason,omitempty"`
}

// Definition is one recorded definition of a discovered tool. The digests
// and the risk are computed by PostgreSQL; the hints are self-described by
// the server and untrusted (ADR-023 §3–§5).
type Definition struct {
	ID            uuid.UUID       `json:"id"`
	Seq           int             `json:"seq"`
	ScanID        uuid.UUID       `json:"scan_id"`
	Fingerprint   string          `json:"fingerprint"`
	DisplayDigest string          `json:"display_digest"`
	Risk          string          `json:"risk"`
	Changes       []string        `json:"changes"`
	ReadOnly      bool            `json:"read_only"`
	Destructive   bool            `json:"destructive"`
	Idempotent    bool            `json:"idempotent"`
	OpenWorld     bool            `json:"open_world"`
	ObservedAt    time.Time       `json:"observed_at"`
	ObservedBy    string          `json:"observed_by"`
	Definition    json.RawMessage `json:"definition"`
	Display       json.RawMessage `json:"display"`
}

// MCPServer is an MCP connector's scan state.
type MCPServer struct {
	ConnectorID         uuid.UUID  `json:"connector_id"`
	NextScanAt          time.Time  `json:"next_scan_at"`
	RequestedAt         *time.Time `json:"requested_at,omitempty"`
	RequestedBy         *uuid.UUID `json:"requested_by,omitempty"`
	RequestReason       string     `json:"request_reason,omitempty"`
	Leased              bool       `json:"leased"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastScan            *Scan      `json:"last_scan,omitempty"`
}

// Scan is one recorded scan of an MCP server.
type Scan struct {
	ID                 uuid.UUID       `json:"id"`
	CompletedAt        time.Time       `json:"completed_at"`
	WorkerID           string          `json:"worker_id"`
	Outcome            string          `json:"outcome"`
	ErrorClass         string          `json:"error_class,omitempty"`
	ProtocolVersion    string          `json:"protocol_version,omitempty"`
	ServerInfo         json.RawMessage `json:"server_info,omitempty"`
	ToolsListed        int             `json:"tools_listed"`
	ToolsAdded         int             `json:"tools_added"`
	DefinitionsChanged int             `json:"definitions_changed"`
	MetadataChanged    int             `json:"metadata_changed"`
	ToolsMissing       int             `json:"tools_missing"`
	Rejected           json.RawMessage `json:"rejected"`
}

const toolColumns = `t.id, t.connector_id, t.name, COALESCE(t.remote_name, ''), t.origin, t.active_contract_id,
	COALESCE(ct.fingerprint = eacp.tool_fingerprint(t.tenant_id, t.id), false),
	t.quarantined_at IS NULL AND ct.revoked_at IS NULL AND COALESCE(ct.fingerprint = eacp.tool_fingerprint(t.tenant_id, t.id), false),
	t.missing_since, t.quarantined_at, CASE WHEN t.quarantined_at IS NOT NULL THEN t.quarantine_reason ELSE '' END,
	` + definitionColumns + `
	FROM eacp.tools t
	LEFT JOIN eacp.tool_contracts ct ON ct.tenant_id = t.tenant_id AND ct.id = t.active_contract_id
	LEFT JOIN eacp.tool_definitions d ON d.tenant_id = t.tenant_id AND d.id = t.definition_id`

const definitionColumns = `d.id, d.seq, d.scan_id, encode(d.fingerprint, 'hex'), encode(d.display_digest, 'hex'), d.risk,
	d.changes, d.read_only, d.destructive, d.idempotent, d.open_world, d.observed_at, d.observed_by,
	d.definition::jsonb, d.display::jsonb`

func scanTool(r pgx.Row) (Tool, error) {
	var t Tool
	var d struct {
		id                                 *uuid.UUID
		seq                                *int
		scan                               *uuid.UUID
		fp, disp, risk, by                 *string
		changes                            []string
		ro, destructive, idempotent, openW *bool
		at                                 *time.Time
		definition, display                json.RawMessage
	}
	err := r.Scan(&t.ID, &t.ConnectorID, &t.Name, &t.RemoteName, &t.Origin, &t.ActiveContractID,
		&t.ContractMatches, &t.Executable, &t.MissingSince, &t.QuarantinedAt, &t.QuarantineReason,
		&d.id, &d.seq, &d.scan, &d.fp, &d.disp, &d.risk, &d.changes, &d.ro, &d.destructive, &d.idempotent,
		&d.openW, &d.at, &d.by, &d.definition, &d.display)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, newErr(ErrNotFound, "no such tool")
	}
	if err != nil || d.id == nil {
		return t, err
	}
	t.Definition = &Definition{ID: *d.id, Seq: *d.seq, ScanID: *d.scan, Fingerprint: *d.fp, DisplayDigest: *d.disp,
		Risk: *d.risk, Changes: d.changes, ReadOnly: *d.ro, Destructive: *d.destructive, Idempotent: *d.idempotent,
		OpenWorld: *d.openW, ObservedAt: *d.at, ObservedBy: *d.by, Definition: d.definition, Display: d.display}
	return t, nil
}

// ListTools lists a connector's tools with their current definition and
// whether they may execute.
func (s *Service) ListTools(ctx context.Context, a Actor, connectorID uuid.UUID) ([]Tool, error) {
	out := []Tool{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+toolColumns+` WHERE t.connector_id = $1 ORDER BY t.name`, connectorID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTool(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// GetTool returns one tool.
func (s *Service) GetTool(ctx context.Context, a Actor, toolID uuid.UUID) (Tool, error) {
	var t Tool
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		t, err = scanTool(tx.QueryRow(ctx, `SELECT `+toolColumns+` WHERE t.id = $1`, toolID))
		return err
	})
	return t, err
}

// ToolDefinitions returns a discovered tool's definition history, newest
// first (schema tracking, ADR-023 §4).
func (s *Service) ToolDefinitions(ctx context.Context, a Actor, toolID uuid.UUID) ([]Definition, error) {
	out := []Definition{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+definitionColumns+` FROM eacp.tool_definitions d
			WHERE d.tool_id = $1 ORDER BY d.seq DESC`, toolID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Definition, error) {
			var d Definition
			err := r.Scan(&d.ID, &d.Seq, &d.ScanID, &d.Fingerprint, &d.DisplayDigest, &d.Risk, &d.Changes,
				&d.ReadOnly, &d.Destructive, &d.Idempotent, &d.OpenWorld, &d.ObservedAt, &d.ObservedBy,
				&d.Definition, &d.Display)
			return d, err
		})
		return err
	})
	if out == nil {
		out = []Definition{}
	}
	return out, err
}

// QuarantineTool blocks a tool (operator or registry_approver, one person,
// journaled). Actions already dispatched are not affected.
func (s *Service) QuarantineTool(ctx context.Context, a Actor, toolID uuid.UUID, reason string) (Tool, error) {
	return s.setQuarantine(ctx, a, toolID, `now()`, reason)
}

// ReleaseTool lifts a tool's quarantine (a registry_approver other than the
// principal who quarantined it). It never recertifies: a tool whose
// contract no longer matches stays blocked.
func (s *Service) ReleaseTool(ctx context.Context, a Actor, toolID uuid.UUID, reason string) (Tool, error) {
	return s.setQuarantine(ctx, a, toolID, `NULL`, reason)
}

func (s *Service) setQuarantine(ctx context.Context, a Actor, toolID uuid.UUID, at, reason string) (Tool, error) {
	var t Tool
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		if err := execOne(ctx, tx, `UPDATE eacp.tools SET quarantined_at = `+at+`, quarantine_reason = $2
			WHERE id = $1`, toolID, reason); err != nil {
			return err
		}
		var err error
		t, err = scanTool(tx.QueryRow(ctx, `SELECT `+toolColumns+` WHERE t.id = $1`, toolID))
		return err
	})
	return t, err
}

const serverColumns = `m.connector_id, m.next_scan_at, m.requested_at, m.requested_by, COALESCE(m.request_reason, ''),
	COALESCE(m.lease_until > now(), false), m.consecutive_failures, ` + scanColumns + `
	FROM eacp.mcp_servers m LEFT JOIN eacp.mcp_scans s ON s.tenant_id = m.tenant_id AND s.id = m.last_scan_id`

const scanColumns = `s.id, s.completed_at, s.worker_id, s.outcome, COALESCE(s.error_class, ''),
	COALESCE(s.protocol_version, ''), s.server_info, s.tools_listed, s.tools_added, s.definitions_changed,
	s.metadata_changed, s.tools_missing, s.rejected`

func scanServer(r pgx.Row) (MCPServer, error) {
	var m MCPServer
	var sc struct {
		id                                    *uuid.UUID
		at                                    *time.Time
		worker, outcome, class, version       *string
		info, rejected                        json.RawMessage
		listed, added, changed, meta, missing *int
	}
	err := r.Scan(&m.ConnectorID, &m.NextScanAt, &m.RequestedAt, &m.RequestedBy, &m.RequestReason, &m.Leased,
		&m.ConsecutiveFailures, &sc.id, &sc.at, &sc.worker, &sc.outcome, &sc.class, &sc.version, &sc.info,
		&sc.listed, &sc.added, &sc.changed, &sc.meta, &sc.missing, &sc.rejected)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, newErr(ErrNotFound, "no such MCP server")
	}
	if err != nil || sc.id == nil {
		return m, err
	}
	m.LastScan = &Scan{ID: *sc.id, CompletedAt: *sc.at, WorkerID: *sc.worker, Outcome: *sc.outcome,
		ErrorClass: *sc.class, ProtocolVersion: *sc.version, ServerInfo: sc.info, ToolsListed: *sc.listed,
		ToolsAdded: *sc.added, DefinitionsChanged: *sc.changed, MetadataChanged: *sc.meta,
		ToolsMissing: *sc.missing, Rejected: sc.rejected}
	return m, nil
}

// MCPServer returns an MCP connector's scan state and its last scan.
func (s *Service) MCPServer(ctx context.Context, a Actor, connectorID uuid.UUID) (MCPServer, error) {
	var m MCPServer
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		m, err = scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` WHERE m.connector_id = $1`, connectorID))
		return err
	})
	return m, err
}

// RequestScan asks the scanners to list an MCP server's tools now
// (operator or registry_editor, with a reason, journaled).
func (s *Service) RequestScan(ctx context.Context, a Actor, connectorID uuid.UUID, reason string) (MCPServer, error) {
	var m MCPServer
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		if err := execOne(ctx, tx, `UPDATE eacp.mcp_servers SET requested_at = now(), request_reason = $2
			WHERE connector_id = $1`, connectorID, reason); err != nil {
			return err
		}
		var err error
		m, err = scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` WHERE m.connector_id = $1`, connectorID))
		return err
	})
	return m, err
}

// MCPScans returns an MCP server's most recent scans, newest first.
func (s *Service) MCPScans(ctx context.Context, a Actor, connectorID uuid.UUID, limit int) ([]Scan, error) {
	out := []Scan{}
	limit = min(max(limit, 1), 100)
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+scanColumns+` FROM eacp.mcp_scans s
			WHERE s.connector_id = $1 ORDER BY s.completed_at DESC, s.id LIMIT $2`, connectorID, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Scan, error) {
			var sc Scan
			err := r.Scan(&sc.ID, &sc.CompletedAt, &sc.WorkerID, &sc.Outcome, &sc.ErrorClass, &sc.ProtocolVersion,
				&sc.ServerInfo, &sc.ToolsListed, &sc.ToolsAdded, &sc.DefinitionsChanged, &sc.MetadataChanged,
				&sc.ToolsMissing, &sc.Rejected)
			return sc, err
		})
		return err
	})
	if out == nil {
		out = []Scan{}
	}
	return out, err
}
