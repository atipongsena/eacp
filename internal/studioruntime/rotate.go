package studioruntime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Rotate proposes due keys every interval until ctx ends.
func (r *Runtime) Rotate(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		if n, err := r.RotateOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.WarnContext(ctx, "studio key rotation", "err", err)
		} else if n > 0 {
			r.log.InfoContext(ctx, "studio keys proposed", "count", n)
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// RotateOnce proposes a key under the current master for every approved,
// ACTIVE Studio version that is due: no approved key with more than 30 days
// left and no pending proposal (PostgreSQL decides). Only the hash leaves the
// runtime, and a registry_approver approves each key; the runtime never
// does. It returns how many it proposed.
func (r *Runtime) RotateOnce(ctx context.Context) (int, error) {
	proposed := 0
	var errs []error
	for _, tenant := range r.tenants {
		var due struct {
			Due []uuid.UUID `json:"due"`
		}
		_, err := r.api.do(ctx, r.keys[tenant], "GET", "/v1/studio/runtime/credentials?master_version="+r.master.Version(),
			nil, nil, &due)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: due keys: %w", tenant, err))
			continue
		}
		for _, version := range due.Due {
			id := uuid.New()
			_, hash := r.master.Key(tenant, id)
			status, err := r.api.do(ctx, r.keys[tenant], "POST", "/v1/studio/runtime/credentials", nil, map[string]any{
				"version_id": version, "id": id, "hash": hex.EncodeToString(hash), "master_version": r.master.Version()}, nil)
			switch {
			case err == nil:
				proposed++
			case status == 409:
				// Another replica proposed first: the version is no longer due.
			default:
				errs = append(errs, fmt.Errorf("tenant %s: propose a key for version %s: %w", tenant, version, err))
			}
		}
	}
	return proposed, errors.Join(errs...)
}
