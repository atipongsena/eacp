package storage

import (
	"context"

	"github.com/pressly/goose/v3"
)

// MigrateTo moves the schema up or down to version (tests only).
func MigrateTo(ctx context.Context, ownerDSN string, version int64) error {
	return withProvider(ctx, ownerDSN, func(p *goose.Provider) error {
		current, err := p.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		switch {
		case version == current:
			return nil
		case version > current:
			_, err = p.UpTo(ctx, version)
		default:
			_, err = p.DownTo(ctx, version)
		}
		return err
	})
}
