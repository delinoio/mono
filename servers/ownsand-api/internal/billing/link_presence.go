// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
)

const (
	linkPresenceBatchSize = 32
	linkPresenceTimeout   = 5 * time.Second
	firstOrganizationID   = "00000000-0000-0000-0000-000000000000"
)

// CheckLinkPresence checks one page of cached links. The next pass starts
// after the last checked ID, including when an individual check failed.
func (s *Statuses) CheckLinkPresence(ctx context.Context) error {
	if s.client == nil {
		return ErrReportingDisabled
	}
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	if !s.held() {
		return nil
	}
	if s.whileHeld != nil {
		var period *lease.Period
		if !s.whileHeld(func(p *lease.Period) { period = p }) {
			return nil
		}
		periodCtx, cancel := period.ContextUntilLoss(ctx)
		defer cancel()
		ctx = periodCtx
	}

	listCtx, cancel := context.WithTimeout(ctx, linkPresenceTimeout)
	query := `SELECT organization_id::text FROM organization_billing
		WHERE billing_account_id IS NOT NULL AND organization_id > $1 ORDER BY organization_id LIMIT $2`
	after := s.presenceAfter
	if after == "" {
		query = `SELECT organization_id::text FROM organization_billing
			WHERE billing_account_id IS NOT NULL AND organization_id >= $1 ORDER BY organization_id LIMIT $2`
		after = firstOrganizationID
	}
	rows, err := s.db.Pool.Query(listCtx, query, after, linkPresenceBatchSize)
	if err != nil {
		cancel()
		return err
	}
	orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	cancel()
	if err != nil {
		return err
	}
	for _, orgID := range orgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.held() {
			return nil
		}
		if err := s.checkOrganizationLink(ctx, orgID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.WarnContext(ctx, "billing_link_presence_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		}
		s.presenceAfter = orgID
	}
	if len(orgs) < linkPresenceBatchSize {
		s.presenceAfter = ""
	}
	return nil
}

func (s *Statuses) checkOrganizationLink(ctx context.Context, orgID string) error {
	bounded, cancel := context.WithTimeout(ctx, linkPresenceTimeout)
	defer cancel()
	var change Change
	err := s.db.InTx(bounded, func(tx pgx.Tx) error {
		var locked int
		err := tx.QueryRow(bounded, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE", orgID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		st, ok, err := LoadStatus(bounded, tx, orgID, false)
		if err != nil || !ok || !st.Linked() || !s.held() {
			return err
		}
		_, err = s.client.GetOrganizationBilling(bounded, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
			Service: ServiceID, OrganizationId: orgID,
		}))
		if connect.CodeOf(err) != connect.CodeNotFound {
			return err
		}
		_, _, change, err = UnlinkTx(bounded, tx, orgID, SourceUnlink, s.now())
		return err
	})
	if err == nil {
		s.Committed(ctx, change)
	}
	return err
}

func (s *Statuses) runLinkPresence(ctx context.Context) {
	tick := time.NewTicker(s.timing.Tick)
	defer tick.Stop()
	for {
		if err := s.CheckLinkPresence(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "billing_link_presence_scan_failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
