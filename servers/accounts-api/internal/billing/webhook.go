// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// WebhookPath is the route of the Paddle webhook.
const WebhookPath = "POST /webhooks/paddle"

// maxWebhookBytes bounds a webhook body. Paddle notifications are far
// smaller.
const maxWebhookBytes = 1 << 20

// Webhook results, which are also metric label values.
const (
	webhookApplied          = "applied"
	webhookDuplicate        = "duplicate"
	webhookIgnored          = "ignored"
	webhookUnmatched        = "unmatched"
	webhookInvalidSignature = "invalid_signature"
	webhookInvalidPayload   = "invalid_payload"
	webhookError            = "error"
)

// metricEventType bounds the event type label to the types that the server
// handles.
func metricEventType(eventType string) string {
	switch {
	case eventType == paddle.EventTransactionCompleted, eventType == paddle.EventTransactionPaymentFailed,
		eventType == paddle.EventAdjustmentCreated, eventType == paddle.EventAdjustmentUpdated:
		return eventType
	case paddle.IsSubscriptionEvent(eventType):
		return "subscription"
	case eventType == "":
		return "unknown"
	default:
		return "other"
	}
}

// eventOutcome is what applying one event did.
type eventOutcome struct {
	Result           string
	BillingAccountID string
	TransactionID    string
}

// WebhookHandler serves `POST /webhooks/paddle`. It verifies the
// `Paddle-Signature` header before it parses the body: a missing or invalid
// signature, or a timestamp more than 5 minutes off, gets 401 and changes
// nothing. A verified event is applied in the transaction that records its
// event ID, so a replayed event changes nothing; an error rolls everything
// back and answers 500, so Paddle retries.
func (s *Service) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
		if err != nil {
			s.webhookRejected(ctx, w, http.StatusRequestEntityTooLarge, webhookInvalidPayload, "body_too_large")
			return
		}
		if err := paddle.VerifySignature(r.Header.Get(paddle.SignatureHeader), body, s.webhookSecret, s.now(), paddle.SignatureTolerance); err != nil {
			reason := "signature_mismatch"
			switch {
			case errors.Is(err, paddle.ErrSignatureMissing):
				reason = "signature_missing"
			case errors.Is(err, paddle.ErrSignatureMalformed):
				reason = "signature_malformed"
			case errors.Is(err, paddle.ErrSignatureStale):
				reason = "signature_stale"
			}
			s.webhookRejected(ctx, w, http.StatusUnauthorized, webhookInvalidSignature, reason)
			return
		}
		ev, err := paddle.ParseEvent(body)
		if err != nil {
			s.webhookRejected(ctx, w, http.StatusBadRequest, webhookInvalidPayload, "invalid_event")
			return
		}
		start := time.Now()
		out, err := s.applyEvent(ctx, ev)
		attrs := []slog.Attr{
			slog.String("paddle_event_id", ev.EventID),
			slog.String("event_type", ev.EventType),
			slog.String("billing_account_id", out.BillingAccountID),
			slog.String("paddle_transaction_id", out.TransactionID),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if err != nil {
			s.metrics.Webhook(metricEventType(ev.EventType), webhookError)
			s.logger.LogAttrs(ctx, slog.LevelError, "billing_paddle_webhook_failed", append(attrs, slog.Any("error", err))...)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.metrics.Webhook(metricEventType(ev.EventType), out.Result)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "billing_paddle_webhook", append(attrs, slog.String("result", out.Result))...)
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Service) webhookRejected(ctx context.Context, w http.ResponseWriter, status int, result, reason string) {
	s.metrics.Webhook("unknown", result)
	s.logger.WarnContext(ctx, "billing_paddle_webhook_rejected",
		slog.Int("status", status),
		slog.String("reason", reason))
	http.Error(w, http.StatusText(status), status)
}

