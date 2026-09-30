package studio

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// The Agent Hub (Phase 27b, ADR-033 Rev 1.3). PostgreSQL decides who
// proposes, who approves each scope, who sees a listing and who clones it
// (migration 00029); this file calls those functions and reads the rows.

// Listing is a Hub listing as its caller sees it.
type Listing struct {
	ID                 uuid.UUID  `json:"id"`
	AgentID            uuid.UUID  `json:"agent_id"`
	Name               string     `json:"name"`
	DisplayName        string     `json:"display_name"`
	Description        string     `json:"description"`
	DepartmentID       uuid.UUID  `json:"department_id"`
	DepartmentName     string     `json:"department_name"`
	OwnerID            uuid.UUID  `json:"owner_id"`
	Scope              string     `json:"scope"`
	State              string     `json:"state"`
	PublishedVersionID uuid.UUID  `json:"published_version_id"`
	Version            int        `json:"version"`
	Tags               []string   `json:"tags"`
	Runnable           bool       `json:"runnable"`
	RunCount           int64      `json:"run_count"`
	PublishedAt        time.Time  `json:"published_at"`
	ClonedFromVersion  *uuid.UUID `json:"cloned_from_version,omitempty"`
}

// HubListing is one listing with its published definition and its tools.
type HubListing struct {
	Listing
	Definition json.RawMessage `json:"definition"`
	Tools      []Tool          `json:"tools"`
}

// ListingProposal is a proposal to list an agent's version in the Hub.
type ListingProposal struct {
	ID             uuid.UUID  `json:"id"`
	AgentID        uuid.UUID  `json:"agent_id"`
	AgentName      string     `json:"agent_name"`
	DisplayName    string     `json:"display_name"`
	DepartmentID   uuid.UUID  `json:"department_id"`
	DepartmentName string     `json:"department_name"`
	VersionID      uuid.UUID  `json:"version_id"`
	Version        int        `json:"version"`
	Scope          string     `json:"scope"`
	Tags           []string   `json:"tags"`
	Note           *string    `json:"note,omitempty"`
	ProposedBy     uuid.UUID  `json:"proposed_by"`
	ProposedAt     time.Time  `json:"proposed_at"`
	Decision       *string    `json:"decision"`
	DecidedBy      *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason *string    `json:"decision_reason,omitempty"`
	Tools          []Tool     `json:"tools,omitempty"`
}

// AgentListing is an agent's listing and its latest proposal, either absent.
type AgentListing struct {
	Listing  *Listing         `json:"listing"`
	Proposal *ListingProposal `json:"proposal"`
}

// ProposeIn proposes a version to the Hub.
type ProposeIn struct {
	VersionID uuid.UUID `json:"version_id"`
	Scope     string    `json:"scope"`
	Tags      []string  `json:"tags"`
	Note      string    `json:"note"`
}

// CloneIn names a clone and its department.
type CloneIn struct {
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	DepartmentID uuid.UUID `json:"department_id"`
}

// HubQuery narrows the Hub: a case-insensitive text in the name, display
// name or description, a tag, a department. Zero values match everything.
type HubQuery struct {
	Text       string
	Tag        string
	Department uuid.UUID
}

const listingCols = `h.id, h.agent_id, h.name, h.display_name, h.description, h.department_id, h.department_name,
	h.owner_id, h.scope, h.state, h.published_version_id, h.version, h.tags, h.runnable, h.run_count, h.published_at,
	h.cloned_from_version`

func scanListing(row pgx.Row) (Listing, error) {
	var l Listing
	err := row.Scan(&l.ID, &l.AgentID, &l.Name, &l.DisplayName, &l.Description, &l.DepartmentID, &l.DepartmentName,
		&l.OwnerID, &l.Scope, &l.State, &l.PublishedVersionID, &l.Version, &l.Tags, &l.Runnable, &l.RunCount,
		&l.PublishedAt, &l.ClonedFromVersion)
	return l, err
}

// Hub lists the listings a sees, matching q, by display name.
func (s *Service) Hub(ctx context.Context, a registry.Actor, q HubQuery) ([]Listing, error) {
	out := []Listing{}
	err := s.actorRead(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+listingCols+` FROM eacp.studio_hub_listings() h
			WHERE ($1 = '' OR strpos(lower(h.name || ' ' || h.display_name || ' ' || h.description), lower($1)) > 0)
			  AND ($2 = '' OR $2 = ANY (h.tags))
			  AND ($3::uuid IS NULL OR h.department_id = $3)
			ORDER BY lower(h.display_name), h.id`, strings.TrimSpace(q.Text), q.Tag, nullID(q.Department))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Listing, error) { return scanListing(r) })
		return err
	})
	return out, err
}

