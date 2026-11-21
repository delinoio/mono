// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// Subscription is the stored subscription of a billing account.
type Subscription struct {
	Status                SubscriptionStatus
	PaddleSubscriptionID  string
	CheckoutTransactionID string
	CancelAtPeriodEnd     bool
	CurrentPeriodEnd      *time.Time
	PaddleUpdateTime      *time.Time
	exists                bool
}

// Proto returns the protobuf message.
func (s Subscription) Proto() *accountsv1.Subscription {
	out := &accountsv1.Subscription{
		Status:            s.Status.Proto(),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		PriceUsdMicros:    SubscriptionPriceUSDMicros,
	}
	if s.CurrentPeriodEnd != nil && s.Status != SubscriptionPending {
		out.CurrentPeriodEnd = timestamppb.New(*s.CurrentPeriodEnd)
	}
	return out
}

// Manageable reports whether the subscription can be canceled or resumed.
func (s Subscription) Manageable() bool {
	return s.Status == SubscriptionActive || s.Status == SubscriptionPastDue
}

// LoadSubscription reads the subscription; a billing account without one has
// SubscriptionNone. lock locks the row for the transaction.
func LoadSubscription(ctx context.Context, q db.Querier, accountID string, lock bool) (Subscription, error) {
	query := `SELECT status, COALESCE(paddle_subscription_id, ''), COALESCE(checkout_transaction_id, ''),
			cancel_at_period_end, current_period_end, paddle_update_time
		FROM subscriptions WHERE billing_account_id = $1`
	if lock {
		query += " FOR UPDATE"
	}
	var sub Subscription
	var status string
	err := q.QueryRow(ctx, query, accountID).Scan(&status, &sub.PaddleSubscriptionID, &sub.CheckoutTransactionID,
		&sub.CancelAtPeriodEnd, &sub.CurrentPeriodEnd, &sub.PaddleUpdateTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{Status: SubscriptionNone}, nil
	}
	if err != nil {
		return Subscription{}, err
	}
	sub.Status = SubscriptionStatus(status)
	sub.exists = true
	return sub, nil
}

// saveSubscription writes the subscription row.
func saveSubscription(ctx context.Context, tx pgx.Tx, accountID string, sub Subscription, customerID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO subscriptions (billing_account_id, status, paddle_subscription_id, paddle_customer_id,
			checkout_transaction_id, cancel_at_period_end, current_period_end, paddle_update_time, create_time, update_time)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8, $9, $9)
		ON CONFLICT (billing_account_id) DO UPDATE SET status = EXCLUDED.status,
			paddle_subscription_id = EXCLUDED.paddle_subscription_id,
			paddle_customer_id = COALESCE(EXCLUDED.paddle_customer_id, subscriptions.paddle_customer_id),
			checkout_transaction_id = EXCLUDED.checkout_transaction_id,
			cancel_at_period_end = EXCLUDED.cancel_at_period_end, current_period_end = EXCLUDED.current_period_end,
			paddle_update_time = EXCLUDED.paddle_update_time, update_time = EXCLUDED.update_time`,
		accountID, string(sub.Status), sub.PaddleSubscriptionID, customerID, sub.CheckoutTransactionID,
		sub.CancelAtPeriodEnd, sub.CurrentPeriodEnd, sub.PaddleUpdateTime, now)
	return err
}

// mapPaddleStatus maps a Paddle subscription status. accounts-api never
// pauses or trials subscriptions; a paused subscription is not active and
// counts as PAST_DUE, a trialing one as ACTIVE.
func mapPaddleStatus(status string) (SubscriptionStatus, bool) {
	switch status {
	case paddle.SubscriptionStatusActive, paddle.SubscriptionStatusTrialing:
		return SubscriptionActive, true
	case paddle.SubscriptionStatusPastDue, paddle.SubscriptionStatusPaused:
		return SubscriptionPastDue, true
	case paddle.SubscriptionStatusCanceled:
		return SubscriptionCanceled, true
	default:
		return "", false
	}
}

// recordPaddleSubscription remembers which billing account a Paddle
// subscription belongs to.
func recordPaddleSubscription(ctx context.Context, tx pgx.Tx, paddleID, accountID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO paddle_subscriptions (paddle_subscription_id, billing_account_id, create_time)
		VALUES ($1, $2, $3) ON CONFLICT (paddle_subscription_id) DO NOTHING`, paddleID, accountID, now)
	return err
}