// applyEvent records the event ID and applies the event in one transaction.
func (s *Service) applyEvent(ctx context.Context, ev paddle.Event) (eventOutcome, error) {
	var out eventOutcome
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		now := s.timestamp()
		out = eventOutcome{}
		tag, err := tx.Exec(ctx, `INSERT INTO paddle_events (paddle_event_id, event_type, occurred_at, result, process_time)
			VALUES ($1, $2, $3, 'processing', $4) ON CONFLICT (paddle_event_id) DO NOTHING`,
			ev.EventID, ev.EventType, nullTime(ev.OccurredAt), now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out.Result = webhookDuplicate
			return nil
		}
		switch {
		case ev.EventType == paddle.EventTransactionCompleted:
			out, err = s.onTransactionCompleted(ctx, tx, ev, now)
		case ev.EventType == paddle.EventTransactionPaymentFailed:
			out, err = s.onPaymentFailed(ctx, tx, ev, now)
		case paddle.IsSubscriptionEvent(ev.EventType):
			out, err = s.onSubscription(ctx, tx, ev, now)
		case ev.EventType == paddle.EventAdjustmentCreated, ev.EventType == paddle.EventAdjustmentUpdated:
			out, err = s.onAdjustment(ctx, tx, ev, now)
		default:
			out.Result = webhookIgnored
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE paddle_events SET result = $2, billing_account_id = NULLIF($3, '')::uuid
			WHERE paddle_event_id = $1`, ev.EventID, out.Result, out.BillingAccountID)
		return err
	})
	return out, err
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// resolveAccount finds the billing account of a Paddle entity: first through
// the checkout transactions and subscriptions that accounts-api recorded,
// then through a top-up ID of a one-time charge, and last through the
// `billing_account_id` custom data. The custom data is only a fallback,
// because a checkout opened in the browser can carry any custom data; it
// still names a billing account that exists.
func resolveAccount(ctx context.Context, tx pgx.Tx, transactionID, subscriptionID, topUpID string, custom map[string]any) (string, error) {
	lookup := func(query, key string) (string, error) {
		if key == "" {
			return "", nil
		}
		var id string
		err := tx.QueryRow(ctx, query, key).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	candidates := []struct{ query, key string }{
		{`SELECT billing_account_id::text FROM paddle_checkouts WHERE paddle_transaction_id = $1`, transactionID},
		{`SELECT billing_account_id::text FROM paddle_subscriptions WHERE paddle_subscription_id = $1`, subscriptionID},
	}
	if uuidv7.Valid(topUpID) {
		candidates = append(candidates, struct{ query, key string }{
			`SELECT billing_account_id::text FROM top_ups WHERE top_up_id = $1::uuid`, strings.ToLower(topUpID)})
	}
	if id := paddle.CustomString(custom, paddle.CustomDataBillingAccountID); uuidv7.Valid(id) {
		candidates = append(candidates, struct{ query, key string }{
			`SELECT billing_account_id::text FROM billing_accounts WHERE billing_account_id = $1::uuid`, strings.ToLower(id)})
	}
	for _, c := range candidates {
		id, err := lookup(c.query, c.key)
		if err != nil || id != "" {
			return id, err
		}
	}
	return "", nil
}

// chargeTopUpID returns the `top_up_id` custom data of the first item price
// that has one.
func chargeTopUpID(txn paddle.Transaction) string {
	for _, item := range txn.Items {
		if id := paddle.CustomString(item.Price.CustomData, paddle.CustomDataTopUpID); id != "" {
			return id
		}
	}
	return ""
}

// hasPrice reports whether the transaction has an item of priceID.
func hasPrice(txn paddle.Transaction, priceID string) bool {
	for _, item := range txn.Items {
		if item.Price.ID == priceID {
			return true
		}
	}
	return false
}

// creditOf returns the tax-exclusive credit amount of a transaction: the
// subtotal after discounts, in cents converted to micro-USD.
func creditOf(txn paddle.Transaction) (int64, error) {
	subtotal, err := paddle.ParseCents(txn.Details.Totals.Subtotal)
	if err != nil {
		return 0, err
	}
	discount, err := paddle.ParseCents(txn.Details.Totals.Discount)
	if err != nil {
		return 0, err
	}
	return CentsToMicros(subtotal - discount)
}

// loadTopUpForCharge returns the top-up of a one-time charge transaction in
// the billing account, locked, or ok false.
func loadTopUpForCharge(ctx context.Context, tx pgx.Tx, accountID, topUpID string) (topUp, bool, error) {
	if !uuidv7.Valid(topUpID) {
		return topUp{}, false, nil
	}
	t, err := scanTopUp(tx.QueryRow(ctx, `SELECT `+topUpColumns+` FROM top_ups
		WHERE top_up_id = $1::uuid AND billing_account_id = $2 FOR UPDATE`, strings.ToLower(topUpID), accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return topUp{}, false, nil
	}
	return t, err == nil, err
}

// onTransactionCompleted adds the credit of a completed purchase: the $10
// grant of a subscription payment, or the pre-tax amount of a one-time
// top-up charge.
func (s *Service) onTransactionCompleted(ctx context.Context, tx pgx.Tx, ev paddle.Event, now time.Time) (eventOutcome, error) {
	var txn paddle.Transaction
	if err := json.Unmarshal(ev.Data, &txn); err != nil || txn.ID == "" {
		return eventOutcome{Result: webhookInvalidPayload}, nil
	}
	out := eventOutcome{TransactionID: txn.ID}
	topUpID := chargeTopUpID(txn)
	accountID, err := resolveAccount(ctx, tx, txn.ID, txn.SubscriptionID, topUpID, txn.CustomData)
	if err != nil {
		return out, err
	}
	if accountID == "" {
		s.logger.WarnContext(ctx, "billing_paddle_event_unmatched",
			slog.String("paddle_event_id", ev.EventID), slog.String("paddle_transaction_id", txn.ID))
		out.Result = webhookUnmatched
		return out, nil
	}
	out.BillingAccountID = accountID
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return out, err
	}
	if acct.Deleted {
		s.logger.WarnContext(ctx, "billing_paddle_event_for_deleted_account",
			slog.String("paddle_event_id", ev.EventID), slog.String("billing_account_id", accountID),
			slog.String("paddle_transaction_id", txn.ID))
		out.Result = webhookIgnored
		return out, nil
	}
	if !strings.EqualFold(txn.CurrencyCode, "USD") {
		s.logger.ErrorContext(ctx, "billing_paddle_currency_not_usd",
			slog.String("paddle_event_id", ev.EventID), slog.String("billing_account_id", accountID),
			slog.String("paddle_transaction_id", txn.ID), slog.String("currency_code", txn.CurrencyCode))
		out.Result = webhookIgnored
		return out, nil
	}
	switch {
	case txn.Origin == paddle.OriginSubscriptionCharge || topUpID != "":
		amount, err := creditOf(txn)
		if err != nil || amount <= 0 {
			s.logger.ErrorContext(ctx, "billing_paddle_amount_invalid",
				slog.String("paddle_event_id", ev.EventID), slog.String("paddle_transaction_id", txn.ID))
			out.Result = webhookIgnored
			return out, nil
		}
		t, found, err := loadTopUpForCharge(ctx, tx, accountID, topUpID)
		if err != nil {
			return out, err
		}
		kind := TopUpManual
		if found {
			kind = t.Kind
			if t.Amount != amount {
				s.logger.WarnContext(ctx, "billing_top_up_amount_mismatch",
					slog.String("billing_account_id", accountID), slog.String("top_up_id", t.ID),
					slog.String("paddle_transaction_id", txn.ID),
					slog.Int64("requested_usd_micros", t.Amount), slog.Int64("paid_usd_micros", amount))
			}
		} else {
			// A one-time charge that accounts-api did not create, for
			// example from the Paddle dashboard. It is a purchase of credit,
			// so it becomes a manual top-up grant.
			s.logger.WarnContext(ctx, "billing_charge_without_top_up",
				slog.String("billing_account_id", accountID), slog.String("paddle_transaction_id", txn.ID))
		}
		if _, _, err := s.addGrant(ctx, tx, acct, grantInput{Kind: kind.LedgerKind(), Amount: amount, PaddleTransactionID: txn.ID}, now); err != nil {
			return out, err
		}
		if found {
			if err := s.completeTopUp(ctx, tx, t, txn.ID, now); err != nil {
				return out, err
			}
		}
	case hasPrice(txn, s.priceID):
		if _, _, err := s.addGrant(ctx, tx, acct, grantInput{
			Kind: LedgerSubscriptionCredit, Amount: SubscriptionCreditUSDMicros, PaddleTransactionID: txn.ID,
		}, now); err != nil {
			return out, err
		}
		if err := s.activateFromPayment(ctx, tx, acct, txn, now); err != nil {
			return out, err
		}
	default:
		s.logger.WarnContext(ctx, "billing_transaction_not_credit",
			slog.String("billing_account_id", accountID), slog.String("paddle_transaction_id", txn.ID),
			slog.String("origin", txn.Origin))
		out.Result = webhookIgnored
		return out, nil
	}
	out.Result = webhookApplied
	return out, nil
}

// onPaymentFailed handles a failed payment: a failed one-time charge fails
// its top-up, and a failed renewal of the subscription emails the owners and
// admins. Failed attempts inside the checkout are ignored; the user sees
// them there.
func (s *Service) onPaymentFailed(ctx context.Context, tx pgx.Tx, ev paddle.Event, now time.Time) (eventOutcome, error) {
	var txn paddle.Transaction
	if err := json.Unmarshal(ev.Data, &txn); err != nil || txn.ID == "" {
		return eventOutcome{Result: webhookInvalidPayload}, nil
	}
	out := eventOutcome{TransactionID: txn.ID}
	topUpID := chargeTopUpID(txn)
	accountID, err := resolveAccount(ctx, tx, txn.ID, txn.SubscriptionID, topUpID, txn.CustomData)
	if err != nil || accountID == "" {
		out.Result = webhookUnmatched
		return out, err
	}
	out.BillingAccountID = accountID
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return out, err
	}
	switch {
	case txn.Origin == paddle.OriginSubscriptionCharge || topUpID != "":
		t, found, err := loadTopUpForCharge(ctx, tx, accountID, topUpID)
		if err != nil {
			return out, err
		}
		if !found {
			out.Result = webhookIgnored
			return out, nil
		}
		if err := s.failTopUp(ctx, tx, acct, t.ID, FailurePaymentFailed, now); err != nil {
			return out, err
		}
	case txn.Origin == paddle.OriginWeb || txn.Origin == paddle.OriginAPI || acct.Deleted:
		out.Result = webhookIgnored
		return out, nil
	case txn.SubscriptionID != "":
		s.logger.WarnContext(ctx, "billing_subscription_payment_failed",
			slog.String("billing_account_id", accountID),
			slog.String("paddle_subscription_id", txn.SubscriptionID),
			slog.String("paddle_transaction_id", txn.ID))
		if err := s.enqueue(ctx, tx, accountID, NotifyPaymentFailed, "payment_failed/subscription/"+txn.ID,
			map[string]string{notifyDetailPaymentScope: notifyScopeSubscription}, now); err != nil {
			return out, err
		}
	default:
		out.Result = webhookIgnored
		return out, nil
	}
	out.Result = webhookApplied
	return out, nil
}

// onSubscription stores the subscription entity of a subscription event.
func (s *Service) onSubscription(ctx context.Context, tx pgx.Tx, ev paddle.Event, now time.Time) (eventOutcome, error) {
	var entity paddle.Subscription
	if err := json.Unmarshal(ev.Data, &entity); err != nil || entity.ID == "" {
		return eventOutcome{Result: webhookInvalidPayload}, nil
	}
	accountID, err := resolveAccount(ctx, tx, entity.TransactionID, entity.ID, "", entity.CustomData)
	if err != nil || accountID == "" {
		if err == nil {
			s.logger.WarnContext(ctx, "billing_paddle_event_unmatched",
				slog.String("paddle_event_id", ev.EventID), slog.String("paddle_subscription_id", entity.ID))
		}
		return eventOutcome{Result: webhookUnmatched}, err
	}
	out := eventOutcome{BillingAccountID: accountID, TransactionID: entity.TransactionID}
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return out, err
	}
	applied, err := s.applyPaddleSubscription(ctx, tx, acct, entity, now)
	if err != nil {
		return out, err
	}
	out.Result = webhookIgnored
	if applied {
		out.Result = webhookApplied
	}
	return out, nil
}

// onAdjustment removes credit for an approved refund or chargeback of a
// purchase. Each adjustment applies once, also when Paddle sends it as
// created and again as updated. The amount is the tax-exclusive subtotal of
// the adjustment, at most the credit of the purchase that is not refunded
// yet; the balance can become negative.
func (s *Service) onAdjustment(ctx context.Context, tx pgx.Tx, ev paddle.Event, now time.Time) (eventOutcome, error) {
	var adj paddle.Adjustment
	if err := json.Unmarshal(ev.Data, &adj); err != nil || adj.ID == "" {
		return eventOutcome{Result: webhookInvalidPayload}, nil
	}
	out := eventOutcome{TransactionID: adj.TransactionID}
	if (adj.Action != paddle.AdjustmentActionRefund && adj.Action != paddle.AdjustmentActionChargeback) ||
		adj.Status != paddle.AdjustmentStatusApproved {
		out.Result = webhookIgnored
		return out, nil
	}
	var grantID, accountID string
	err := tx.QueryRow(ctx, `SELECT credit_grant_id::text, billing_account_id::text FROM credit_grants
		WHERE paddle_transaction_id = $1`, adj.TransactionID).Scan(&grantID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		s.logger.WarnContext(ctx, "billing_refund_without_grant",
			slog.String("paddle_event_id", ev.EventID), slog.String("paddle_transaction_id", adj.TransactionID))
		out.Result = webhookUnmatched
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.BillingAccountID = accountID
	acct, err := lockAccount(ctx, tx, accountID)
	if err != nil {
		return out, err
	}
	var original, refunded int64
	if err := tx.QueryRow(ctx, `SELECT original_usd_micros, refunded_usd_micros FROM credit_grants
		WHERE credit_grant_id = $1 FOR UPDATE`, grantID).Scan(&original, &refunded); err != nil {
		return out, err
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM credit_refunds WHERE paddle_adjustment_id = $1)`,
		adj.ID).Scan(&done); err != nil {
		return out, err
	}
	if done || acct.Deleted {
		out.Result = webhookIgnored
		return out, nil
	}
	subtotal := adj.Totals.Subtotal
	cents, err := paddle.ParseCents(subtotal)
	if err != nil || cents < 0 {
		out.Result = webhookInvalidPayload
		return out, nil
	}
	amount, err := CentsToMicros(cents)
	if err != nil {
		return out, err
	}
	amount = min(amount, original-refunded)
	var entryID *string
	if amount > 0 {
		id, err := s.removeCredit(ctx, tx, acct, grantID, adj.TransactionID, amount, now)
		if err != nil {
			return out, err
		}
		entryID = &id
	}
	if _, err := tx.Exec(ctx, `INSERT INTO credit_refunds (paddle_adjustment_id, credit_grant_id, amount_usd_micros,
			ledger_entry_id, create_time) VALUES ($1, $2, $3, $4, $5)`, adj.ID, grantID, amount, entryID, now); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `UPDATE credit_grants SET refunded_usd_micros = refunded_usd_micros + $2
		WHERE credit_grant_id = $1`, grantID, amount); err != nil {
		return out, err
	}
	s.logger.InfoContext(ctx, "billing_credit_refunded",
		slog.String("billing_account_id", accountID),
		slog.String("credit_grant_id", grantID),
		slog.String("paddle_transaction_id", adj.TransactionID),
		slog.String("paddle_adjustment_id", adj.ID),
		slog.String("action", adj.Action),
		slog.Int64("amount_usd_micros", amount),
		slog.Int64("balance_after_usd_micros", acct.Balance))
	out.Result = webhookApplied
	return out, nil
}
