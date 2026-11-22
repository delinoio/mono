// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// Ledger rules (docs/servers-accounts-api-foundation.md, Payments and
// credits):
//
//   - billing_accounts.balance_usd_micros is the running balance. A negative
//     balance is debt.
//   - While the balance is zero or more it equals the sum of the remaining
//     credit of the grants; while it is negative every grant has nothing
//     remaining. Every operation below keeps this invariant.
//   - Usage consumes the grant that expires first (FIFO by expire_time, then
//     by grant ID), and what no grant covers becomes debt.
//   - A new grant first pays off debt; only the rest stays in the grant.
//   - Expired credit leaves the balance as an EXPIRY entry. Every operation
//     expires the due grants of the billing account first, so usage never
//     consumes expired credit even when the expiry job is late.

// grantBalance is the remaining credit of one grant.
type grantBalance struct {
	ID        string
	Remaining int64
}

// consume plans the consumption of amount from grants, which are in
// consumption order. It returns the amount taken from each grant and the rest
// that no grant covers (debt).
func consume(grants []grantBalance, amount int64) (takes []int64, rest int64) {
	takes = make([]int64, len(grants))
	rest = amount
	for i, g := range grants {
		if rest <= 0 {
			break
		}
		take := min(g.Remaining, rest)
		takes[i] = take
		rest -= take
	}
	return takes, rest
}

// payoff splits a new grant of amount at balance into the part that pays off
// debt and the part that stays in the grant.
func payoff(balance, amount int64) (debtPaid, kept int64) {
	if balance >= 0 {
		return 0, amount
	}
	debt := -balance
	if debt >= amount {
		return amount, 0
	}
	return debt, amount - debt
}

// accountState is a billing account row locked for update.
type accountState struct {
	ID      string
	Name    string
	Balance int64
	Deleted bool
}

// lockAccount locks the billing account row for the transaction. Every
// credit change calls it before it touches grants, the ledger, top-ups or
// settings, so the lock order is always the billing account first.
func lockAccount(ctx context.Context, tx pgx.Tx, accountID string) (*accountState, error) {
	acct := &accountState{ID: accountID}
	err := tx.QueryRow(ctx, `SELECT name, balance_usd_micros, delete_time IS NOT NULL FROM billing_accounts
		WHERE billing_account_id = $1 FOR UPDATE`, accountID).Scan(&acct.Name, &acct.Balance, &acct.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAccountMissing
	}
	if err != nil {
		return nil, err
	}
	return acct, nil
}

// entry is a ledger entry to write.
type entry struct {
	Kind                LedgerKind
	Amount              int64
	PaddleTransactionID string
	CreditGrantID       string
	Service             string
	OrganizationID      string
	Meter               string
	WindowStart         time.Time
	Quantity            int64
}

// writeEntry appends e to the ledger and stores the new balance.
func writeEntry(ctx context.Context, tx pgx.Tx, acct *accountState, e entry, at time.Time) (string, error) {
	if (e.Amount > 0 && acct.Balance > math.MaxInt64-e.Amount) || (e.Amount < 0 && acct.Balance < math.MinInt64-e.Amount) {
		return "", fmt.Errorf("balance of billing account %s overflows", acct.ID)
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	acct.Balance += e.Amount
	var windowStart *time.Time
	if !e.WindowStart.IsZero() {
		w := e.WindowStart.UTC()
		windowStart = &w
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_entry_id, billing_account_id, kind, amount_usd_micros,
			balance_after_usd_micros, create_time, paddle_transaction_id, credit_grant_id, service, organization_id, meter,
			window_start, quantity)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, '')::uuid, NULLIF($9, ''), NULLIF($10, ''),
			NULLIF($11, ''), $12, $13)`,
		id, acct.ID, string(e.Kind), e.Amount, acct.Balance, at, e.PaddleTransactionID, e.CreditGrantID,
		e.Service, e.OrganizationID, e.Meter, windowStart, e.Quantity); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_accounts SET balance_usd_micros = $2 WHERE billing_account_id = $1`,
		acct.ID, acct.Balance); err != nil {
		return "", err
	}
	return id, nil
}

