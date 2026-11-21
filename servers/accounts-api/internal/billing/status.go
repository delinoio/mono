// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// Billing status rules (docs/protos-delino-accounts-api-contract.md, Billing
// status):
//
//   - A billing account is in good standing while its subscription is ACTIVE
//     and its balance is greater than zero. billing_accounts.nonpayment_since
//     records the start of the uninterrupted non-payment otherwise.
//   - A current link is ALLOWED in good standing. Otherwise it is BLOCKED
//     from blocked_since, the later of nonpayment_since and the link time,
//     and SUSPENDED from suspend_time, blocked_since plus 3 days.
//   - SyncStatuses recomputes the status of every current link of a billing
//     account. Every change of the balance or the subscription calls it in
//     the transaction of the change, and the worker calls it when a
//     suspension is due. A change of any status field takes the next
//     sequence of the service.

// SuspendAfter is the time from the start of a block to the suspension.
const SuspendAfter = 72 * time.Hour

// LinkStatus is the stored billing status of a link. The values never
// change.
type LinkStatus string

const (
	StatusAllowed   LinkStatus = "allowed"
	StatusBlocked   LinkStatus = "blocked"
	StatusSuspended LinkStatus = "suspended"
)

// Proto returns the protobuf value.
func (s LinkStatus) Proto() accountsv1.BillingStatus {
	switch s {
	case StatusAllowed:
		return accountsv1.BillingStatus_BILLING_STATUS_ALLOWED
	case StatusBlocked:
		return accountsv1.BillingStatus_BILLING_STATUS_BLOCKED
	case StatusSuspended:
		return accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED
	default:
		return accountsv1.BillingStatus_BILLING_STATUS_UNSPECIFIED
	}
}

// StatusReason says why a link is BLOCKED or SUSPENDED, as stored.
type StatusReason string

const (
	ReasonNone                 StatusReason = "none"
	ReasonNoActiveSubscription StatusReason = "no_active_subscription"
	ReasonNonPositiveBalance   StatusReason = "non_positive_balance"
)

// Proto returns the protobuf value.
func (r StatusReason) Proto() accountsv1.BillingStatusReason {
	switch r {
	case ReasonNone:
		return accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NONE
	case ReasonNoActiveSubscription:
		return accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NO_ACTIVE_SUBSCRIPTION
	case ReasonNonPositiveBalance:
		return accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NON_POSITIVE_BALANCE
	default:
		return accountsv1.BillingStatusReason_BILLING_STATUS_REASON_UNSPECIFIED
	}
}

// OrganizationStatus is the billing status of a current link.
type OrganizationStatus struct {
	LinkID           string
	Service          string
	OrganizationID   string
	OrganizationName string
	BillingAccountID string
	LinkTime         time.Time
	Status           LinkStatus
	Reason           StatusReason
	BlockedSince     *time.Time
	SuspendTime      *time.Time
	Sequence         int64
}

// Proto returns the protobuf message.
func (o OrganizationStatus) Proto() *accountsv1.OrganizationBillingStatus {
	service, _ := ServiceFromKey(o.Service)
	out := &accountsv1.OrganizationBillingStatus{
		Service:          service,
		OrganizationId:   o.OrganizationID,
		BillingAccountId: o.BillingAccountID,
		Status:           o.Status.Proto(),
		Reason:           o.Reason.Proto(),
		Sequence:         o.Sequence,
	}
	if o.BlockedSince != nil {
		out.BlockedSince = timestamppb.New(*o.BlockedSince)
	}
	if o.SuspendTime != nil {
		out.SuspendTime = timestamppb.New(*o.SuspendTime)
	}
	return out
}

// statusColumns are the columns that scanStatus reads, from organization_links
// as l.
const statusColumns = `l.organization_link_id::text, l.service, l.organization_id, l.organization_name,
	l.billing_account_id::text, l.link_time, COALESCE(l.billing_status, ''), COALESCE(l.status_reason, ''),
	l.blocked_since, l.suspend_time, COALESCE(l.status_sequence, 0)`

func scanStatus(row pgx.Row) (OrganizationStatus, error) {
	var o OrganizationStatus
	var status, reason string
	err := row.Scan(&o.LinkID, &o.Service, &o.OrganizationID, &o.OrganizationName, &o.BillingAccountID, &o.LinkTime,
		&status, &reason, &o.BlockedSince, &o.SuspendTime, &o.Sequence)
	o.Status, o.Reason = LinkStatus(status), StatusReason(reason)
	return o, err
}

func collectStatuses(rows pgx.Rows) ([]OrganizationStatus, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OrganizationStatus, error) { return scanStatus(row) })
}

// standing is the payment state of a billing account.
type standing struct {
	Good   bool
	Reason StatusReason
}