// applyPaddleSubscription stores a Paddle subscription entity (from a
// webhook or an API response) as the subscription of the billing account,
// which tx holds locked. It ignores an entity older than the stored one and
// a subscription that is not the current one while the current one is
// ACTIVE or PAST_DUE. applied reports whether the row changed.
func (s *Service) applyPaddleSubscription(ctx context.Context, tx pgx.Tx, acct *accountState, entity paddle.Subscription, now time.Time) (bool, error) {
	status, ok := mapPaddleStatus(entity.Status)
	if !ok || entity.ID == "" {
		return false, nil
	}
	if err := recordPaddleSubscription(ctx, tx, entity.ID, acct.ID, now); err != nil {
		return false, err
	}
	current, err := LoadSubscription(ctx, tx, acct.ID, true)
	if err != nil {
		return false, err
	}
	if current.PaddleSubscriptionID != "" && current.PaddleSubscriptionID != entity.ID {
		if current.Manageable() {
			s.logger.WarnContext(ctx, "billing_subscription_not_current",
				slog.String("billing_account_id", acct.ID),
				slog.String("paddle_subscription_id", entity.ID))
			return false, nil
		}
	} else if current.PaddleUpdateTime != nil && !entity.UpdatedAt.IsZero() && entity.UpdatedAt.Before(*current.PaddleUpdateTime) {
		return false, nil
	}
	next := Subscription{
		Status:               status,
		PaddleSubscriptionID: entity.ID,
		CancelAtPeriodEnd:    entity.CancelsAtPeriodEnd(),
	}
	if !entity.UpdatedAt.IsZero() {
		t := entity.UpdatedAt.UTC()
		next.PaddleUpdateTime = &t
	}
	if entity.CurrentBillingPeriod != nil && status != SubscriptionCanceled {
		t := entity.CurrentBillingPeriod.EndsAt.UTC()
		next.CurrentPeriodEnd = &t
	}
	if acct.Deleted && status != SubscriptionCanceled {
		// A checkout that finished after the deletion. Operators cancel it
		// in the Paddle dashboard; the log names it.
		s.logger.WarnContext(ctx, "billing_subscription_of_deleted_account",
			slog.String("billing_account_id", acct.ID),
			slog.String("paddle_subscription_id", entity.ID))
	}
	if err := saveSubscription(ctx, tx, acct.ID, next, entity.CustomerID, now); err != nil {
		return false, err
	}
	if current.Status != next.Status {
		s.logger.InfoContext(ctx, "billing_subscription_status_changed",
			slog.String("billing_account_id", acct.ID),
			slog.String("paddle_subscription_id", entity.ID),
			slog.String("from", string(current.Status)),
			slog.String("to", string(next.Status)))
	}
	return true, nil
}

// activateFromPayment marks the subscription ACTIVE after a verified
// subscription payment, in case the transaction webhook arrives before the
// subscription webhook. It does not replace another current subscription.
func (s *Service) activateFromPayment(ctx context.Context, tx pgx.Tx, acct *accountState, txn paddle.Transaction, now time.Time) error {
	if txn.SubscriptionID == "" {
		return nil
	}
	if err := recordPaddleSubscription(ctx, tx, txn.SubscriptionID, acct.ID, now); err != nil {
		return err
	}
	current, err := LoadSubscription(ctx, tx, acct.ID, true)
	if err != nil {
		return err
	}
	if current.PaddleSubscriptionID != "" && current.PaddleSubscriptionID != txn.SubscriptionID && current.Manageable() {
		return nil
	}
	if current.PaddleSubscriptionID == txn.SubscriptionID && current.Status == SubscriptionCanceled {
		// The payment of a subscription that has ended already, for example
		// a webhook that arrived late. The subscription webhooks own the
		// state.
		return nil
	}
	next := current
	next.Status = SubscriptionActive
	next.PaddleSubscriptionID = txn.SubscriptionID
	next.CheckoutTransactionID = ""
	if current.PaddleSubscriptionID != txn.SubscriptionID {
		next.CancelAtPeriodEnd = false
		next.PaddleUpdateTime = nil
	}
	if txn.BillingPeriod != nil {
		end := txn.BillingPeriod.EndsAt.UTC()
		if next.CurrentPeriodEnd == nil || end.After(*next.CurrentPeriodEnd) {
			next.CurrentPeriodEnd = &end
		}
	}
	if err := saveSubscription(ctx, tx, acct.ID, next, txn.CustomerID, now); err != nil {
		return err
	}
	if current.Status != SubscriptionActive {
		s.logger.InfoContext(ctx, "billing_subscription_status_changed",
			slog.String("billing_account_id", acct.ID),
			slog.String("paddle_subscription_id", txn.SubscriptionID),
			slog.String("paddle_transaction_id", txn.ID),
			slog.String("from", string(current.Status)),
			slog.String("to", string(SubscriptionActive)))
	}
	return nil
}

// paddleError maps a Paddle failure of a user action to a Connect error.
func paddleError(err error, action string) error {
	switch paddle.Classify(err) {
	case paddle.OutcomeRejected, paddle.OutcomeNotFound:
		return failedPrecondition("the payment provider rejected the " + action)
	default:
		return unavailable("the payment provider is unavailable; try again later")
	}
}

