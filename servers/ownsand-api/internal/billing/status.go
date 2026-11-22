// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// Durations of the billing status rules
// (docs/servers-ownsand-api-foundation.md#billing-status-cache).
const (
	// SuspendAfter is how long non-payment lasts before the organization is
	// suspended.
	SuspendAfter = 3 * 24 * time.Hour
	// StaleAfter is the age of the sync time of a linked organization after
	// which the RPC gates fail open.
	StaleAfter = time.Hour
	// PollInterval is the interval of the ListBillingStatusChanges poll.
	PollInterval = time.Minute
	// StatusPageSize is the page size of the poll.
	StatusPageSize = 200
)

// AccountsWebURL is the delino-accounts web app, where users manage billing.
const AccountsWebURL = "https://accounts.delino.io"

// Phases, for brevity.
const (
	PhaseAllowed   = ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED
	PhaseBlocked   = ownsandv1.BillingPhase_BILLING_PHASE_BLOCKED
	PhaseSuspended = ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED
)

// Status is the cached billing status of one organization.
type Status struct {
	OrganizationID string
	// BillingAccountID is the linked billing account; empty without a link.
	BillingAccountID string
	// Phase is the stored phase: the status that delino-accounts reported
	// for a linked organization, or the status that the control plane
	// decided for an organization without a link.
	Phase ownsandv1.BillingPhase
	// BlockedSince is the start of the non-payment; nil while ALLOWED.
	BlockedSince *time.Time
	// SyncTime is the last time delino-accounts confirmed the status of a
	// linked organization; nil without a link.
	SyncTime *time.Time
	// Sequence is the delino-accounts sequence of the applied status.
	Sequence int64
}

// Linked reports whether the organization has a billing account link.
func (s Status) Linked() bool { return s.BillingAccountID != "" }

// SuspendTime returns BlockedSince plus 3 days, or the zero time while
// ALLOWED.
func (s Status) SuspendTime() time.Time {
	if s.BlockedSince == nil {
		return time.Time{}
	}
	return s.BlockedSince.Add(SuspendAfter)
}

// Confirmed returns the phase of the confirmed status at now: the stored
// phase, or SUSPENDED once 3 days of non-payment have passed since the
// block start. The suspension worker acts only on it.
func (s Status) Confirmed(now time.Time) ownsandv1.BillingPhase {
	switch {
	case s.Phase == PhaseAllowed:
		return PhaseAllowed
	case s.Phase == PhaseSuspended:
		return PhaseSuspended
	case s.BlockedSince != nil && !now.Before(s.SuspendTime()):
		return PhaseSuspended
	}
	return PhaseBlocked
}

// Stale reports whether the status of a linked organization is more than
// StaleAfter old, so that the RPC gates fail open.
func (s Status) Stale(now time.Time) bool {
	return s.Linked() && (s.SyncTime == nil || now.Sub(*s.SyncTime) > StaleAfter)
}

// Enforced returns the phase that the RPC gates apply at now: the confirmed
// phase, or ALLOWED while the status of a linked organization is stale.
func (s Status) Enforced(now time.Time) ownsandv1.BillingPhase {
	if s.Stale(now) {
		return PhaseAllowed
	}
	return s.Confirmed(now)
}

// ReasonOf returns the billing reason that a rejected RPC carries in phase.
func ReasonOf(phase ownsandv1.BillingPhase, linked bool) ownsandv1.BillingReason {
	switch phase {
	case PhaseBlocked:
		if linked {
			return ownsandv1.BillingReason_BILLING_REASON_BLOCKED
		}
		return ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED
	case PhaseSuspended:
		return ownsandv1.BillingReason_BILLING_REASON_SUSPENDED
	}
	return ownsandv1.BillingReason_BILLING_REASON_UNSPECIFIED
}

