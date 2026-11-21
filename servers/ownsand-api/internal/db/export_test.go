// SPDX-License-Identifier: FSL-1.1-ALv2

package db

import (
	"context"
	"log/slog"
)

// MigrateUpTo applies the pending migrations up to maxVersion.
func (d *DB) MigrateUpTo(ctx context.Context, logger *slog.Logger, maxVersion int) error {
	return d.migrate(ctx, logger, maxVersion)
}
