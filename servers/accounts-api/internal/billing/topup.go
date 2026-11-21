// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// Top-up timing.
const (
	// TopUpIdempotencyWindow is how long a manual top-up key returns the
	// first result.
	TopUpIdempotencyWindow = 24 * time.Hour
	// TopUpTimeout is how long a top-up may wait for its verified payment
	// webhook. After it, the top-up fails as timed out; a webhook that
	// arrives later still adds the credit.
	TopUpTimeout = time.Hour
	// paddleCallTimeout bounds one Paddle call of a top-up. The call runs
	// without the caller's cancellation, so a client that disconnects does
	// not leave the result unknown.
	paddleCallTimeout = 20 * time.Second
)

// topUp is a top-up row.
type topUp struct {
	ID                   string
	BillingAccountID     string
	Kind                 TopUpKind
	Amount               int64
	Status               TopUpStatus
	PaddleSubscriptionID string
	CreateTime           time.Time
}

const topUpColumns = `top_up_id::text, billing_account_id::text, kind, amount_usd_micros, status,
	COALESCE(paddle_subscription_id, ''), create_time`

func scanTopUp(row pgx.Row) (topUp, error) {
	var t topUp
	err := row.Scan(&t.ID, &t.BillingAccountID, &t.Kind, &t.Amount, &t.Status, &t.PaddleSubscriptionID, &t.CreateTime)
	return t, err
}

// open reports whether the top-up still waits for its result.
func (t topUp) open() bool {
	switch t.Status {
	case TopUpPending, TopUpCharging, TopUpCharged, TopUpUnknown:
		return true
	default:
		return false
	}
}

// ValidateTopUpAmount applies the manual top-up limits: $25 to $10,000 in
// whole cents.
func ValidateTopUpAmount(amount int64) error {
	if amount < MinTopUpUSDMicros || amount > MaxTopUpUSDMicros || !WholeCents(amount) {
		return invalidArgument("amount_usd_micros must be 25 to 10,000 USD in whole cents")
	}
	return nil
}

// queueAutoTopUp queues an automatic top-up after a usage deduction left the
// balance below the threshold, unless one is in flight, the subscription
// cannot be charged or the monthly cap does not allow it.
func (s *Service) queueAutoTopUp(ctx context.Context, tx pgx.Tx, acct *accountState, st Settings, now time.Time) (bool, error) {
	skip := func(reason string) (bool, error) {
		s.logger.InfoContext(ctx, "billing_auto_top_up_skipped",
			slog.String("billing_account_id", acct.ID),
			slog.String("reason", reason),
			slog.Int64("balance_usd_micros", acct.Balance))
		return false, nil
	}
	sub, err := LoadSubscription(ctx, tx, acct.ID, false)
	if err != nil {
		return false, err
	}
	if sub.PaddleSubscriptionID == "" || !sub.Manageable() {
		return skip("no_subscription")
	}
	var inFlight bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM top_ups WHERE billing_account_id = $1 AND kind = 'auto'
		AND status IN ('pending', 'charging', 'charged', 'unknown'))`, acct.ID).Scan(&inFlight); err != nil {
		return false, err
	}
	if inFlight {
		return skip("in_flight")
	}
	mtd, err := MonthToDate(ctx, tx, acct.ID, now)
	if err != nil {
		return false, err
	}
	if !capAllows(mtd, st.AutoAmount, st.AutoCap) {
		return skip("monthly_cap")
	}
	id, err := newID()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros, status,
			paddle_subscription_id, create_time, update_time)
		VALUES ($1, $2, 'auto', $3, 'pending', $4, $5, $5)`, id, acct.ID, st.AutoAmount, sub.PaddleSubscriptionID, now); err != nil {
		return false, err
	}
	s.logger.InfoContext(ctx, "billing_auto_top_up_queued",
		slog.String("billing_account_id", acct.ID),
		slog.String("top_up_id", id),
		slog.Int64("amount_usd_micros", st.AutoAmount),
		slog.Int64("balance_usd_micros", acct.Balance),
		slog.Int64("month_to_date_usd_micros", mtd))
	return true, nil
}