// expireDue writes an EXPIRY entry for every grant of the billing account
// whose expire_time has passed and that has credit remaining.
func (s *Service) expireDue(ctx context.Context, tx pgx.Tx, acct *accountState, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT credit_grant_id::text, remaining_usd_micros FROM credit_grants
		WHERE billing_account_id = $1 AND remaining_usd_micros > 0 AND expire_time <= $2
		ORDER BY expire_time, credit_grant_id FOR UPDATE`, acct.ID, now)
	if err != nil {
		return err
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[grantBalance])
	if err != nil {
		return err
	}
	for _, g := range grants {
		if err := s.forfeitGrant(ctx, tx, acct, g, now); err != nil {
			return err
		}
		s.logger.InfoContext(ctx, "billing_credit_expired",
			slog.String("billing_account_id", acct.ID),
			slog.String("credit_grant_id", g.ID),
			slog.Int64("amount_usd_micros", g.Remaining),
			slog.Int64("balance_after_usd_micros", acct.Balance))
	}
	return nil
}

// forfeitGrant removes the remaining credit of g with an EXPIRY entry.
func (s *Service) forfeitGrant(ctx context.Context, tx pgx.Tx, acct *accountState, g grantBalance, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE credit_grants SET remaining_usd_micros = 0 WHERE credit_grant_id = $1`, g.ID); err != nil {
		return err
	}
	_, err := writeEntry(ctx, tx, acct, entry{Kind: LedgerExpiry, Amount: -g.Remaining, CreditGrantID: g.ID}, now)
	return err
}

// consumeGrants takes amount from the usable grants of the billing account,
// first from prefer (a refunded grant) when it is set, then in FIFO order.
// What no grant covers becomes debt through the balance of the ledger entry
// that the caller writes.
func consumeGrants(ctx context.Context, tx pgx.Tx, accountID string, amount int64, now time.Time, prefer string) error {
	if amount <= 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT credit_grant_id::text, remaining_usd_micros FROM credit_grants
		WHERE billing_account_id = $1 AND remaining_usd_micros > 0 AND expire_time > $2
		ORDER BY (credit_grant_id::text = $3) DESC, expire_time, credit_grant_id FOR UPDATE`, accountID, now, prefer)
	if err != nil {
		return err
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[grantBalance])
	if err != nil {
		return err
	}
	takes, _ := consume(grants, amount)
	for i, take := range takes {
		if take == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE credit_grants SET remaining_usd_micros = remaining_usd_micros - $2
			WHERE credit_grant_id = $1`, grants[i].ID, take); err != nil {
			return err
		}
	}
	return nil
}

// grantInput is a verified purchase.
type grantInput struct {
	Kind                LedgerKind
	Amount              int64
	PaddleTransactionID string
}