// HubListing returns one listing a sees, with its published definition and
// its tools in plain words. Any other listing is not found.
func (s *Service) HubListing(ctx context.Context, a registry.Actor, id uuid.UUID) (HubListing, error) {
	var out HubListing
	err := s.actorRead(ctx, a, func(tx pgx.Tx) error {
		var err error
		if out.Listing, err = scanListing(tx.QueryRow(ctx, `SELECT `+listingCols+`
			FROM eacp.studio_hub_listings() h WHERE h.id = $1`, id)); err != nil {
			return err
		}
		var def string
		var capability []string
		if err := tx.QueryRow(ctx, `SELECT eacp.studio_hub_definition($1), s.capability FROM eacp.studio_versions s
			WHERE s.id = $2`, id, out.PublishedVersionID).Scan(&def, &capability); err != nil {
			return err
		}
		out.Definition = json.RawMessage(def)
		out.Tools, err = toolsOf(ctx, tx, capability)
		return err
	})
	return out, err
}

const proposalSQL = `SELECT p.id, p.agent_id, g.name, g.display_name, sa.department_group_id, grp.display_name,
	p.version_id, v.version, p.scope, p.tags, p.note, p.proposed_by, p.proposed_at,
	p.decision, p.decided_by, p.decided_at, p.decision_reason, s.capability
	FROM eacp.studio_listing_proposals p
	JOIN eacp.studio_agents sa ON sa.tenant_id = p.tenant_id AND sa.id = p.agent_id
	JOIN eacp.agents g ON g.tenant_id = p.tenant_id AND g.id = p.agent_id
	JOIN eacp.groups grp ON grp.tenant_id = sa.tenant_id AND grp.id = sa.department_group_id
	JOIN eacp.agent_versions v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
	JOIN eacp.studio_versions s ON s.tenant_id = p.tenant_id AND s.id = p.version_id`

func scanProposal(row pgx.Row) (ListingProposal, []string, error) {
	var p ListingProposal
	var capability []string
	err := row.Scan(&p.ID, &p.AgentID, &p.AgentName, &p.DisplayName, &p.DepartmentID, &p.DepartmentName,
		&p.VersionID, &p.Version, &p.Scope, &p.Tags, &p.Note, &p.ProposedBy, &p.ProposedAt,
		&p.Decision, &p.DecidedBy, &p.DecidedAt, &p.DecisionReason, &capability)
	return p, capability, err
}

// AgentListing returns agent's listing and latest proposal. Unless all,
// only the owner sees them; for anyone else the agent is not found.
func (s *Service) AgentListing(ctx context.Context, a registry.Actor, agent uuid.UUID, all bool) (AgentListing, error) {
	var out AgentListing
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT g.owner_principal_id FROM eacp.studio_agents sa
			JOIN eacp.agents g ON g.tenant_id = sa.tenant_id AND g.id = sa.id WHERE sa.id = $1`, agent).Scan(&owner); err != nil {
			return err
		}
		if !all && owner != a.PrincipalID {
			return pgx.ErrNoRows
		}
		p, _, err := scanProposal(tx.QueryRow(ctx, proposalSQL+` WHERE p.agent_id = $1
			ORDER BY p.proposed_at DESC, p.id DESC LIMIT 1`, agent))
		switch {
		case err == nil:
			out.Proposal = &p
		case err != pgx.ErrNoRows:
			return err
		}
		l, err := scanListing(tx.QueryRow(ctx, `SELECT l.id, l.agent_id, g.name, g.display_name, sa.description,
				sa.department_group_id, grp.display_name, g.owner_principal_id, l.scope, l.state, l.published_version_id,
				v.version, l.tags, l.state IN ('PUBLISHED', 'DEPRECATED') AND v.state = 'ACTIVE',
				(SELECT count(r.id) FROM eacp.studio_runs r WHERE r.tenant_id = l.tenant_id AND r.agent_id = l.agent_id),
				l.published_at, sa.cloned_from_version
			FROM eacp.studio_listings l
			JOIN eacp.studio_agents sa ON sa.tenant_id = l.tenant_id AND sa.id = l.agent_id
			JOIN eacp.agents g ON g.tenant_id = l.tenant_id AND g.id = l.agent_id
			JOIN eacp.groups grp ON grp.tenant_id = sa.tenant_id AND grp.id = sa.department_group_id
			JOIN eacp.agent_versions v ON v.tenant_id = l.tenant_id AND v.id = l.published_version_id
			WHERE l.agent_id = $1`, agent))
		switch {
		case err == nil:
			out.Listing = &l
		case err != pgx.ErrNoRows:
			return err
		}
		return nil
	})
	return out, err
}

// ProposeListing proposes agent's version to the Hub as a.
func (s *Service) ProposeListing(ctx context.Context, a registry.Actor, agent uuid.UUID, in ProposeIn) (ListingProposal, error) {
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_listing_propose($1, $2, $3, $4, $5)`,
			agent, in.VersionID, in.Scope, tags, in.Note).Scan(&id)
	})
	if err != nil {
		return ListingProposal{}, err
	}
	var p ListingProposal
	err = s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		p, _, err = scanProposal(tx.QueryRow(ctx, proposalSQL+` WHERE p.id = $1`, id))
		return err
	})
	return p, err
}