// CreateTopUp runs a manual top-up. authorize runs first in the transaction
// that records the top-up; it checks the caller's role and locks the billing
// account. created runs in the same transaction when a new top-up is
// recorded (not for a repeated key). The Paddle charge runs after that
// transaction commits, so a concurrent retry with the same key finds the
// recorded top-up and charges nothing again.
func (s *Service) CreateTopUp(ctx context.Context, accountID, actorUserID, key string, amount int64, authorize func(pgx.Tx) error, created func(pgx.Tx, string) error) error {
	if err := ValidateTopUpAmount(amount); err != nil {
		return err
	}
	now := s.timestamp()
	var t topUp
	var existing bool
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := authorize(tx); err != nil {
			return err
		}
		if _, err := lockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `SELECT `+topUpColumns+` FROM top_ups
			WHERE billing_account_id = $1 AND idempotency_key = $2 AND create_time > $3
			ORDER BY create_time DESC LIMIT 1`, accountID, key, now.Add(-TopUpIdempotencyWindow))
		var err error
		t, err = scanTopUp(row)
		if err == nil {
			existing = true
			if t.Amount != amount {
				return invalidArgument("idempotency_key was used with another amount")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		sub, err := LoadSubscription(ctx, tx, accountID, false)
		if err != nil {
			return err
		}
		if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID == "" {
			return failedPrecondition("the subscription is not active")
		}
		id, err := newID()
		if err != nil {
			return err
		}
		t = topUp{ID: id, BillingAccountID: accountID, Kind: TopUpManual, Amount: amount, Status: TopUpCharging,
			PaddleSubscriptionID: sub.PaddleSubscriptionID, CreateTime: now}
		if _, err := tx.Exec(ctx, `INSERT INTO top_ups (top_up_id, billing_account_id, kind, idempotency_key, actor_user_id,
				amount_usd_micros, status, paddle_subscription_id, create_time, update_time)
			VALUES ($1, $2, 'manual', $3, $4, $5, 'charging', $6, $7, $7)`,
			id, accountID, key, actorUserID, amount, sub.PaddleSubscriptionID, now); err != nil {
			return err
		}
		if created != nil {
			return created(tx, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if existing {
		s.logger.InfoContext(ctx, "billing_top_up_repeated",
			slog.String("billing_account_id", accountID),
			slog.String("top_up_id", t.ID),
			slog.String("status", string(t.Status)))
		switch t.Status {
		case TopUpCharged, TopUpCompleted:
			return nil
		case TopUpFailed:
			return failedPrecondition("the payment provider rejected the top-up")
		default:
			return unavailable("the result of the first request with this idempotency_key is not known yet; try again later")
		}
	}
	switch outcome, err := s.charge(ctx, t); {
	case err != nil:
		return err
	case outcome == paddle.OutcomeOK:
		return nil
	case outcome == paddle.OutcomeRejected, outcome == paddle.OutcomeNotFound:
		return failedPrecondition("the payment provider rejected the top-up")
	default:
		return unavailable("the payment provider is unavailable; try again later")
	}
}

// failureReason maps a rejected charge to its reason.
func failureReason(outcome paddle.Outcome, code string) FailureReason {
	switch {
	case outcome == paddle.OutcomeNotFound:
		return FailureNoSubscription
	case code == paddle.CodePaymentDeclined:
		return FailureDeclined
	case code == paddle.CodeUpdateWhenPastDue:
		return FailurePastDue
	case code == paddle.CodeNextBilledAtTooSoon:
		return FailureTooSoon
	default:
		return FailureRejected
	}
}

// outcomeLabel is the metric label of a Paddle outcome.
func outcomeLabel(o paddle.Outcome) string {
	switch o {
	case paddle.OutcomeOK:
		return "accepted"
	case paddle.OutcomeRejected:
		return "rejected"
	case paddle.OutcomeNotFound:
		return "not_found"
	case paddle.OutcomeNotApplied:
		return "not_applied"
	default:
		return "unknown"
	}
}

// charge calls Paddle for a top-up in status charging and records the
// outcome: accepted (charged; the credit follows with the webhook),
// rejected (failed), not applied (a manual top-up is removed so the key can
// be retried; an automatic one fails without counting) or unknown (the
// webhook or the timeout decides).
func (s *Service) charge(ctx context.Context, t topUp) (paddle.Outcome, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), paddleCallTimeout)
	defer cancel()
	cents, ok := MicrosToCents(t.Amount)
	if !ok {
		return paddle.OutcomeNotApplied, fmt.Errorf("top-up %s amount is not whole cents", t.ID)
	}
	start := time.Now()
	_, callErr := s.paddle.CreateOneTimeCharge(ctx, t.PaddleSubscriptionID, paddle.Charge{
		ProductID:   s.productID,
		AmountCents: cents,
		Name:        "Delino credit",
		Description: "Delino prepaid credit top-up",
		CustomData: paddle.CustomData{
			paddle.CustomDataTopUpID:          t.ID,
			paddle.CustomDataBillingAccountID: t.BillingAccountID,
		},
	})
	outcome := paddle.Classify(callErr)
	code := paddle.ErrorCode(callErr)
	s.metrics.TopUpAttempt(string(t.Kind), outcomeLabel(outcome))
	attrs := []slog.Attr{
		slog.String("billing_account_id", t.BillingAccountID),
		slog.String("top_up_id", t.ID),
		slog.String("kind", string(t.Kind)),
		slog.Int64("amount_usd_micros", t.Amount),
		slog.String("paddle_subscription_id", t.PaddleSubscriptionID),
		slog.String("outcome", outcomeLabel(outcome)),
		slog.String("paddle_error_code", code),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if callErr != nil {
		attrs = append(attrs, slog.Any("error", callErr))
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "billing_top_up_charge", attrs...)

	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		now := s.timestamp()
		acct, err := lockAccount(ctx, tx, t.BillingAccountID)
		if err != nil {
			return err
		}
		switch outcome {
		case paddle.OutcomeOK:
			_, err = tx.Exec(ctx, `UPDATE top_ups SET status = 'charged', update_time = $2
				WHERE top_up_id = $1 AND status = 'charging'`, t.ID, now)
			return err
		case paddle.OutcomeUnknown:
			_, err = tx.Exec(ctx, `UPDATE top_ups SET status = 'unknown', update_time = $2
				WHERE top_up_id = $1 AND status = 'charging'`, t.ID, now)
			return err
		case paddle.OutcomeNotApplied:
			if t.Kind == TopUpManual {
				_, err = tx.Exec(ctx, `DELETE FROM top_ups WHERE top_up_id = $1 AND status = 'charging'`, t.ID)
				return err
			}
			return s.failTopUp(ctx, tx, acct, t.ID, FailureNotApplied, now)
		default:
			return s.failTopUp(ctx, tx, acct, t.ID, failureReason(outcome, code), now)
		}
	})
	return outcome, err
}