// evaluateStanding applies the status rules: only an ACTIVE subscription is
// active, and the balance must be greater than zero. Without an active
// subscription the reason is NO_ACTIVE_SUBSCRIPTION, also when the balance
// is not positive either.
func evaluateStanding(sub SubscriptionStatus, balance int64) standing {
	switch {
	case sub != SubscriptionActive:
		return standing{Reason: ReasonNoActiveSubscription}
	case balance <= 0:
		return standing{Reason: ReasonNonPositiveBalance}
	default:
		return standing{Good: true, Reason: ReasonNone}
	}
}

// desiredStatus returns the status of a link that was created at linkTime,
// for a billing account in st whose non-payment started at nonpaymentSince
// (unused in good standing), at now.
func desiredStatus(st standing, nonpaymentSince, linkTime, now time.Time) (LinkStatus, StatusReason, *time.Time, *time.Time) {
	if st.Good {
		return StatusAllowed, ReasonNone, nil, nil
	}
	since := nonpaymentSince
	if linkTime.After(since) {
		since = linkTime
	}
	suspend := since.Add(SuspendAfter)
	status := StatusBlocked
	if !now.Before(suspend) {
		status = StatusSuspended
	}
	return status, st.Reason, &since, &suspend
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// nextSequence returns the next status sequence of the service. The row lock
// is held until the transaction ends, so sequences commit in order. Callers
// hold their billing account lock first and take sequences in service
// order, so the lock order is always billing account, then sequences.
func nextSequence(ctx context.Context, tx pgx.Tx, service string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `INSERT INTO billing_status_sequences (service, last_sequence) VALUES ($1, 1)
		ON CONFLICT (service) DO UPDATE SET last_sequence = billing_status_sequences.last_sequence + 1
		RETURNING last_sequence`, service).Scan(&seq)
	return seq, err
}

// SyncStatuses recomputes the billing status of every current link of the
// billing account in tx. The caller holds the billing account lock (a
// SELECT ... FOR UPDATE of the billing account row in tx). A deleted billing
// account has no current links and is left unchanged.
func (s *Service) SyncStatuses(ctx context.Context, tx pgx.Tx, accountID string) error {
	return s.syncStatuses(ctx, tx, accountID, s.timestamp())
}

