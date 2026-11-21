// SPDX-License-Identifier: FSL-1.1-ALv2

package paddle_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

const secret = "pdl_ntfset_test_secret"

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"event_id":"evt_1"}`)
	valid := paddle.SignatureHeaderValue(secret, now, body)
	other := paddle.SignatureHeaderValue("another secret", now, body)
	_, otherH1, _ := strings.Cut(other, ";")

	for name, tc := range map[string]struct {
		header string
		body   []byte
		now    time.Time
		want   error
	}{
		"valid":                {valid, body, now, nil},
		"valid at tolerance":   {valid, body, now.Add(paddle.SignatureTolerance), nil},
		"rotated secrets":      {valid + ";" + otherH1, body, now, nil},
		"missing":              {"", body, now, paddle.ErrSignatureMissing},
		"no h1":                {"ts=1800000000", body, now, paddle.ErrSignatureMalformed},
		"no ts":                {otherH1, body, now, paddle.ErrSignatureMalformed},
		"bad hex":              {"ts=1800000000;h1=zz", body, now, paddle.ErrSignatureMalformed},
		"bad ts":               {"ts=abc;" + otherH1, body, now, paddle.ErrSignatureMalformed},
		"stale":                {valid, body, now.Add(paddle.SignatureTolerance + time.Second), paddle.ErrSignatureStale},
		"future":               {valid, body, now.Add(-paddle.SignatureTolerance - time.Second), paddle.ErrSignatureStale},
		"other secret":         {other, body, now, paddle.ErrSignatureMismatch},
		"changed body":         {valid, []byte(`{"event_id":"evt_2"}`), now, paddle.ErrSignatureMismatch},
		"added white space":    {valid, append(append([]byte(nil), body...), '\n'), now, paddle.ErrSignatureMismatch},
		"segment without kv":   {"ts=1800000000;garbage", body, now, paddle.ErrSignatureMalformed},
		"unknown keys ignored": {valid + ";v2=abc", body, now, nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := paddle.VerifySignature(tc.header, tc.body, secret, tc.now, paddle.SignatureTolerance)
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifySignature = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	ev, err := paddle.ParseEvent(paddletest.Event("evt_1", paddle.EventTransactionCompleted, time.Now(), map[string]any{"id": "txn_1"}))
	if err != nil || ev.EventID != "evt_1" || ev.EventType != paddle.EventTransactionCompleted {
		t.Fatalf("ParseEvent = %+v, %v", ev, err)
	}
	for _, body := range []string{`not json`, `{}`, `{"event_id":"evt_1","event_type":"x"}`} {
		if _, err := paddle.ParseEvent([]byte(body)); err == nil {
			t.Errorf("ParseEvent(%s) succeeded", body)
		}
	}
}

func newClient(t *testing.T) (*paddle.Client, *paddletest.Server) {
	fake := paddletest.New(t)
	return paddle.New(paddle.Config{BaseURL: fake.URL, APIKey: paddletest.APIKey}), fake
}

func TestClientCheckoutAndSubscription(t *testing.T) {
	client, fake := newClient(t)
	ctx := context.Background()
	txn, err := client.CreateCheckoutTransaction(ctx, "pri_sub", paddle.CustomData{paddle.CustomDataBillingAccountID: "acct"})
	if err != nil || !strings.HasPrefix(txn.ID, "txn_") {
		t.Fatalf("CreateCheckoutTransaction = %+v, %v", txn, err)
	}
	req := fake.Requests()[0]
	items := req.Body["items"].([]any)
	if req.Path != "/transactions" || items[0].(map[string]any)["price_id"] != "pri_sub" ||
		req.Body["custom_data"].(map[string]any)["billing_account_id"] != "acct" {
		t.Fatalf("request = %+v", req)
	}

	end := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second)
	fake.SetSubscription(paddle.Subscription{ID: "sub_1", Status: paddle.SubscriptionStatusActive,
		CurrentBillingPeriod: &paddle.Period{StartsAt: end.AddDate(0, -1, 0), EndsAt: end}})
	sub, err := client.CancelSubscription(ctx, "sub_1", paddle.EffectiveNextBillingPeriod)
	if err != nil || !sub.CancelsAtPeriodEnd() || sub.Status != paddle.SubscriptionStatusActive {
		t.Fatalf("CancelSubscription = %+v, %v", sub, err)
	}
	sub, err = client.RemoveScheduledChange(ctx, "sub_1")
	if err != nil || sub.CancelsAtPeriodEnd() {
		t.Fatalf("RemoveScheduledChange = %+v, %v", sub, err)
	}
	if _, err := client.GetSubscription(ctx, "sub_missing"); paddle.Classify(err) != paddle.OutcomeNotFound {
		t.Fatalf("GetSubscription(missing) = %v", err)
	}
}

func TestClientOneTimeCharge(t *testing.T) {
	client, fake := newClient(t)
	ctx := context.Background()
	fake.SetSubscription(paddle.Subscription{ID: "sub_1", Status: paddle.SubscriptionStatusActive})
	_, err := client.CreateOneTimeCharge(ctx, "sub_1", paddle.Charge{
		ProductID: "pro_topup", AmountCents: 2500, Name: "Credit", Description: "Delino credit top-up",
		CustomData: paddle.CustomData{paddle.CustomDataTopUpID: "tu_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	charge := fake.Charges()[0]
	if charge.Body["effective_from"] != "immediately" || charge.Body["on_payment_failure"] != "prevent_change" ||
		paddletest.ChargeTopUpID(charge) != "tu_1" || paddletest.ChargeAmountCents(charge) != "2500" {
		t.Fatalf("charge body = %+v", charge.Body)
	}

	fake.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})
	_, err = client.CreateOneTimeCharge(ctx, "sub_1", paddle.Charge{ProductID: "pro", AmountCents: 2500, Description: "xx"})
	if paddle.Classify(err) != paddle.OutcomeRejected || paddle.ErrorCode(err) != paddle.CodePaymentDeclined {
		t.Fatalf("declined charge = %v", err)
	}
	if strings.Contains(err.Error(), "fake failure") {
		t.Fatalf("error carries the Paddle detail: %v", err)
	}
	fake.FailCharges(&paddletest.Failure{Status: http.StatusInternalServerError, Code: "internal_error"})
	_, err = client.CreateOneTimeCharge(ctx, "sub_1", paddle.Charge{ProductID: "pro", AmountCents: 2500, Description: "xx"})
	if paddle.Classify(err) != paddle.OutcomeUnknown {
		t.Fatalf("5xx charge = %v", err)
	}
	fake.FailCharges(&paddletest.Failure{Status: http.StatusTooManyRequests, Code: "too_many_requests"})
	_, err = client.CreateOneTimeCharge(ctx, "sub_1", paddle.Charge{ProductID: "pro", AmountCents: 2500, Description: "xx"})
	if paddle.Classify(err) != paddle.OutcomeNotApplied {
		t.Fatalf("429 charge = %v", err)
	}
	fake.FailCharges(nil)

	fake.SetSubscription(paddle.Subscription{ID: "sub_2", Status: paddle.SubscriptionStatusPastDue})
	_, err = client.CreateOneTimeCharge(ctx, "sub_2", paddle.Charge{ProductID: "pro", AmountCents: 2500, Description: "xx"})
	if paddle.ErrorCode(err) != paddle.CodeUpdateWhenPastDue {
		t.Fatalf("past_due charge = %v", err)
	}
}

func TestClientInvoiceAndAuth(t *testing.T) {
	client, fake := newClient(t)
	ctx := context.Background()
	link, err := client.InvoiceURL(ctx, "txn_1")
	if err != nil || !strings.Contains(link, "txn_1") {
		t.Fatalf("InvoiceURL = %q, %v", link, err)
	}
	wrong := paddle.New(paddle.Config{BaseURL: fake.URL, APIKey: "wrong"})
	if _, err := wrong.InvoiceURL(ctx, "txn_1"); paddle.Classify(err) != paddle.OutcomeNotApplied {
		t.Fatalf("wrong key = %v", err)
	}
	unreachable := paddle.New(paddle.Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"})
	if _, err := unreachable.GetSubscription(ctx, "sub"); paddle.Classify(err) != paddle.OutcomeUnknown {
		t.Fatalf("unreachable = %v", err)
	}
}

func TestEnvironment(t *testing.T) {
	if env, ok := paddle.ParseEnvironment("sandbox"); !ok || env.BaseURL() != "https://sandbox-api.paddle.com" {
		t.Fatal("sandbox")
	}
	if env, ok := paddle.ParseEnvironment("production"); !ok || env.BaseURL() != "https://api.paddle.com" {
		t.Fatal("production")
	}
	if _, ok := paddle.ParseEnvironment("live"); ok {
		t.Fatal("live accepted")
	}
}

func TestParseCents(t *testing.T) {
	if v, err := paddle.ParseCents("2500"); err != nil || v != 2500 {
		t.Fatal(v, err)
	}
	if v, err := paddle.ParseCents(""); err != nil || v != 0 {
		t.Fatal(v, err)
	}
	if _, err := paddle.ParseCents("25.00"); err == nil {
		t.Fatal("decimal accepted")
	}
}