// failTopUp marks an open top-up failed in tx, which holds the billing
// account lock. A top-up that already finished stays unchanged, so the
// synchronous result of the charge and a payment webhook never count one
// failure twice. A failed automatic top-up emails the owners and admins,
// counts toward the shutoff unless nothing was charged, and turns automatic
// top-up off after the second failure in a row. A manual top-up that fails
// after Paddle accepted it (a failed payment webhook or a timeout) also
// emails them; a rejection in the response reaches the caller instead.
func (s *Service) failTopUp(ctx context.Context, tx pgx.Tx, acct *accountState, topUpID string, reason FailureReason, now time.Time) error {
	var prev TopUpStatus
	row := tx.QueryRow(ctx, `SELECT `+topUpColumns+` FROM top_ups WHERE top_up_id = $1 FOR UPDATE`, topUpID)
	t, err := scanTopUp(row)
	if err != nil {
		return err
	}
	if !t.open() {
		return nil
	}
	prev = t.Status
	if _, err := tx.Exec(ctx, `UPDATE top_ups SET status = 'failed', failure_reason = $2, update_time = $3
		WHERE top_up_id = $1`, topUpID, string(reason), now); err != nil {
		return err
	}
	result := "failed"
	if reason == FailureTimedOut {
		result = "timed_out"
	}
	s.metrics.TopUpResult(string(t.Kind), result)
	s.logger.WarnContext(ctx, "billing_top_up_failed",
		slog.String("billing_account_id", acct.ID),
		slog.String("top_up_id", t.ID),
		slog.String("kind", string(t.Kind)),
		slog.String("reason", string(reason)),
		slog.String("previous_status", string(prev)),
		slog.Int64("amount_usd_micros", t.Amount))

	asyncFailure := prev == TopUpCharged || prev == TopUpUnknown || reason == FailurePaymentFailed || reason == FailureTimedOut
	if t.Kind == TopUpManual {
		if !asyncFailure {
			return nil
		}
		return s.enqueue(ctx, tx, acct.ID, NotifyPaymentFailed, "payment_failed/top_up/"+t.ID, map[string]string{
			notifyDetailPaymentScope: notifyScopeTopUp,
			notifyDetailTopUpKind:    string(t.Kind),
			notifyDetailAmount:       fmt.Sprint(t.Amount),
			notifyDetailReason:       string(reason),
		}, now)
	}
	if !reason.CountsAsFailure() {
		return nil
	}
	if err := ensureSettings(ctx, tx, acct.ID, now); err != nil {
		return err
	}
	st, err := loadSettings(ctx, tx, acct.ID)
	if err != nil {
		return err
	}
	count, turnOff := nextFailureState(st.ConsecutiveFailures, st.AutoEnabled)
	if _, err := tx.Exec(ctx, `UPDATE credit_settings SET auto_top_up_consecutive_failures = $2,
			auto_top_up_enabled = auto_top_up_enabled AND NOT $3, update_time = $4
		WHERE billing_account_id = $1`, acct.ID, count, turnOff, now); err != nil {
		return err
	}
	if err := s.enqueue(ctx, tx, acct.ID, NotifyPaymentFailed, "payment_failed/top_up/"+t.ID, map[string]string{
		notifyDetailPaymentScope: notifyScopeTopUp,
		notifyDetailTopUpKind:    string(t.Kind),
		notifyDetailAmount:       fmt.Sprint(t.Amount),
		notifyDetailReason:       string(reason),
	}, now); err != nil {
		return err
	}
	if !turnOff {
		return nil
	}
	s.metrics.AutoTopUpDisabled()
	s.logger.WarnContext(ctx, "billing_auto_top_up_disabled",
		slog.String("billing_account_id", acct.ID),
		slog.String("top_up_id", t.ID),
		slog.Int("consecutive_failures", int(count)))
	return s.enqueue(ctx, tx, acct.ID, NotifyAutoTopUpDisabled, "auto_top_up_disabled/"+t.ID, map[string]string{
		notifyDetailAmount: fmt.Sprint(t.Amount),
	}, now)
}