func (s *Service) syncStatuses(ctx context.Context, tx pgx.Tx, accountID string, now time.Time) error {
	var balance, due int64
	var deleted bool
	var nonpayment *time.Time
	err := tx.QueryRow(ctx, `SELECT a.balance_usd_micros, a.delete_time IS NOT NULL, a.nonpayment_since,
			COALESCE((SELECT SUM(remaining_usd_micros) FROM credit_grants g
				WHERE g.billing_account_id = a.billing_account_id AND g.remaining_usd_micros > 0 AND g.expire_time <= $2), 0)
		FROM billing_accounts a WHERE a.billing_account_id = $1 FOR UPDATE OF a`, accountID, now).Scan(&balance, &deleted, &nonpayment, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return errAccountMissing
	}
	if err != nil || deleted {
		return err
	}
	sub, err := LoadSubscription(ctx, tx, accountID, false)
	if err != nil {
		return err
	}
	// Credit of grants that expired but that the worker has not removed yet
	// does not count, like in GetBalance.
	st := evaluateStanding(sub.Status, balance-due)
	switch {
	case st.Good && nonpayment != nil:
		nonpayment = nil
		if _, err := tx.Exec(ctx, `UPDATE billing_accounts SET nonpayment_since = NULL WHERE billing_account_id = $1`, accountID); err != nil {
			return err
		}
	case !st.Good && nonpayment == nil:
		nonpayment = &now
		if _, err := tx.Exec(ctx, `UPDATE billing_accounts SET nonpayment_since = $2 WHERE billing_account_id = $1`, accountID, now); err != nil {
			return err
		}
	}
	since := now
	if nonpayment != nil {
		since = *nonpayment
	}
	rows, err := tx.Query(ctx, `SELECT `+statusColumns+` FROM organization_links l
		WHERE l.billing_account_id = $1 AND l.unlink_time IS NULL
		ORDER BY l.service, l.organization_id FOR UPDATE`, accountID)
	if err != nil {
		return err
	}
	links, err := collectStatuses(rows)
	if err != nil {
		return err
	}
	for _, cur := range links {
		status, reason, blocked, suspend := desiredStatus(st, since, cur.LinkTime, now)
		if cur.Status == status && cur.Reason == reason && sameTime(cur.BlockedSince, blocked) && sameTime(cur.SuspendTime, suspend) {
			continue
		}
		seq, err := nextSequence(ctx, tx, cur.Service)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE organization_links SET billing_status = $2, status_reason = $3, blocked_since = $4,
				suspend_time = $5, status_sequence = $6, status_update_time = $7
			WHERE organization_link_id = $1`, cur.LinkID, string(status), string(reason), blocked, suspend, seq, now); err != nil {
			return err
		}
		next := cur
		next.Status, next.Reason, next.BlockedSince, next.SuspendTime, next.Sequence = status, reason, blocked, suspend, seq
		if err := s.statusChanged(ctx, tx, cur, next, now); err != nil {
			return err
		}
	}
	return nil
}

// statusChanged logs a status change and queues the blocked or suspended
// email. The deduplication key holds the link and the start of the block,
// so each block of a link sends each email at most once.
func (s *Service) statusChanged(ctx context.Context, tx pgx.Tx, from, to OrganizationStatus, now time.Time) error {
	attrs := []slog.Attr{
		slog.String("billing_account_id", to.BillingAccountID),
		slog.String("service", to.Service),
		slog.String("organization_id", to.OrganizationID),
		slog.String("from", string(from.Status)),
		slog.String("to", string(to.Status)),
		slog.String("reason", string(to.Reason)),
		slog.Int64("sequence", to.Sequence),
	}
	if to.BlockedSince != nil {
		attrs = append(attrs, slog.Time("blocked_since", *to.BlockedSince), slog.Time("suspend_time", *to.SuspendTime))
	}
	msg := "billing_status_changed"
	switch {
	case to.Status == StatusBlocked && from.Status != StatusBlocked:
		msg = "billing_organization_blocked"
	case to.Status == StatusSuspended && from.Status != StatusSuspended:
		msg = "billing_organization_suspended"
	case to.Status == StatusAllowed && from.Status != StatusAllowed && from.Status != "":
		msg = "billing_organization_resumed"
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
	if from.Status != to.Status {
		s.metrics.StatusChange(string(to.Status))
	}
	if to.Status == from.Status || to.BlockedSince == nil {
		return nil
	}
	kind := NotifyOrganizationBlocked
	if to.Status == StatusSuspended {
		kind = NotifyOrganizationSuspended
	}
	key := fmt.Sprintf("%s/%s/%d", kind, to.LinkID, to.BlockedSince.UnixMicro())
	return s.enqueue(ctx, tx, to.BillingAccountID, kind, key, map[string]string{
		notifyDetailService:          to.Service,
		notifyDetailOrganizationID:   to.OrganizationID,
		notifyDetailOrganizationName: to.OrganizationName,
		notifyDetailReason:           string(to.Reason),
		notifyDetailSuspendTime:      to.SuspendTime.UTC().Format(time.RFC3339Nano),
	}, now)
}

// SyncAccount recomputes the statuses of one billing account in its own
// transaction.
func (s *Service) SyncAccount(ctx context.Context, accountID string) error {
	return s.db.InTx(ctx, func(tx pgx.Tx) error {
		acct, err := lockAccount(ctx, tx, accountID)
		if err != nil {
			return err
		}
		return s.syncStatuses(ctx, tx, acct.ID, s.timestamp())
	})
}

// SuspendFilter selects the links of SuspendDue. Empty fields select all.
type SuspendFilter struct {
	Service           string
	BillingAccountIDs []string
}

// SuspendDue records the suspension of every BLOCKED link whose suspension
// time has passed: it recomputes the statuses of their billing accounts.
// The worker calls it for every link; reads call it for the links they
// return, so a status is never reported BLOCKED after its suspension time.
func (s *Service) SuspendDue(ctx context.Context, f SuspendFilter) error {
	now := s.timestamp()
	rows, err := s.db.Pool.Query(ctx, `SELECT DISTINCT billing_account_id::text FROM organization_links
		WHERE unlink_time IS NULL AND billing_status = 'blocked' AND suspend_time <= $1
			AND ($2 = '' OR service = $2) AND (cardinality($3::uuid[]) = 0 OR billing_account_id = ANY($3::uuid[]))
		LIMIT 100`, now, f.Service, nonNilIDs(f.BillingAccountIDs))
	if err != nil {
		return err
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range accounts {
		if err := s.SyncAccount(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// CurrentStatus returns the status of the current link of the organization,
// or false when it has no link.
func CurrentStatus(ctx context.Context, q db.Querier, service, organizationID string) (OrganizationStatus, bool, error) {
	o, err := scanStatus(q.QueryRow(ctx, `SELECT `+statusColumns+` FROM organization_links l
		WHERE l.service = $1 AND l.organization_id = $2 AND l.unlink_time IS NULL`, service, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationStatus{}, false, nil
	}
	return o, err == nil, err
}

// CurrentStatuses returns the statuses of the current links of the
// organizations of the service, ordered by organization ID. Organizations
// without a link are left out. Due suspensions are recorded first.
func (s *Service) CurrentStatuses(ctx context.Context, service string, organizationIDs []string) ([]OrganizationStatus, error) {
	query := func() ([]OrganizationStatus, error) {
		rows, err := s.db.Pool.Query(ctx, `SELECT `+statusColumns+` FROM organization_links l
			WHERE l.service = $1 AND l.organization_id = ANY($2::text[]) AND l.unlink_time IS NULL
			ORDER BY l.organization_id COLLATE "C"`, service, nonNilIDs(organizationIDs))
		if err != nil {
			return nil, err
		}
		return collectStatuses(rows)
	}
	out, err := query()
	if err != nil {
		return nil, err
	}
	if due := s.dueAccounts(out); len(due) > 0 {
		if err := s.SuspendDue(ctx, SuspendFilter{Service: service, BillingAccountIDs: due}); err != nil {
			return nil, err
		}
		return query()
	}
	return out, nil
}

// dueAccounts returns the billing accounts of statuses that are BLOCKED past
// their suspension time.
func (s *Service) dueAccounts(statuses []OrganizationStatus) []string {
	now := s.timestamp()
	seen := map[string]bool{}
	var out []string
	for _, o := range statuses {
		if o.Status == StatusBlocked && o.SuspendTime != nil && !now.Before(*o.SuspendTime) && !seen[o.BillingAccountID] {
			seen[o.BillingAccountID] = true
			out = append(out, o.BillingAccountID)
		}
	}
	return out
}

// FreshStatus returns the current status of the organization after it
// recorded a due suspension.
func (s *Service) FreshStatus(ctx context.Context, service, organizationID string) (OrganizationStatus, bool, error) {
	o, ok, err := CurrentStatus(ctx, s.db.Pool, service, organizationID)
	if err != nil || !ok {
		return o, ok, err
	}
	if due := s.dueAccounts([]OrganizationStatus{o}); len(due) > 0 {
		if err := s.SyncAccount(ctx, o.BillingAccountID); err != nil {
			return OrganizationStatus{}, false, err
		}
		return CurrentStatus(ctx, s.db.Pool, service, organizationID)
	}
	return o, true, nil
}

// ListStatusChanges returns up to limit current statuses of the service with
// a sequence greater than after, in sequence order. Due suspensions of the
// service are recorded first.
func (s *Service) ListStatusChanges(ctx context.Context, service string, after int64, limit int) ([]OrganizationStatus, error) {
	if err := s.SuspendDue(ctx, SuspendFilter{Service: service}); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT `+statusColumns+` FROM organization_links l
		WHERE l.service = $1 AND l.unlink_time IS NULL AND l.status_sequence > $2
		ORDER BY l.status_sequence LIMIT $3`, service, after, limit)
	if err != nil {
		return nil, err
	}
	return collectStatuses(rows)
}