// Proto returns the OrganizationBilling that GetBillingStatus and the link
// RPCs return at now: the enforced phase, so a stale linked organization
// reports ALLOWED.
func (s Status) Proto(now time.Time) *ownsandv1.OrganizationBilling {
	phase := s.Enforced(now)
	b := &ownsandv1.OrganizationBilling{Phase: phase, Reason: ReasonOf(phase, s.Linked()), Linked: s.Linked()}
	if phase != PhaseAllowed && s.BlockedSince != nil {
		b.BlockedSince = timestamppb.New(*s.BlockedSince)
		b.SuspendTime = timestamppb.New(s.SuspendTime())
	}
	return b
}

// PhaseName returns the lowercase name of a phase for logs and metric
// labels.
func PhaseName(p ownsandv1.BillingPhase) string {
	switch p {
	case PhaseAllowed:
		return "allowed"
	case PhaseBlocked:
		return "blocked"
	case PhaseSuspended:
		return "suspended"
	}
	return "unspecified"
}

// unlinkedStatus returns the status of an organization without a link whose
// block starts at start.
func unlinkedStatus(orgID string, start time.Time, sequence int64, now time.Time) Status {
	st := Status{OrganizationID: orgID, Phase: PhaseBlocked, BlockedSince: &start, Sequence: sequence}
	st.Phase = st.Confirmed(now)
	return st
}

const statusSelect = `SELECT organization_id::text, COALESCE(billing_account_id::text, ''), phase, blocked_since, status_sync_time, status_sequence
	FROM organization_billing`

func scanStatus(row pgx.Row) (Status, error) {
	var st Status
	var phase int16
	if err := row.Scan(&st.OrganizationID, &st.BillingAccountID, &phase, &st.BlockedSince, &st.SyncTime, &st.Sequence); err != nil {
		return Status{}, err
	}
	st.Phase = ownsandv1.BillingPhase(phase)
	return st, nil
}

// LoadStatus reads the status row of an organization; lock adds FOR
// UPDATE. It reports false when the organization has no row (it does not
// exist).
func LoadStatus(ctx context.Context, q db.Querier, orgID string, lock bool) (Status, bool, error) {
	sql := statusSelect + " WHERE organization_id = $1"
	if lock {
		sql += " FOR UPDATE"
	}
	st, err := scanStatus(q.QueryRow(ctx, sql, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	return st, true, nil
}

// InsertStatus writes the status row of a new organization: no link,
// BLOCKED with ACCOUNT_REQUIRED from now.
func InsertStatus(ctx context.Context, q db.Querier, orgID string, now time.Time) error {
	return insertStatus(ctx, q, unlinkedStatus(orgID, now, 0, now), now)
}

func insertStatus(ctx context.Context, q db.Querier, st Status, now time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, blocked_since, suspend_time,
		status_sync_time, status_sequence, update_time) VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, $8, $9)`, statusArgs(st, now)...)
	return err
}

func statusArgs(st Status, now time.Time) []any {
	var suspend *time.Time
	if st.BlockedSince != nil {
		t := st.SuspendTime()
		suspend = &t
	}
	return []any{st.OrganizationID, st.BillingAccountID, int16(st.Phase), int16(ReasonOf(st.Phase, st.Linked())), st.BlockedSince, suspend, st.SyncTime, st.Sequence, now}
}

// writeStatus replaces the status row of st.OrganizationID.
func writeStatus(ctx context.Context, q db.Querier, st Status, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE organization_billing SET billing_account_id = NULLIF($2, '')::uuid, phase = $3, reason = $4, blocked_since = $5,
		suspend_time = $6, status_sync_time = $7, status_sequence = $8, update_time = $9 WHERE organization_id = $1`, statusArgs(st, now)...)
	return err
}

// Change is a change of the cached status of one organization, for logs.
type Change struct {
	OrganizationID string
	// BillingAccountID is the link after the change (before an unlink, the
	// former link).
	BillingAccountID string
	From, To         ownsandv1.BillingPhase
	Linked           bool
	BlockedSince     *time.Time
	// Source names what changed the status: report, poll, link, unlink,
	// transfer, delete or time.
	Source string
}

// Changed reports whether the phase or the link changed.
func (c Change) Changed() bool {
	return c.From != c.To || c.Source == SourceLink || c.Source == SourceUnlink
}