// completeTopUp marks a top-up completed after its verified payment webhook
// added the credit. A successful automatic top-up resets the failure count,
// also when it had failed by timeout before the webhook arrived.
func (s *Service) completeTopUp(ctx context.Context, tx pgx.Tx, t topUp, transactionID string, now time.Time) error {
	if t.Status == TopUpCompleted {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE top_ups SET status = 'completed', failure_reason = NULL, paddle_transaction_id = $2, update_time = $3
		WHERE top_up_id = $1`, t.ID, transactionID, now); err != nil {
		return err
	}
	if t.Kind == TopUpAuto {
		if _, err := tx.Exec(ctx, `UPDATE credit_settings SET auto_top_up_consecutive_failures = 0, update_time = $2
			WHERE billing_account_id = $1`, t.BillingAccountID, now); err != nil {
			return err
		}
	}
	s.metrics.TopUpResult(string(t.Kind), "completed")
	s.logger.InfoContext(ctx, "billing_top_up_completed",
		slog.String("billing_account_id", t.BillingAccountID),
		slog.String("top_up_id", t.ID),
		slog.String("kind", string(t.Kind)),
		slog.String("paddle_transaction_id", transactionID),
		slog.String("previous_status", string(t.Status)))
	return nil
}

// ProcessAutoTopUps charges the queued automatic top-ups. Each top-up moves
// to charging in its own transaction under the billing account lock before
// the Paddle call, so two workers never charge one top-up twice.
func (s *Service) ProcessAutoTopUps(ctx context.Context) error {
	for range 100 {
		var candidate topUp
		err := s.db.Pool.QueryRow(ctx, `SELECT `+topUpColumns+` FROM top_ups WHERE status = 'pending'
			ORDER BY create_time LIMIT 1`).Scan(&candidate.ID, &candidate.BillingAccountID, &candidate.Kind,
			&candidate.Amount, &candidate.Status, &candidate.PaddleSubscriptionID, &candidate.CreateTime)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var claimed topUp
		var ok bool
		err = s.db.InTx(ctx, func(tx pgx.Tx) error {
			now := s.timestamp()
			acct, err := lockAccount(ctx, tx, candidate.BillingAccountID)
			if err != nil {
				return err
			}
			claimed, err = scanTopUp(tx.QueryRow(ctx, `SELECT `+topUpColumns+` FROM top_ups WHERE top_up_id = $1 FOR UPDATE`, candidate.ID))
			if err != nil || claimed.Status != TopUpPending {
				return err
			}
			// Charge the current subscription; the one of the time of the
			// deduction may have been replaced.
			sub, err := LoadSubscription(ctx, tx, acct.ID, false)
			if err != nil {
				return err
			}
			if acct.Deleted || sub.PaddleSubscriptionID == "" || !sub.Manageable() {
				return s.failTopUp(ctx, tx, acct, claimed.ID, FailureNoSubscription, now)
			}
			claimed.PaddleSubscriptionID = sub.PaddleSubscriptionID
			claimed.Status = TopUpCharging
			ok = true
			_, err = tx.Exec(ctx, `UPDATE top_ups SET status = 'charging', paddle_subscription_id = $2, update_time = $3
				WHERE top_up_id = $1`, claimed.ID, claimed.PaddleSubscriptionID, now)
			return err
		})
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := s.charge(ctx, claimed); err != nil {
			return err
		}
	}
	return nil
}

// TimeOutTopUps fails the top-ups that waited longer than TopUpTimeout for
// their result.
func (s *Service) TimeOutTopUps(ctx context.Context) error {
	cutoff := s.timestamp().Add(-TopUpTimeout)
	rows, err := s.db.Pool.Query(ctx, `SELECT top_up_id::text, billing_account_id::text FROM top_ups
		WHERE status IN ('charging', 'charged', 'unknown') AND update_time < $1 ORDER BY update_time LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	type ref struct{ ID, AccountID string }
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ref])
	if err != nil {
		return err
	}
	for _, r := range refs {
		err := s.db.InTx(ctx, func(tx pgx.Tx) error {
			now := s.timestamp()
			acct, err := lockAccount(ctx, tx, r.AccountID)
			if err != nil {
				return err
			}
			var stale bool
			if err := tx.QueryRow(ctx, `SELECT status IN ('charging', 'charged', 'unknown') AND update_time < $2
				FROM top_ups WHERE top_up_id = $1`, r.ID, now.Add(-TopUpTimeout)).Scan(&stale); err != nil {
				return err
			}
			if !stale {
				return nil
			}
			return s.failTopUp(ctx, tx, acct, r.ID, FailureTimedOut, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
