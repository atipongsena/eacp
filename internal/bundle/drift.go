package bundle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Drift statuses.
const (
	DriftInSync             = "in_sync"
	DriftModified           = "modified"
	DriftMissing            = "missing"
	DriftUnmanagedReference = "unmanaged_reference"
)

// DriftEntry is one address of a bundle and how the registry differs from
// the bundle's last applied change set.
type DriftEntry struct {
	Address string `json:"address"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Drift compares a bundle's last applied change set with the registry.
type Drift struct {
	Bundle      string       `json:"bundle"`
	ChangeSetID uuid.UUID    `json:"change_set_id"`
	Entries     []DriftEntry `json:"entries"`
}

// Drift reports, read-only, how the registry differs from what the bundle
// last applied. It never writes, blocks or remediates.
func (s *Service) Drift(ctx context.Context, a registry.Actor, bundle string) (Drift, error) {
	out := Drift{Bundle: bundle, Entries: []DriftEntry{}}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var desired []byte
		err := tx.QueryRow(ctx, `SELECT cs.id, cs.desired FROM eacp.change_sets cs
			JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id
			WHERE b.name = $1 AND cs.state = 'APPLIED'
			ORDER BY COALESCE(cs.approved_at, cs.submitted_at) DESC LIMIT 1`, bundle).Scan(&out.ChangeSetID, &desired)
		if errors.Is(err, pgx.ErrNoRows) {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "bundle " + bundle + " has no applied change set"}
		}
		if err != nil {
			return err
		}
		doc, err := Decode(desired)
		if err != nil {
			return err
		}
		st, err := loadState(ctx, tx, bundle)
		if err != nil {
			return err
		}
		out.Entries = driftEntries(st, diff(bundle, uuid.Nil, doc, st, false))
		return nil
	})
	return out, err
}

func driftEntries(st State, d Diff) []DriftEntry {
	by := map[string]DriftEntry{}
	for addr := range st.Managed {
		by[addr] = DriftEntry{Address: addr, Status: DriftInSync}
	}
	set := func(addr, status, detail string) {
		if e, ok := by[addr]; ok && e.Status != DriftInSync {
			return
		}
		by[addr] = DriftEntry{Address: addr, Status: status, Detail: detail}
	}
	for _, f := range d.Findings {
		switch f.Kind {
		case KindOrphan:
		case KindUnresolvedReference:
			set(f.Address, DriftMissing, f.Detail)
		case KindUnmanagedReference:
			set(f.Address, DriftUnmanagedReference, f.Detail)
		default:
			set(f.Address, DriftModified, f.Detail)
		}
	}
	for _, s := range d.Steps {
		set(s.Address, DriftModified, fmt.Sprintf("converging would %s %s", s.Op, s.Address))
	}
	// A managed version that was retired or revoked is gone, not modified.
	for addr, id := range st.Managed {
		kind, name, _ := strings.Cut(addr, ".")
		if kind != "version" {
			continue
		}
		for _, v := range st.Agents[name].Versions {
			if v.ID == id && (v.State == registry.StateRetired || v.State == registry.StateRevoked) {
				by[addr] = DriftEntry{Address: addr, Status: DriftMissing,
					Detail: fmt.Sprintf("version %d is %s", v.Number, v.State)}
			}
		}
	}
	out := make([]DriftEntry, 0, len(by))
	for _, e := range by {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b DriftEntry) int { return strings.Compare(a.Address, b.Address) })
	return out
}
