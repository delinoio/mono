// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func TestPaymentWebhookCreditExpiryUsesCapture(t *testing.T) {
	capture := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	occurred := capture.Add(time.Minute)
	processed := capture.AddDate(0, 0, 2)
	expiry := capture.AddDate(1, 0, 0)
	for _, kind := range []LedgerKind{LedgerSubscriptionCredit, LedgerManualTopUp, LedgerAutoTopUp} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			f.clock.Set(processed)
			acct := f.account()
			txnID := "txn_top_up"
			data := paddletest.TransactionData{ID: txnID, Origin: paddle.OriginSubscriptionCharge, SubtotalCents: 2500}
			wantAmount := int64(25 * usd)
			if kind == LedgerSubscriptionCredit {
				txnID = f.checkout(acct)
				data = paddletest.TransactionData{ID: txnID, Origin: paddle.OriginWeb, SubscriptionID: "sub_checkout", PriceID: priceID, SubtotalCents: 1000}
				wantAmount = 10 * usd
			} else {
				topUpID, err := newID()
				if err != nil {
					t.Fatal(err)
				}
				topUpKind := TopUpManual
				var idempotencyKey *string
				if kind == LedgerAutoTopUp {
					topUpKind = TopUpAuto
				} else {
					idempotencyKey = &topUpID
				}
				if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind,
					idempotency_key, amount_usd_micros, status, create_time, update_time)
					VALUES ($1, $2, $3, $4, $5, 'charged', $6, $6)`,
					topUpID, acct, string(topUpKind), idempotencyKey, wantAmount, processed); err != nil {
					t.Fatal(err)
				}
				data.TopUpID = topUpID
			}
			payment := paddletest.Transaction(data)
			payment["payments"] = []map[string]any{
				{"status": "error", "captured_at": nil},
				{"status": "captured", "captured_at": capture.In(time.FixedZone("offset", -4*60*60))},
			}
			deliver := func(eventID string) {
				t.Helper()
				body := paddletest.Event(eventID, paddle.EventTransactionCompleted, occurred, payment)
				if code := f.deliver(body, paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)); code != http.StatusOK {
					t.Fatalf("payment %s: HTTP %d", eventID, code)
				}
			}
			deliver("evt_capture")
			deliver("evt_capture")
			payment["payments"] = nil
			f.clock.Advance(time.Hour)
			deliver("evt_capture_replay")
			f.wantLedger(acct, wantEntry{kind, wantAmount, wantAmount})

			var created, eventOccurred, eventProcessed time.Time
			if err := f.db.Pool.QueryRow(ctx(), `SELECT g.create_time, e.occurred_at, e.process_time
				FROM credit_grants g JOIN paddle_events e ON e.billing_account_id = g.billing_account_id
				WHERE g.paddle_transaction_id = $1 AND e.paddle_event_id = 'evt_capture'`, txnID).
				Scan(&created, &eventOccurred, &eventProcessed); err != nil {
				t.Fatal(err)
			}
			if expires := f.grantExpireTime(txnID); !expires.Equal(expiry) || !created.Equal(processed) ||
				!eventOccurred.Equal(occurred) || !eventProcessed.Equal(processed) {
				t.Fatalf("expiry %s, grant creation %s, event occurrence %s, event process %s",
					expires, created, eventOccurred, eventProcessed)
			}
			invoices, err := ListInvoices(ctx(), f.db.Pool, acct, time.Time{}, "", 10)
			if err != nil || len(invoices) != 1 || !invoices[0].CreateTime.Equal(processed) {
				t.Fatalf("invoices = %+v, err = %v", invoices, err)
			}
			if data.TopUpID != "" {
				var status TopUpStatus
				if err := f.db.Pool.QueryRow(ctx(), `SELECT status FROM top_ups WHERE top_up_id = $1`, data.TopUpID).
					Scan(&status); err != nil || status != TopUpCompleted {
					t.Fatalf("top-up status = %q, err = %v", status, err)
				}
			}
		})
	}
}

func TestPaymentWebhookCreditExpiryFallback(t *testing.T) {
	first := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		payments any
	}{
		{name: "missing payments"},
		{name: "null timestamp", payments: []map[string]any{{"status": "captured", "captured_at": nil}}},
		{name: "invalid timestamp", payments: []map[string]any{{"status": "captured", "captured_at": 123}}},
		{name: "malformed payments", payments: "unknown"},
		{name: "multiple captures", payments: []map[string]any{
			{"status": "captured", "captured_at": first},
			{"status": "captured", "captured_at": first.Add(time.Minute)},
		}},
		{name: "another captured attempt without a timestamp", payments: []map[string]any{
			{"status": "captured", "captured_at": first},
			{"status": "captured", "captured_at": nil},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			const txnID = "txn_fallback"
			payment := purchase(txnID, acct, LedgerManualTopUp, 2500)
			if tc.payments != nil {
				payment["payments"] = tc.payments
			}
			if result := f.webhookResult(paddle.EventTransactionCompleted, payment); result != webhookApplied {
				t.Fatalf("payment result %q, want %q", result, webhookApplied)
			}
			f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})
			if got, want := f.grantExpireTime(txnID), f.clock.Now().UTC().AddDate(1, 0, 0); !got.Equal(want) {
				t.Fatalf("expiry %s, want %s", got, want)
			}
		})
	}
}

func TestPaymentWebhookExpiresNewlyRecordedCredit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		debt          int64
		pendingRefund bool
	}{
		{name: "empty account"},
		{name: "debt and pending refund", debt: 5 * usd, pendingRefund: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			const txnID = "txn_already_due"
			var want []wantEntry
			if tc.debt > 0 {
				f.deduct(acct, tc.debt)
				want = append(want, wantEntry{LedgerUsage, -tc.debt, -tc.debt})
			}
			if tc.pendingRefund {
				data := paddletest.Adjustment("adj_early", "refund", "approved", txnID, 300)
				if result := f.webhookResult(paddle.EventAdjustmentCreated, data); result != webhookPending {
					t.Fatalf("refund result %q, want %q", result, webhookPending)
				}
			}
			// Postgres stores the timestamp at microsecond precision, so this
			// capture expires at the processing time once stored.
			capture := f.clock.Now().UTC().AddDate(-1, 0, 0).Add(time.Nanosecond)
			payment := purchase(txnID, acct, LedgerManualTopUp, 2500)
			payment["payments"] = []map[string]any{{"status": "captured", "captured_at": capture}}
			for range 2 {
				if result := f.webhookResult(paddle.EventTransactionCompleted, payment); result != webhookApplied {
					t.Fatalf("payment result %q, want %q", result, webhookApplied)
				}
			}
			kept := 25*usd - tc.debt
			want = append(want, wantEntry{LedgerManualTopUp, 25 * usd, kept}, wantEntry{LedgerExpiry, -kept, 0})
			var balance int64
			if tc.pendingRefund {
				balance = -3 * usd
				want = append(want, wantEntry{LedgerRefund, balance, balance})
				if pending := f.pendingCount(); pending != 0 {
					t.Fatalf("pending adjustments = %d, want 0", pending)
				}
			}
			f.wantLedger(acct, want...)
			got, grants, err := Balance(ctx(), f.db.Pool, acct, f.clock.Now())
			if err != nil || got != balance || len(grants) != 0 {
				t.Fatalf("usable balance = %d, grants = %+v, err = %v", got, grants, err)
			}
		})
	}
}