// RefreshStatusMetrics sets the number of linked organizations for each
// status.
func (s *Service) RefreshStatusMetrics(ctx context.Context) error {
	if s.metrics == nil {
		return nil
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT billing_status, count(*) FROM organization_links
		WHERE unlink_time IS NULL AND billing_status IS NOT NULL GROUP BY billing_status`)
	if err != nil {
		return err
	}
	type count struct {
		Status string
		N      int64
	}
	counts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[count])
	if err != nil {
		return err
	}
	byStatus := map[string]int64{string(StatusAllowed): 0, string(StatusBlocked): 0, string(StatusSuspended): 0}
	for _, c := range counts {
		byStatus[c.Status] = c.N
	}
	for status, n := range byStatus {
		s.metrics.LinkedOrganizations(status, n)
	}
	return nil
}

// LockAccount locks the billing account row for tx, as every change of a
// billing account does first.
func LockAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	_, err := lockAccount(ctx, tx, accountID)
	return err
}

// LinkedOrganizations returns up to limit current links of the billing
// account in link order (link time, then link ID), after the position
// (afterTime, afterID) when after is true.
func LinkedOrganizations(ctx context.Context, q db.Querier, accountID string, afterTime time.Time, afterID string, after bool, limit int) ([]OrganizationStatus, error) {
	rows, err := q.Query(ctx, `SELECT `+statusColumns+` FROM organization_links l
		WHERE l.billing_account_id = $1 AND l.unlink_time IS NULL
			AND ($2 = false OR (l.link_time, l.organization_link_id) > ($3, $4::uuid))
		ORDER BY l.link_time, l.organization_link_id LIMIT $5`, accountID, after, afterTime, afterID, limit)
	if err != nil {
		return nil, err
	}
	return collectStatuses(rows)
}