// Sources of a status change.
const (
	SourceReport   = "report"
	SourcePoll     = "poll"
	SourceLink     = "link"
	SourceUnlink   = "unlink"
	SourceTransfer = "transfer"
	SourceDelete   = "delete"
	SourceTime     = "time"
)

// LogChange writes the billing_status_changed record of a change.
func LogChange(ctx context.Context, logger *slog.Logger, c Change) {
	attrs := []slog.Attr{
		slog.String("organization_id", c.OrganizationID), slog.String("source", c.Source),
		slog.String("from_phase", PhaseName(c.From)), slog.String("to_phase", PhaseName(c.To)),
		slog.Bool("linked", c.Linked), slog.String("reason", ReasonOf(c.To, c.Linked).String()),
	}
	if c.BillingAccountID != "" {
		attrs = append(attrs, slog.String("billing_account_id", c.BillingAccountID))
	}
	if c.BlockedSince != nil {
		attrs = append(attrs, slog.Time("blocked_since", *c.BlockedSince), slog.Time("suspend_time", c.BlockedSince.Add(SuspendAfter)))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "billing_status_changed", attrs...)
}

// mergeRemote returns st with the delino-accounts status r applied at now.
// While the organization stays in non-payment, the earlier block start is
// kept, so a link or a new remote block start never restarts the 3-day
// period. It reports false for a status without a known value.
func mergeRemote(st Status, r *accountsv1.OrganizationBillingStatus, now time.Time) (Status, bool) {
	next := st
	next.Sequence = r.GetSequence()
	next.SyncTime = &now
	switch r.GetStatus() {
	case accountsv1.BillingStatus_BILLING_STATUS_ALLOWED:
		next.Phase, next.BlockedSince = PhaseAllowed, nil
		return next, true
	case accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED:
		start := now
		if r.GetBlockedSince() != nil {
			start = r.GetBlockedSince().AsTime().UTC()
		}
		if st.Phase != PhaseAllowed && st.BlockedSince != nil && st.BlockedSince.Before(start) {
			start = *st.BlockedSince
		}
		next.BlockedSince = &start
		next.Phase = PhaseBlocked
		if r.GetStatus() == accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED {
			next.Phase = PhaseSuspended
		}
		next.Phase = next.Confirmed(now)
		return next, true
	}
	return st, false
}

// remoteOrganization returns the canonical organization ID of a remote
// status, or false when it is not an OwnSand organization ID.
func remoteOrganization(r *accountsv1.OrganizationBillingStatus) (string, bool) {
	if r.GetService() != ServiceID || !uuidv7.Valid(r.GetOrganizationId()) {
		return "", false
	}
	return strings.ToLower(r.GetOrganizationId()), true
}

// ApplyRemoteTx applies a delino-accounts status of a linked organization
// in tx. A status of another billing account than the known link (for
// example one that arrives after an unlink), of an unknown organization or
// with a sequence below the applied one is ignored; a status with the
// applied sequence only refreshes the sync time. It returns the change and
// whether the status was applied.
func ApplyRemoteTx(ctx context.Context, tx pgx.Tx, r *accountsv1.OrganizationBillingStatus, source string, now time.Time) (Change, bool, error) {
	orgID, ok := remoteOrganization(r)
	if !ok {
		return Change{}, false, nil
	}
	st, ok, err := LoadStatus(ctx, tx, orgID, true)
	if err != nil || !ok {
		return Change{}, false, err
	}
	if !st.Linked() || !strings.EqualFold(st.BillingAccountID, r.GetBillingAccountId()) || r.GetSequence() < st.Sequence {
		return Change{}, false, nil
	}
	if r.GetSequence() == st.Sequence {
		_, err := tx.Exec(ctx, "UPDATE organization_billing SET status_sync_time = $2 WHERE organization_id = $1 AND (status_sync_time IS NULL OR status_sync_time < $2)", orgID, now)
		return Change{}, false, err
	}
	next, ok := mergeRemote(st, r, now)
	if !ok {
		return Change{}, false, nil
	}
	if err := writeStatus(ctx, tx, next, now); err != nil {
		return Change{}, false, err
	}
	return Change{OrganizationID: orgID, BillingAccountID: next.BillingAccountID, From: st.Confirmed(now), To: next.Confirmed(now),
		Linked: true, BlockedSince: next.BlockedSince, Source: source}, true, nil
}