// ListingRequests lists the open proposals a may decide, oldest first, each
// with its tools in plain words.
func (s *Service) ListingRequests(ctx context.Context, a registry.Actor) ([]ListingProposal, error) {
	out := []ListingProposal{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, proposalSQL+` WHERE p.decision IS NULL
			  AND p.proposed_by <> $1 AND g.owner_principal_id <> $1
			  AND eacp.studio_listing_approver($1, p.scope, sa.department_group_id)
			  AND (NOT 'template' = ANY (p.tags) OR eacp.holds_role($1, 'admin'))
			ORDER BY p.proposed_at, p.id`, a.PrincipalID)
		if err != nil {
			return err
		}
		type row struct {
			p   ListingProposal
			cap []string
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			p, c, err := scanProposal(r)
			return row{p, c}, err
		})
		if err != nil {
			return err
		}
		for _, r := range got {
			if r.p.Tools, err = toolsOf(ctx, tx, r.cap); err != nil {
				return err
			}
			out = append(out, r.p)
		}
		return nil
	})
	return out, err
}

// DecideListing approves or rejects a proposal as a; it returns the
// listing's state after an approval, or "rejected".
func (s *Service) DecideListing(ctx context.Context, a registry.Actor, proposal uuid.UUID, approve bool, reason string) (string, error) {
	var state string
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_listing_decide($1, $2, $3)`, proposal, approve, reason).Scan(&state)
	})
	return state, err
}

// CancelListing cancels a's open proposal.
func (s *Service) CancelListing(ctx context.Context, a registry.Actor, proposal uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_listing_cancel($1, $2)`, proposal, reason)
		return err
	})
}

// RetireListing deprecates or withdraws a listing as a.
func (s *Service) RetireListing(ctx context.Context, a registry.Actor, listing uuid.UUID, state, reason string) (string, error) {
	var got string
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_listing_retire($1, $2, $3)`, listing, state, reason).Scan(&got)
	})
	return got, err
}

// Clone copies a listing's published definition into a new agent of a.
func (s *Service) Clone(ctx context.Context, a registry.Actor, listing uuid.UUID, in CloneIn) (Version, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_clone($1, $2, $3, $4)`,
			listing, in.Name, in.DisplayName, nullID(in.DepartmentID)).Scan(&id)
	})
	if err != nil {
		return Version{}, err
	}
	return s.Version(ctx, a, id, true)
}

// toolsOf describes each tool of capability with its protocol and contract
// side effects in plain words.
func toolsOf(ctx context.Context, tx pgx.Tx, capability []string) ([]Tool, error) {
	rows, err := tx.Query(ctx, `SELECT c.name || '.' || t.name, c.protocol, COALESCE(k.side_effects, '{}')
		FROM unnest($1::text[]) AS ref(r)
		JOIN eacp.connectors c ON c.name = split_part(ref.r, '.', 1)
		JOIN eacp.tools t ON t.tenant_id = c.tenant_id AND t.connector_id = c.id
		                 AND t.name = substr(ref.r, length(c.name) + 2)
		LEFT JOIN eacp.tool_contracts k ON k.tenant_id = t.tenant_id AND k.id = t.active_contract_id
		ORDER BY 1`, capability)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tool, error) {
		var t Tool
		err := row.Scan(&t.Ref, &t.Protocol, &t.SideEffects)
		t.PlainWords = PlainWords(t.SideEffects)
		return t, err
	})
}

// actorRead is a read-only transaction with the caller bound as the actor,
// for reads that PostgreSQL filters by who asks (eacp.studio_hub_listings).
func (s *Service) actorRead(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	return classify(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		return fn(tx)
	}))
}