// addGrant creates the credit grant of a verified purchase and its ledger
// entry, then applies the pending adjustments of the purchase. A purchase
// that already has a grant adds nothing (created is false). A webhook caller
// holds the purchase lock (lockPurchase) before the lock of acct.
func (s *Service) addGrant(ctx context.Context, tx pgx.Tx, acct *accountState, in grantInput, now time.Time) (grantID string, created bool, err error) {
	if in.Amount <= 0 {
		return "", false, fmt.Errorf("credit grant amount %d is not positive", in.Amount)
	}
	err = tx.QueryRow(ctx, `SELECT credit_grant_id::text FROM credit_grants WHERE paddle_transaction_id = $1`,
		in.PaddleTransactionID).Scan(&grantID)
	if err == nil {
		return grantID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	// The balance before the inline expiry is the baseline of the low-balance
	// check: an expiry that the new grant does not make up for is a decrease.
	before := acct.Balance
	if err := s.expireDue(ctx, tx, acct, now); err != nil {
		return "", false, err
	}
	debtPaid, kept := payoff(acct.Balance, in.Amount)
	if grantID, err = newID(); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO credit_grants (credit_grant_id, billing_account_id, source, original_usd_micros,
			remaining_usd_micros, paddle_transaction_id, create_time, expire_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		grantID, acct.ID, string(in.Kind), in.Amount, kept, in.PaddleTransactionID, now, GrantExpireTime(now)); err != nil {
		return "", false, err
	}
	if _, err := writeEntry(ctx, tx, acct, entry{
		Kind: in.Kind, Amount: in.Amount, PaddleTransactionID: in.PaddleTransactionID, CreditGrantID: grantID,
	}, now); err != nil {
		return "", false, err
	}
	// afterIncrease runs first so that a grant that reaches the threshold
	// re-arms the alert before afterDecrease reads the settings. A purchase
	// never queues an automatic top-up (usage is false).
	if err := s.afterIncrease(ctx, tx, acct, now); err != nil {
		return "", false, err
	}
	if _, err := s.afterDecrease(ctx, tx, acct, before, now, false); err != nil {
		return "", false, err
	}
	s.metrics.CreditGrant(string(in.Kind))
	s.logger.InfoContext(ctx, "billing_credit_granted",
		slog.String("billing_account_id", acct.ID),
		slog.String("credit_grant_id", grantID),
		slog.String("source", string(in.Kind)),
		slog.String("paddle_transaction_id", in.PaddleTransactionID),
		slog.Int64("amount_usd_micros", in.Amount),
		slog.Int64("debt_paid_usd_micros", debtPaid),
		slog.Int64("balance_after_usd_micros", acct.Balance))
	// Approved refunds and chargebacks of this purchase that arrived before
	// its payment now reduce the new grant, as if they had arrived after it.
	if err := s.applyPendingAdjustments(ctx, tx, acct, grantID, in.PaddleTransactionID, now); err != nil {
		return "", false, err
	}
	return grantID, true, nil
}

// removeCredit removes amount for a refund or chargeback of grantID with a
// REFUND entry. It takes the credit from the refunded grant first, then from
// the other grants in FIFO order; the rest becomes debt.
func (s *Service) removeCredit(ctx context.Context, tx pgx.Tx, acct *accountState, grantID, transactionID string, amount int64, now time.Time) (string, error) {
	if err := s.expireDue(ctx, tx, acct, now); err != nil {
		return "", err
	}
	before := acct.Balance
	if err := consumeGrants(ctx, tx, acct.ID, amount, now, grantID); err != nil {
		return "", err
	}
	id, err := writeEntry(ctx, tx, acct, entry{
		Kind: LedgerRefund, Amount: -amount, PaddleTransactionID: transactionID, CreditGrantID: grantID,
	}, now)
	if err != nil {
		return "", err
	}
	if _, err := s.afterDecrease(ctx, tx, acct, before, now, false); err != nil {
		return "", err
	}
	return id, nil
}

// Deduction is one usage deduction. The usage-report API calls Deduct with
// the amount that the consuming service computed.
type Deduction struct {
	BillingAccountID string
	// AmountUSDMicros is zero or positive. It is always deducted, also below
	// zero.
	AmountUSDMicros int64
	// Service, OrganizationID and Meter are the stable identifiers of the
	// usage; WindowStart and Quantity describe the window.
	Service        string
	OrganizationID string
	Meter          string
	WindowStart    time.Time
	Quantity       int64
}

// DeductionResult is the outcome of Deduct.
type DeductionResult struct {
	LedgerEntryID         string
	BalanceAfterUSDMicros int64
	// AutoTopUpQueued reports that the deduction queued an automatic
	// top-up. The caller calls Wake after its transaction commits.
	AutoTopUpQueued bool
}

// Deduct deducts a usage amount from the billing account in tx: it consumes
// grants in FIFO order, lets the balance go negative (debt), writes a USAGE
// entry, queues an automatic top-up when the balance falls below its
// threshold, queues the low-balance email when automatic top-up is off and
// recomputes the billing statuses of the linked organizations. The caller
// owns tx; Deduct locks the billing account row in it.
func (s *Service) Deduct(ctx context.Context, tx pgx.Tx, d Deduction) (DeductionResult, error) {
	if d.AmountUSDMicros < 0 {
		return DeductionResult{}, fmt.Errorf("usage amount %d is negative", d.AmountUSDMicros)
	}
	now := s.timestamp()
	acct, err := lockAccount(ctx, tx, d.BillingAccountID)
	if err != nil {
		return DeductionResult{}, err
	}
	// before precedes the inline expiry so that a due expiry counts as a
	// decrease, also for a zero-amount deduction.
	before := acct.Balance
	if err := s.expireDue(ctx, tx, acct, now); err != nil {
		return DeductionResult{}, err
	}
	if err := consumeGrants(ctx, tx, acct.ID, d.AmountUSDMicros, now, ""); err != nil {
		return DeductionResult{}, err
	}
	id, err := writeEntry(ctx, tx, acct, entry{
		Kind: LedgerUsage, Amount: -d.AmountUSDMicros, Service: d.Service, OrganizationID: d.OrganizationID,
		Meter: d.Meter, WindowStart: d.WindowStart, Quantity: d.Quantity,
	}, now)
	if err != nil {
		return DeductionResult{}, err
	}
	queued, err := s.afterDecrease(ctx, tx, acct, before, now, d.AmountUSDMicros > 0)
	if err != nil {
		return DeductionResult{}, err
	}
	if err := s.syncStatuses(ctx, tx, acct.ID, now); err != nil {
		return DeductionResult{}, err
	}
	return DeductionResult{LedgerEntryID: id, BalanceAfterUSDMicros: acct.Balance, AutoTopUpQueued: queued}, nil
}

// afterIncrease re-arms the low-balance alert once the balance reaches the
// threshold again.
func (s *Service) afterIncrease(ctx context.Context, tx pgx.Tx, acct *accountState, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE credit_settings SET low_balance_alerted = false, update_time = $3
		WHERE billing_account_id = $1 AND low_balance_alerted AND low_balance_threshold_usd_micros <= $2`,
		acct.ID, acct.Balance, now)
	return err
}

// afterDecrease runs the checks after the balance fell from before to the
// current balance: with automatic top-up on, a usage deduction that leaves
// the balance below the threshold queues an automatic top-up; with it off,
// the first decrease below the alert threshold queues one low-balance email
// until the balance reaches the threshold again.
func (s *Service) afterDecrease(ctx context.Context, tx pgx.Tx, acct *accountState, before int64, now time.Time, usage bool) (bool, error) {
	if acct.Deleted || acct.Balance >= before {
		return false, nil
	}
	settings, err := loadSettings(ctx, tx, acct.ID)
	if err != nil {
		return false, err
	}
	if settings.AutoEnabled {
		if !usage || acct.Balance >= settings.AutoThreshold {
			return false, nil
		}
		return s.queueAutoTopUp(ctx, tx, acct, settings, now)
	}
	if settings.LowBalanceAlerted || acct.Balance >= settings.LowBalanceThreshold {
		return false, nil
	}
	if err := ensureSettings(ctx, tx, acct.ID, now); err != nil {
		return false, err
	}
	var count int64
	if err := tx.QueryRow(ctx, `UPDATE credit_settings SET low_balance_alerted = true,
			low_balance_alert_count = low_balance_alert_count + 1, update_time = $2
		WHERE billing_account_id = $1 RETURNING low_balance_alert_count`, acct.ID, now).Scan(&count); err != nil {
		return false, err
	}
	s.logger.InfoContext(ctx, "billing_low_balance",
		slog.String("billing_account_id", acct.ID),
		slog.Int64("balance_usd_micros", acct.Balance),
		slog.Int64("threshold_usd_micros", settings.LowBalanceThreshold))
	return false, s.enqueue(ctx, tx, acct.ID, NotifyLowBalance, fmt.Sprintf("low_balance/%s/%d", acct.ID, count),
		map[string]string{
			notifyDetailBalance:   fmt.Sprint(acct.Balance),
			notifyDetailThreshold: fmt.Sprint(settings.LowBalanceThreshold),
		}, now)
}

// CreditGrant is a grant for the API.
type CreditGrant struct {
	ID                  string
	Source              LedgerKind
	Original            int64
	Remaining           int64
	PaddleTransactionID string
	CreateTime          time.Time
	ExpireTime          time.Time
}

// Proto returns the protobuf message.
func (g CreditGrant) Proto() *accountsv1.CreditGrant {
	return &accountsv1.CreditGrant{
		CreditGrantId:       g.ID,
		Source:              g.Source.Proto(),
		OriginalUsdMicros:   g.Original,
		RemainingUsdMicros:  g.Remaining,
		PaddleTransactionId: g.PaddleTransactionID,
		CreateTime:          timestamppb.New(g.CreateTime),
		ExpireTime:          timestamppb.New(g.ExpireTime),
	}
}

// Balance returns the balance and the unexpired grants with remaining credit
// in consumption order. Credit of grants that expired but that the expiry
// job has not removed yet is left out of both.
func Balance(ctx context.Context, q db.Querier, accountID string, now time.Time) (int64, []CreditGrant, error) {
	var balance, due int64
	if err := q.QueryRow(ctx, `SELECT a.balance_usd_micros, COALESCE((SELECT SUM(remaining_usd_micros) FROM credit_grants g
			WHERE g.billing_account_id = a.billing_account_id AND g.remaining_usd_micros > 0 AND g.expire_time <= $2), 0)
		FROM billing_accounts a WHERE a.billing_account_id = $1`, accountID, now).Scan(&balance, &due); err != nil {
		return 0, nil, err
	}
	rows, err := q.Query(ctx, `SELECT credit_grant_id::text, source, original_usd_micros, remaining_usd_micros,
			paddle_transaction_id, create_time, expire_time
		FROM credit_grants WHERE billing_account_id = $1 AND remaining_usd_micros > 0 AND expire_time > $2
		ORDER BY expire_time, credit_grant_id`, accountID, now)
	if err != nil {
		return 0, nil, err
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[CreditGrant])
	if err != nil {
		return 0, nil, err
	}
	return balance - due, grants, nil
}

// LedgerEntry is a ledger entry for the API.
type LedgerEntry struct {
	ID                  string
	BillingAccountID    string
	Kind                LedgerKind
	Amount              int64
	BalanceAfter        int64
	CreateTime          time.Time
	PaddleTransactionID *string
	CreditGrantID       *string
	Service             *string
	OrganizationID      *string
	Meter               *string
	WindowStart         *time.Time
	Quantity            int64
}

// Proto returns the protobuf message.
func (e LedgerEntry) Proto() *accountsv1.LedgerEntry {
	out := &accountsv1.LedgerEntry{
		LedgerEntryId:         e.ID,
		BillingAccountId:      e.BillingAccountID,
		Kind:                  e.Kind.Proto(),
		AmountUsdMicros:       e.Amount,
		BalanceAfterUsdMicros: e.BalanceAfter,
		CreateTime:            timestamppb.New(e.CreateTime),
		PaddleTransactionId:   deref(e.PaddleTransactionID),
		CreditGrantId:         deref(e.CreditGrantID),
		Service:               deref(e.Service),
		OrganizationId:        deref(e.OrganizationID),
		Meter:                 deref(e.Meter),
		Quantity:              e.Quantity,
	}
	if e.WindowStart != nil {
		out.WindowStart = timestamppb.New(*e.WindowStart)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ListLedger returns up to limit entries of the billing account, newest
// first, that are older than beforeID (all when empty).
func ListLedger(ctx context.Context, q db.Querier, accountID, beforeID string, limit int) ([]LedgerEntry, error) {
	if beforeID == "" {
		beforeID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := q.Query(ctx, `SELECT ledger_entry_id::text, billing_account_id::text, kind, amount_usd_micros,
			balance_after_usd_micros, create_time, paddle_transaction_id, credit_grant_id::text, service, organization_id,
			meter, window_start, quantity
		FROM ledger_entries WHERE billing_account_id = $1 AND ledger_entry_id < $2
		ORDER BY ledger_entry_id DESC LIMIT $3`, accountID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LedgerEntry])
}

// Invoice is a completed Paddle transaction that added credit.
type Invoice struct {
	CreditGrantID       string
	PaddleTransactionID string
	Kind                LedgerKind
	Amount              int64
	CreateTime          time.Time
}

// Proto returns the protobuf message.
func (i Invoice) Proto() *accountsv1.Invoice {
	return &accountsv1.Invoice{
		PaddleTransactionId: i.PaddleTransactionID,
		Kind:                i.Kind.Proto(),
		AmountUsdMicros:     i.Amount,
		CreateTime:          timestamppb.New(i.CreateTime),
	}
}

// ListInvoices returns up to limit completed purchases of the billing
// account, newest first, after the position (afterTime, afterGrantID); a zero
// afterTime starts at the newest.
func ListInvoices(ctx context.Context, q db.Querier, accountID string, afterTime time.Time, afterGrantID string, limit int) ([]Invoice, error) {
	if afterTime.IsZero() {
		afterTime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		afterGrantID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := q.Query(ctx, `SELECT credit_grant_id::text, paddle_transaction_id, source, original_usd_micros, create_time
		FROM credit_grants WHERE billing_account_id = $1 AND (create_time, credit_grant_id) < ($2, $3::uuid)
		ORDER BY create_time DESC, credit_grant_id DESC LIMIT $4`, accountID, afterTime, afterGrantID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Invoice])
}