// StartSubscription returns the checkout transaction of the subscription.
// tx holds the billing account lock. While the subscription is PENDING it
// returns the open checkout transaction again; with ACTIVE or PAST_DUE it
// fails with already_exists. Otherwise it creates a Paddle transaction for
// the subscription price with the billing account ID in its custom data.
// created reports whether a new checkout was created.
func (s *Service) StartSubscription(ctx context.Context, tx pgx.Tx, accountID string) (txnID string, sub Subscription, created bool, err error) {
	now := s.timestamp()
	sub, err = LoadSubscription(ctx, tx, accountID, true)
	if err != nil {
		return "", Subscription{}, false, err
	}
	switch sub.Status {
	case SubscriptionActive, SubscriptionPastDue:
		return "", Subscription{}, false, alreadyExists("the billing account already has a subscription")
	case SubscriptionPending:
		if sub.CheckoutTransactionID != "" {
			return sub.CheckoutTransactionID, sub, false, nil
		}
	}
	txn, err := s.paddle.CreateCheckoutTransaction(ctx, s.priceID, paddle.CustomData{paddle.CustomDataBillingAccountID: accountID})
	if err != nil {
		s.logger.WarnContext(ctx, "billing_paddle_call_failed",
			slog.String("operation", "create_checkout_transaction"),
			slog.String("billing_account_id", accountID),
			slog.String("paddle_error_code", paddle.ErrorCode(err)),
			slog.Any("error", err))
		return "", Subscription{}, false, paddleError(err, "checkout")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO paddle_checkouts (paddle_transaction_id, billing_account_id, create_time)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, txn.ID, accountID, now); err != nil {
		return "", Subscription{}, false, err
	}
	sub = Subscription{Status: SubscriptionPending, CheckoutTransactionID: txn.ID}
	if err := saveSubscription(ctx, tx, accountID, sub, "", now); err != nil {
		return "", Subscription{}, false, err
	}
	s.logger.InfoContext(ctx, "billing_subscription_checkout_created",
		slog.String("billing_account_id", accountID),
		slog.String("paddle_transaction_id", txn.ID))
	return txn.ID, sub, true, nil
}

// changeSubscription runs a Paddle call that returns the subscription entity
// and stores the result. tx holds the billing account lock.
func (s *Service) changeSubscription(ctx context.Context, tx pgx.Tx, accountID, operation string, call func(id string) (paddle.Subscription, error)) (Subscription, error) {
	now := s.timestamp()
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return Subscription{}, err
	}
	sub, err := LoadSubscription(ctx, tx, accountID, true)
	if err != nil {
		return Subscription{}, err
	}
	entity, err := call(sub.PaddleSubscriptionID)
	if err != nil {
		s.logger.WarnContext(ctx, "billing_paddle_call_failed",
			slog.String("operation", operation),
			slog.String("billing_account_id", accountID),
			slog.String("paddle_subscription_id", sub.PaddleSubscriptionID),
			slog.String("paddle_error_code", paddle.ErrorCode(err)),
			slog.Any("error", err))
		return Subscription{}, paddleError(err, "subscription change")
	}
	if _, err := s.applyPaddleSubscription(ctx, tx, acct, entity, now); err != nil {
		return Subscription{}, err
	}
	return LoadSubscription(ctx, tx, accountID, false)
}

// CancelSubscription schedules the cancellation at the end of the paid
// period. tx holds the billing account lock. A scheduled cancellation is
// kept unchanged.
func (s *Service) CancelSubscription(ctx context.Context, tx pgx.Tx, accountID string) (Subscription, bool, error) {
	sub, err := LoadSubscription(ctx, tx, accountID, true)
	if err != nil {
		return Subscription{}, false, err
	}
	if !sub.Manageable() || sub.PaddleSubscriptionID == "" {
		return Subscription{}, false, failedPrecondition("the subscription is not active or past due")
	}
	if sub.CancelAtPeriodEnd {
		return sub, false, nil
	}
	sub, err = s.changeSubscription(ctx, tx, accountID, "cancel_subscription", func(id string) (paddle.Subscription, error) {
		return s.paddle.CancelSubscription(ctx, id, paddle.EffectiveNextBillingPeriod)
	})
	return sub, err == nil, err
}

