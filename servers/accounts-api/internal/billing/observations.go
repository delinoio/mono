// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// TxObservations holds transition telemetry for one transaction attempt.
// Its zero value is ready to use.
type TxObservations struct {
	pending []func()
}

// Flush emits the observations in the order they were collected and clears
// them. Call it only after the transaction commits successfully.
func (o *TxObservations) Flush() {
	for _, observe := range o.pending {
		observe()
	}
	o.pending = nil
}

// InObservedTx runs a transaction and emits its observations after commit.
func (s *Service) InObservedTx(ctx context.Context, run func(pgx.Tx, *TxObservations) error) error {
	var observations TxObservations
	if err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		return run(tx, &observations)
	}); err != nil {
		return err
	}
	observations.Flush()
	return nil
}