// LinkTx records the link of the organization to billingAccountID with the
// status r that LinkOrganization returned. The organization must not be
// linked to another billing account. A status with a lower sequence than the
// applied one (a repeated link of the same billing account) keeps the
// cached status.
func LinkTx(ctx context.Context, tx pgx.Tx, orgID, billingAccountID string, r *accountsv1.OrganizationBillingStatus, now time.Time) (Status, Change, error) {
	st, ok, err := LoadStatus(ctx, tx, orgID, true)
	if err != nil {
		return Status{}, Change{}, err
	}
	if !ok {
		return Status{}, Change{}, pgx.ErrNoRows
	}
	from := st.Confirmed(now)
	next := st
	if !st.Linked() {
		next.BillingAccountID, next.Sequence, next.SyncTime = billingAccountID, 0, nil
	}
	if r.GetSequence() >= next.Sequence {
		if merged, ok := mergeRemote(next, r, now); ok {
			next = merged
		} else {
			// The link itself is confirmed; keep the phase.
			next.SyncTime = &now
		}
	}
	if err := writeStatus(ctx, tx, next, now); err != nil {
		return Status{}, Change{}, err
	}
	return next, Change{OrganizationID: orgID, BillingAccountID: billingAccountID, From: from, To: next.Confirmed(now), Linked: true,
		BlockedSince: next.BlockedSince, Source: SourceLink}, nil
}

// UnlinkTx removes the link of the organization in tx: it becomes BLOCKED
// with ACCOUNT_REQUIRED. The block starts now unless the organization was
// already in non-payment, which keeps the earlier start. It returns the
// status after the unlink, the former billing account ID (empty when the
// organization had no link or does not exist, and nothing changed) and the
// change.
func UnlinkTx(ctx context.Context, tx pgx.Tx, orgID, source string, now time.Time) (Status, string, Change, error) {
	st, ok, err := LoadStatus(ctx, tx, orgID, true)
	if err != nil || !ok {
		return Status{}, "", Change{}, err
	}
	if !st.Linked() {
		return st, "", Change{}, nil
	}
	start := now
	if st.Phase != PhaseAllowed && st.BlockedSince != nil && st.BlockedSince.Before(now) {
		start = *st.BlockedSince
	}
	next := unlinkedStatus(orgID, start, st.Sequence, now)
	if err := writeStatus(ctx, tx, next, now); err != nil {
		return Status{}, "", Change{}, err
	}
	return next, st.BillingAccountID, Change{OrganizationID: orgID, BillingAccountID: st.BillingAccountID, From: st.Confirmed(now), To: next.Confirmed(now),
		BlockedSince: next.BlockedSince, Source: source}, nil
}

// AddPendingUnlinkTx records that UnlinkOrganization must still be sent for
// the organization, whose link to billingAccountID was removed locally. A
// link attempt row of the organization becomes a plain pending unlink whose
// first send waits for the call of the attempt.
func AddPendingUnlinkTx(ctx context.Context, tx pgx.Tx, orgID, billingAccountID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO billing_pending_unlinks (organization_id, billing_account_id, create_time, attempts, next_attempt_time)
		VALUES ($1, $2, $3, 0, $3) ON CONFLICT (organization_id) DO UPDATE SET billing_account_id = excluded.billing_account_id,
		create_time = excluded.create_time, attempts = 0,
		next_attempt_time = greatest(excluded.next_attempt_time, COALESCE(billing_pending_unlinks.link_call_deadline, excluded.next_attempt_time)),
		link_attempt_id = NULL, link_call_deadline = NULL`, orgID, billingAccountID, now)
	return err
}