// ResumeSubscription removes a scheduled cancellation. tx holds the billing
// account lock. Without a scheduled cancellation nothing changes.
func (s *Service) ResumeSubscription(ctx context.Context, tx pgx.Tx, accountID string) (Subscription, bool, error) {
	sub, err := LoadSubscription(ctx, tx, accountID, true)
	if err != nil {
		return Subscription{}, false, err
	}
	if !sub.Manageable() || sub.PaddleSubscriptionID == "" {
		return Subscription{}, false, failedPrecondition("the subscription is not active or past due")
	}
	if !sub.CancelAtPeriodEnd {
		return sub, false, nil
	}
	sub, err = s.changeSubscription(ctx, tx, accountID, "resume_subscription", func(id string) (paddle.Subscription, error) {
		return s.paddle.RemoveScheduledChange(ctx, id)
	})
	return sub, err == nil, err
}

// CloseAccount runs the billing part of DeleteBillingAccount in tx, which
// holds the billing account lock: it cancels the Paddle subscription at once,
// forfeits the remaining credit with EXPIRY entries, fails queued automatic
// top-ups and turns automatic top-up off. A Paddle failure fails the deletion
// with unavailable (or failed_precondition when Paddle rejects the
// cancellation), so the billing account stays as it was.
func (s *Service) CloseAccount(ctx context.Context, tx pgx.Tx, accountID string) error {
	now := s.timestamp()
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return err
	}
	sub, err := LoadSubscription(ctx, tx, accountID, true)
	if err != nil {
		return err
	}
	if sub.PaddleSubscriptionID != "" && sub.Status != SubscriptionCanceled {
		entity, err := s.paddle.CancelSubscription(ctx, sub.PaddleSubscriptionID, paddle.EffectiveImmediately)
		switch outcome := paddle.Classify(err); outcome {
		case paddle.OutcomeOK:
			if _, err := s.applyPaddleSubscription(ctx, tx, acct, entity, now); err != nil {
				return err
			}
		case paddle.OutcomeNotFound:
			s.logger.WarnContext(ctx, "billing_subscription_missing_at_paddle",
				slog.String("billing_account_id", accountID),
				slog.String("paddle_subscription_id", sub.PaddleSubscriptionID))
		default:
			s.logger.WarnContext(ctx, "billing_paddle_call_failed",
				slog.String("operation", "cancel_subscription_immediately"),
				slog.String("billing_account_id", accountID),
				slog.String("paddle_subscription_id", sub.PaddleSubscriptionID),
				slog.String("paddle_error_code", paddle.ErrorCode(err)),
				slog.Any("error", err))
			return paddleError(err, "cancellation")
		}
	}
	if sub.exists {
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET status = 'canceled', cancel_at_period_end = false,
				current_period_end = NULL, checkout_transaction_id = NULL, update_time = $2
			WHERE billing_account_id = $1`, accountID, now); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT credit_grant_id::text, remaining_usd_micros FROM credit_grants
		WHERE billing_account_id = $1 AND remaining_usd_micros > 0 ORDER BY expire_time, credit_grant_id FOR UPDATE`, accountID)
	if err != nil {
		return err
	}
	grants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[grantBalance])
	if err != nil {
		return err
	}
	var forfeited int64
	for _, g := range grants {
		if err := s.forfeitGrant(ctx, tx, acct, g, now); err != nil {
			return err
		}
		forfeited += g.Remaining
	}
	if _, err := tx.Exec(ctx, `UPDATE top_ups SET status = 'failed', failure_reason = $2, update_time = $3
		WHERE billing_account_id = $1 AND status = 'pending'`, accountID, string(FailureNoSubscription), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_settings SET auto_top_up_enabled = false, update_time = $2
		WHERE billing_account_id = $1`, accountID, now); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "billing_account_closed",
		slog.String("billing_account_id", accountID),
		slog.String("paddle_subscription_id", sub.PaddleSubscriptionID),
		slog.Int64("forfeited_usd_micros", forfeited))
	return nil
}

// InvoiceURL returns the Paddle invoice link of a purchase of the billing
// account. A transaction of another billing account is not_found.
func (s *Service) InvoiceURL(ctx context.Context, q db.Querier, accountID, transactionID string) (string, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM credit_grants
		WHERE billing_account_id = $1 AND paddle_transaction_id = $2)`, accountID, transactionID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", notFound("transaction not found")
	}
	link, err := s.paddle.InvoiceURL(ctx, transactionID)
	if err != nil {
		s.logger.WarnContext(ctx, "billing_paddle_call_failed",
			slog.String("operation", "get_invoice"),
			slog.String("billing_account_id", accountID),
			slog.String("paddle_transaction_id", transactionID),
			slog.String("paddle_error_code", paddle.ErrorCode(err)),
			slog.Any("error", err))
		switch paddle.Classify(err) {
		case paddle.OutcomeRejected, paddle.OutcomeNotFound:
			return "", failedPrecondition("the transaction has no invoice yet")
		default:
			return "", unavailable("the payment provider is unavailable; try again later")
		}
	}
	return link, nil
}
