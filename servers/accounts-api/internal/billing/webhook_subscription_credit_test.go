// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func TestSubscriptionCreditLimitedToPaidAmount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		subtotal       int64
		discount       int64
		malformed      bool
		paid, grant    int64
		message, level string
	}{
		{name: "full payment", subtotal: 1000, grant: 10 * usd},
		{name: "above cap", subtotal: 1500, grant: 10 * usd},
		{name: "partial payment", subtotal: 1000, discount: 400, paid: 6 * usd, grant: 6 * usd, message: "billing_subscription_credit_reduced", level: "WARN"},
		{name: "zero payment", subtotal: 1000, discount: 1000, message: "billing_subscription_credit_skipped", level: "ERROR"},
		{name: "negative payment", subtotal: 1000, discount: 1100, paid: -usd, message: "billing_subscription_credit_skipped", level: "ERROR"},
		{name: "invalid payment", subtotal: 1000, malformed: true, message: "billing_subscription_credit_skipped", level: "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			txnID := f.checkout(acct)
			logs := f.captureLogs()
			payment := paddletest.Transaction(paddletest.TransactionData{
				ID: txnID, SubscriptionID: "sub_checkout", Origin: paddle.OriginWeb, PriceID: priceID,
				SubtotalCents: tc.subtotal, DiscountCents: tc.discount, TaxCents: 100,
			})
			if tc.malformed {
				payment["details"] = map[string]any{"totals": map[string]any{"subtotal": "invalid", "discount": "0"}}
			}
			for range 2 {
				if result := f.webhookResult(paddle.EventTransactionCompleted, payment); result != webhookApplied {
					t.Fatalf("payment result %q, want %q", result, webhookApplied)
				}
			}
			var want []wantEntry
			if tc.grant > 0 {
				want = append(want, wantEntry{LedgerSubscriptionCredit, tc.grant, tc.grant})
			}
			f.wantLedger(acct, want...)
			sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if err != nil || sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != "sub_checkout" {
				t.Fatalf("subscription = %+v, err = %v", sub, err)
			}

			type creditDecision struct {
				Message       string `json:"msg"`
				Level         string `json:"level"`
				AccountID     string `json:"billing_account_id"`
				TransactionID string `json:"paddle_transaction_id"`
				EventID       string `json:"paddle_event_id"`
				Paid          *int64 `json:"paid_usd_micros"`
				Granted       *int64 `json:"granted_usd_micros"`
			}
			var decisions []creditDecision
			dec := json.NewDecoder(logs)
			for {
				var decision creditDecision
				if err := dec.Decode(&decision); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch decision.Message {
				case "billing_subscription_credit_reduced", "billing_subscription_credit_skipped":
					decisions = append(decisions, decision)
				}
			}
			wantCount := 0
			if tc.message != "" {
				wantCount = 1
				if tc.grant == 0 {
					wantCount = 2
				}
			}
			if len(decisions) != wantCount {
				t.Fatalf("credit decisions = %+v, want %d", decisions, wantCount)
			}
			for _, decision := range decisions {
				want := creditDecision{
					Message: tc.message, Level: tc.level, AccountID: acct, TransactionID: txnID,
					EventID: decision.EventID, Paid: &tc.paid,
				}
				if tc.grant > 0 {
					want.Granted = &tc.grant
				}
				if decision.EventID == "" || !reflect.DeepEqual(decision, want) {
					t.Errorf("credit decision = %+v, want %+v", decision, want)
				}
			}
		})
	}
}

func TestCustomDataPaymentWithFullDiscountAddsNoCredit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	payment := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_unbound", SubscriptionID: "sub_unbound", Origin: paddle.OriginWeb, BillingAccountID: acct,
		PriceID: priceID, SubtotalCents: 1000, DiscountCents: 1000,
	})
	if result := f.webhookResult(paddle.EventTransactionCompleted, payment); result != webhookIgnored {
		t.Fatalf("payment result %q, want %q", result, webhookIgnored)
	}
	f.wantLedger(acct)
	sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil || sub.Status != SubscriptionNone || f.count(`SELECT count(*) FROM paddle_subscriptions`) != 0 {
		t.Fatalf("subscription = %+v, err = %v", sub, err)
	}
}

func TestRefundOfReducedSubscriptionCredit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	txnID := f.checkout(acct)
	payment := paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: "sub_checkout", Origin: paddle.OriginWeb, PriceID: priceID,
		SubtotalCents: 1000, DiscountCents: 400,
	})
	f.webhookResult(paddle.EventTransactionCompleted, payment)
	refund := paddletest.Adjustment("adj_cap", "refund", "approved", txnID, 1000)
	for range 2 {
		f.webhookResult(paddle.EventAdjustmentCreated, refund)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 6 * usd, 6 * usd}, wantEntry{LedgerRefund, -6 * usd, 0})
}

func TestSubscriptionCreditSkippedOnReplayStillReconcilesAdjustment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	const txnID = "txn_existing"
	at := f.clock.Now()
	oldReleaseGrant(t, f.db, acct, txnID, 10*usd, at)
	insertPending(t, f.db, "adj_pending", txnID, "refund", 4*usd, at)
	payment := paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID,
		SubtotalCents: 1000, DiscountCents: 1000,
	})
	if result := f.webhookResult(paddle.EventTransactionCompleted, payment); result != webhookIgnored {
		t.Fatalf("payment result %q, want %q", result, webhookIgnored)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd}, wantEntry{LedgerRefund, -4 * usd, 6 * usd})
	if pending := f.pendingCount(); pending != 0 {
		t.Fatalf("pending adjustments = %d, want 0", pending)
	}
}
