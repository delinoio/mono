// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"time"
)

// AcquireLease takes or renews the named lease for holder until now+ttl. It
// succeeds when the lease is free, expired or already held by holder, and
// reports whether holder holds it afterwards. The comparison uses the
// database clock, so instance clocks do not need to agree.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO service_leases (name, holder, expires_at) VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		WHERE service_leases.holder = EXCLUDED.holder OR service_leases.expires_at < now()`,
		name, holder, ttl.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseLease gives up the lease when holder holds it.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM service_leases WHERE name = $1 AND holder = $2`, name, holder)
	return err
}
