// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

const usd int64 = 1_000_000

func newKey(t *testing.T) string {
	t.Helper()
	key, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// subscribe starts the subscription as the owner and confirms its payment
// with a verified webhook. It returns the Paddle subscription ID.
func subscribe(t *testing.T, env *testenv.Env, f fixture) string {
	t.Helper()
	owner := env.Credit(f.Owner.ID)
	resp, err := owner.StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatalf("StartSubscription: %v", err)
	}
	subID := env.Paddle.NewID("sub")
	end := time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second)
	env.Paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusActive,
		CurrentBillingPeriod: &paddle.Period{StartsAt: end.AddDate(0, -1, 0), EndsAt: end}})
	if code := env.Webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: resp.Msg.GetPaddleTransactionId(), SubscriptionID: subID, Origin: paddle.OriginWeb,
		PriceID: testenv.PaddlePriceID, SubtotalCents: 1000, TaxCents: 100, PeriodEnd: end,
	})); code != http.StatusOK {
		t.Fatalf("payment webhook status %d", code)
	}
	return subID
}

func balanceOf(t *testing.T, env *testenv.Env, userID, accountID string) *accountsv1.GetBalanceResponse {
	t.Helper()
	resp, err := env.Credit(userID).GetBalance(ctx(), connect.NewRequest(&accountsv1.GetBalanceRequest{BillingAccountId: accountID}))
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	return resp.Msg
}

func ledgerKinds(t *testing.T, env *testenv.Env, userID, accountID string) []string {
	t.Helper()
	resp, err := env.Credit(userID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{BillingAccountId: accountID}))
	if err != nil {
		t.Fatalf("ListLedgerEntries: %v", err)
	}
	var out []string
	for _, e := range resp.Msg.GetLedgerEntries() {
		out = append(out, strings.TrimPrefix(e.GetKind().String(), "LEDGER_ENTRY_KIND_"))
	}
	return out
}

func TestCreditRoles(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	id := f.AccountID
	const (
		ok   = connect.Code(0)
		perm = connect.CodePermissionDenied
		nf   = connect.CodeNotFound
		fp   = connect.CodeFailedPrecondition
	)
	type call func(c accountsv1connect.CreditServiceClient) error
	calls := []struct {
		name string
		fn   call
		want map[string]connect.Code
	}{
		{"GetBalance", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.GetBalance(ctx(), connect.NewRequest(&accountsv1.GetBalanceRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": ok, "outsider": nf}},
		{"ListLedgerEntries", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": ok, "outsider": nf}},
		{"GetSubscription", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.GetSubscription(ctx(), connect.NewRequest(&accountsv1.GetSubscriptionRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": ok, "outsider": nf}},
		{"GetAutoTopUpSettings", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.GetAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.GetAutoTopUpSettingsRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": ok, "outsider": nf}},
		{"GetAlertSettings", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.GetAlertSettings(ctx(), connect.NewRequest(&accountsv1.GetAlertSettingsRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": ok, "outsider": nf}},
		{"UpdateAlertSettings", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.UpdateAlertSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAlertSettingsRequest{
				BillingAccountId: id, Settings: &accountsv1.AlertSettings{LowBalanceThresholdUsdMicros: 5 * usd}}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": perm, "outsider": nf}},
		{"UpdateAutoTopUpSettings", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{
				BillingAccountId: id, Settings: &accountsv1.AutoTopUpSettings{Enabled: false}}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": perm, "outsider": nf}},
		{"ListInvoices", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.ListInvoices(ctx(), connect.NewRequest(&accountsv1.ListInvoicesRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": ok, "admin": ok, "viewer": perm, "outsider": nf}},
		{"GetInvoiceUrl", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.GetInvoiceUrl(ctx(), connect.NewRequest(&accountsv1.GetInvoiceUrlRequest{BillingAccountId: id, PaddleTransactionId: "txn_none"}))
			return err
		}, map[string]connect.Code{"owner": nf, "admin": nf, "viewer": perm, "outsider": nf}},
		{"CancelSubscription", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.CancelSubscription(ctx(), connect.NewRequest(&accountsv1.CancelSubscriptionRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": fp, "admin": fp, "viewer": perm, "outsider": nf}},
		{"ResumeSubscription", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.ResumeSubscription(ctx(), connect.NewRequest(&accountsv1.ResumeSubscriptionRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"owner": fp, "admin": fp, "viewer": perm, "outsider": nf}},
		{"CreateTopUp", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.CreateTopUp(ctx(), connect.NewRequest(&accountsv1.CreateTopUpRequest{
				BillingAccountId: id, IdempotencyKey: newKey(t), AmountUsdMicros: 25 * usd}))
			return err
		}, map[string]connect.Code{"owner": fp, "admin": fp, "viewer": perm, "outsider": nf}},
		{"StartSubscription", func(c accountsv1connect.CreditServiceClient) error {
			_, err := c.StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: id}))
			return err
		}, map[string]connect.Code{"viewer": perm, "outsider": nf, "owner": ok, "admin": ok}},
	}
	for _, c := range calls {
		for _, role := range []string{"viewer", "outsider", "owner", "admin"} {
			want, ok := c.want[role]
			if !ok {
				continue
			}
			t.Run(c.name+"/"+role, func(t *testing.T) {
				wantCode(t, c.fn(env.Credit(f.person(role).ID)), want)
			})
		}
	}
	// A deleted or unknown billing account is not_found, and a malformed ID
	// is invalid_argument.
	_, err := env.Credit(f.Owner.ID).GetBalance(ctx(), connect.NewRequest(&accountsv1.GetBalanceRequest{BillingAccountId: newKey(t)}))
	wantCode(t, err, nf)
	_, err = env.Credit(f.Owner.ID).GetBalance(ctx(), connect.NewRequest(&accountsv1.GetBalanceRequest{BillingAccountId: "x"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestSubscriptionLifecycle(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	owner := env.Credit(f.Owner.ID)
	get := func() *accountsv1.Subscription {
		resp, err := env.Credit(f.Viewer.ID).GetSubscription(ctx(), connect.NewRequest(&accountsv1.GetSubscriptionRequest{BillingAccountId: f.AccountID}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetSubscription()
	}
	if s := get(); s.GetStatus() != accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_NONE || s.GetPriceUsdMicros() != 10*usd {
		t.Fatalf("new subscription = %v", s)
	}
	first, err := owner.StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.Credit(f.Admin.ID).StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil || second.Msg.GetPaddleTransactionId() != first.Msg.GetPaddleTransactionId() {
		t.Fatalf("retry returned %v, %v", second, err)
	}
	if s := get(); s.GetStatus() != accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PENDING || s.GetCurrentPeriodEnd() != nil {
		t.Fatalf("pending subscription = %v", s)
	}
	checkout := env.Paddle.Requests()[0]
	if checkout.Path != "/transactions" || checkout.Body["custom_data"].(map[string]any)["billing_account_id"] != f.AccountID {
		t.Fatalf("checkout request = %+v", checkout)
	}

	// An unsigned or wrongly signed payment adds nothing.
	data := paddletest.Transaction(paddletest.TransactionData{
		ID: first.Msg.GetPaddleTransactionId(), SubscriptionID: "sub_x", Origin: paddle.OriginWeb,
		PriceID: testenv.PaddlePriceID, SubtotalCents: 1000,
	})
	body := paddletest.Event("evt_unsigned", paddle.EventTransactionCompleted, time.Now(), data)
	if code := env.PostWebhook(body, ""); code != http.StatusUnauthorized {
		t.Fatalf("unsigned status %d", code)
	}
	if code := env.PostWebhook(body, paddle.SignatureHeaderValue("wrong", time.Now(), body)); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status %d", code)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID); b.GetBalanceUsdMicros() != 0 {
		t.Fatalf("balance after rejected webhooks = %d", b.GetBalanceUsdMicros())
	}
	// A replayed signed delivery adds the credit once.
	sig := paddle.SignatureHeaderValue(testenv.PaddleWebhookSecret, time.Now(), body)
	for range 2 {
		if code := env.PostWebhook(body, sig); code != http.StatusOK {
			t.Fatalf("signed status %d", code)
		}
	}
	b := balanceOf(t, env, f.Viewer.ID, f.AccountID)
	if b.GetBalanceUsdMicros() != 10*usd || len(b.GetCreditGrants()) != 1 ||
		b.GetCreditGrants()[0].GetSource() != accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_SUBSCRIPTION_CREDIT {
		t.Fatalf("balance = %v", b)
	}
	if s := get(); s.GetStatus() != accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE {
		t.Fatalf("subscription after the payment = %v", s)
	}
	_, err = owner.StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeAlreadyExists)

	end := time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second)
	env.Paddle.SetSubscription(paddle.Subscription{ID: "sub_x", Status: paddle.SubscriptionStatusActive,
		CurrentBillingPeriod: &paddle.Period{StartsAt: end.AddDate(0, -1, 0), EndsAt: end}, UpdatedAt: time.Now()})
	cancel, err := env.Credit(f.Admin.ID).CancelSubscription(ctx(), connect.NewRequest(&accountsv1.CancelSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	s := cancel.Msg.GetSubscription()
	if !s.GetCancelAtPeriodEnd() || s.GetStatus() != accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE ||
		!s.GetCurrentPeriodEnd().AsTime().Equal(end) {
		t.Fatalf("canceled subscription = %v", s)
	}
	calls := len(env.Paddle.Requests())
	if _, err := owner.CancelSubscription(ctx(), connect.NewRequest(&accountsv1.CancelSubscriptionRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatal(err)
	}
	if len(env.Paddle.Requests()) != calls {
		t.Fatal("a repeated cancel called Paddle")
	}
	resume, err := owner.ResumeSubscription(ctx(), connect.NewRequest(&accountsv1.ResumeSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil || resume.Msg.GetSubscription().GetCancelAtPeriodEnd() {
		t.Fatalf("resume = %v, %v", resume, err)
	}

	// Deleting the billing account cancels the Paddle subscription at once.
	if _, err := f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatal(err)
	}
	if sub, _ := env.Paddle.Subscription("sub_x"); sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription after the deletion = %s", sub.Status)
	}
}

// Deleting a billing account whose first checkout is still open cancels the
// checkout transaction at Paddle, so a late payment cannot create a
// subscription for the deleted account. A Paddle failure fails the deletion
// and leaves the account and its open checkout.
func TestDeleteBillingAccountCancelsPendingCheckout(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	started, err := env.Credit(f.Owner.ID).StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	txnID := started.Msg.GetPaddleTransactionId()
	del := func() error {
		_, err := f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
		return err
	}

	env.Paddle.Fail("PATCH /transactions/", &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
	wantCode(t, del(), connect.CodeUnavailable)
	if _, err := f.Owner.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatalf("the billing account is gone after a failed deletion: %v", err)
	}
	var checkouts int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM paddle_checkouts WHERE billing_account_id = $1 AND paddle_transaction_id = $2`,
		f.AccountID, txnID).Scan(&checkouts); err != nil || checkouts != 1 {
		t.Fatalf("checkout records = %d, %v", checkouts, err)
	}
	if txn, _ := env.Paddle.Transaction(txnID); txn.Status != paddle.TransactionStatusDraft {
		t.Fatalf("Paddle transaction after the failed deletion = %s", txn.Status)
	}

	env.Paddle.Fail("PATCH /transactions/", nil)
	injectMemberDeleteFailure(t, env)
	wantCode(t, del(), connect.CodeInternal)
	if txn, _ := env.Paddle.Transaction(txnID); txn.Status != paddle.TransactionStatusCanceled {
		t.Fatalf("Paddle transaction after the rollback = %s, want canceled", txn.Status)
	}
	if _, err := env.DB.Pool.Exec(ctx(), `DROP TRIGGER fail_member_delete ON billing_account_members`); err != nil {
		t.Fatal(err)
	}

	env.Paddle.Fail("GET /transactions/", &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
	wantCode(t, del(), connect.CodeUnavailable)
	wantCancellationLookupLog(t, env, "get_checkout_transaction", f.AccountID, "paddle_transaction_id", txnID)
	if _, err := f.Owner.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatalf("billing account after the lookup failed: %v", err)
	}
	sub, err := billing.LoadSubscription(ctx(), env.DB.Pool, f.AccountID, false)
	if err != nil || sub.Status != billing.SubscriptionPending || sub.CheckoutTransactionID != txnID {
		t.Fatalf("local subscription = %+v, %v, want pending checkout %q", sub, err, txnID)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM paddle_checkouts WHERE billing_account_id = $1 AND paddle_transaction_id = $2`,
		f.AccountID, txnID).Scan(&checkouts); err != nil || checkouts != 1 {
		t.Fatalf("checkout records after the lookup failed = %d, %v", checkouts, err)
	}
	if n := auditRowCount(t, env, f.AccountID, api.AuditBillingAccountDeleted); n != 0 {
		t.Fatalf("deletion audits after the lookup failed = %d, want 0", n)
	}

	env.Paddle.Fail("GET /transactions/", nil)
	wantCode(t, del(), 0)
	if n := auditRowCount(t, env, f.AccountID, api.AuditBillingAccountDeleted); n != 1 {
		t.Fatalf("deletion audits after the retry = %d, want 1", n)
	}
}

func wantCancellationLookupLog(t *testing.T, env *testenv.Env, operation, accountID, paddleIDField, paddleID string) {
	t.Helper()
	want := fmt.Sprintf(`"msg":"billing_paddle_call_failed","operation":%q,"billing_account_id":%q,%q:%q`, operation, accountID, paddleIDField, paddleID)
	for line := range strings.SplitSeq(env.Logs.String(), "\n") {
		if strings.Contains(line, want) {
			if strings.Contains(line, "fake failure") || strings.Contains(line, paddletest.APIKey) {
				t.Fatal("lookup failure log includes Paddle response detail or credentials")
			}
			return
		}
	}
	t.Fatalf("no Paddle failure log for %s", operation)
}

func injectMemberDeleteFailure(t *testing.T, env *testenv.Env) {
	t.Helper()
	// The member rows are deleted after the cancellation: fail that write.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_member_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`,
		`CREATE TRIGGER fail_member_delete BEFORE DELETE ON billing_account_members FOR EACH ROW EXECUTE FUNCTION fail_member_delete()`,
	} {
		if _, err := env.DB.Pool.Exec(ctx(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartSubscriptionReplacesCanceledCheckoutAfterDeleteRollback(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	owner := env.Credit(f.Owner.ID)
	admin := env.Credit(f.Admin.ID)
	start := func(client accountsv1connect.CreditServiceClient) (string, error) {
		resp, err := client.StartSubscription(ctx(), connect.NewRequest(&accountsv1.StartSubscriptionRequest{BillingAccountId: f.AccountID}))
		if err != nil {
			return "", err
		}
		if resp.Msg.GetSubscription().GetStatus() != accountsv1.SubscriptionStatus_SUBSCRIPTION_STATUS_PENDING {
			return "", fmt.Errorf("StartSubscription returned %v, want pending", resp.Msg.GetSubscription())
		}
		return resp.Msg.GetPaddleTransactionId(), nil
	}
	original, err := start(owner)
	if err != nil || original == "" {
		t.Fatalf("first start = %q, %v", original, err)
	}

	// Queue both RPCs at the billing account lock before allowing either to proceed.
	concurrentStarts := func() string {
		release := holdAccountRow(t, env, f.AccountID)
		type result struct {
			id  string
			err error
		}
		results := make(chan result, 2)
		for _, client := range []accountsv1connect.CreditServiceClient{owner, admin} {
			go func() {
				id, err := start(client)
				results <- result{id, err}
			}()
		}
		waitRowLockWaiters(t, env, 2)
		release()

		first, second := <-results, <-results
		if first.err != nil || second.err != nil || first.id == "" || first.id != second.id {
			t.Fatalf("concurrent starts = %+v, %+v, want the same pending checkout", first, second)
		}
		return first.id
	}
	if got := concurrentStarts(); got != original {
		t.Fatalf("concurrent starts with a payable checkout = %q, want %q", got, original)
	}

	injectMemberDeleteFailure(t, env)
	_, err = f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeInternal)
	if _, err := f.Owner.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatalf("billing account after rollback: %v", err)
	}
	if txn, _ := env.Paddle.Transaction(original); txn.Status != paddle.TransactionStatusCanceled {
		t.Fatalf("Paddle transaction after rollback = %q, want canceled", txn.Status)
	}
	requirePending := func(id string) {
		t.Helper()
		sub, err := billing.LoadSubscription(ctx(), env.DB.Pool, f.AccountID, false)
		if err != nil || sub.Status != billing.SubscriptionPending || sub.CheckoutTransactionID != id {
			t.Fatalf("local subscription = %+v, %v, want pending checkout %q", sub, err, id)
		}
	}
	requirePending(original)
	if _, err := env.DB.Pool.Exec(ctx(), `DROP TRIGGER fail_member_delete ON billing_account_members`); err != nil {
		t.Fatal(err)
	}

	replacement := concurrentStarts()
	if replacement == original {
		t.Fatalf("StartSubscription reused canceled checkout %q", original)
	}
	if txn, _ := env.Paddle.Transaction(replacement); txn.Status != paddle.TransactionStatusDraft {
		t.Fatalf("replacement transaction at Paddle = %q, want draft", txn.Status)
	}
	requirePending(replacement)
	var mappings int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM paddle_checkouts
		WHERE billing_account_id = $1 AND paddle_transaction_id IN ($2, $3)`, f.AccountID, original, replacement).Scan(&mappings); err != nil || mappings != 2 {
		t.Fatalf("checkout mappings = %d, %v, want both original and replacement", mappings, err)
	}
	if id, err := start(owner); err != nil || id != replacement {
		t.Fatalf("repeat start = %q, %v, want %q", id, err, replacement)
	}
	var creates int
	for _, req := range env.Paddle.Requests() {
		if req.Method == http.MethodPost && req.Path == "/transactions" {
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("Paddle create requests = %d, want one original and one replacement", creates)
	}

	if code := env.Webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: replacement, SubscriptionID: env.Paddle.NewID("sub"), Origin: paddle.OriginWeb,
		PriceID: testenv.PaddlePriceID, SubtotalCents: 1000,
	})); code != http.StatusOK {
		t.Fatalf("replacement payment webhook status = %d, want 200", code)
	}
	sub, err := billing.LoadSubscription(ctx(), env.DB.Pool, f.AccountID, false)
	if err != nil || sub.Status != billing.SubscriptionActive {
		t.Fatalf("local subscription after verified payment = %+v, %v, want active", sub, err)
	}
}

// Paddle cancels the subscription, then a later write of DeleteBillingAccount
// fails and the transaction rolls back. The account stays, and the retry
// completes the deletion after it reads the canceled subscription at Paddle,
// with one cancellation request that Paddle accepted and one forfeiture.
func TestDeleteBillingAccountRetryAfterRollback(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subID := subscribe(t, env, f)
	del := func() error {
		_, err := f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
		return err
	}
	injectMemberDeleteFailure(t, env)
	wantCode(t, del(), connect.CodeInternal)
	if sub, _ := env.Paddle.Subscription(subID); sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription after the failed deletion = %s, want canceled", sub.Status)
	}
	if _, err := f.Owner.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
		t.Fatalf("the billing account is gone after the rolled-back deletion: %v", err)
	}
	var status string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT status FROM subscriptions WHERE billing_account_id = $1`, f.AccountID).Scan(&status); err != nil || status == "canceled" {
		t.Fatalf("local subscription after the rollback = %q, %v; want it unchanged", status, err)
	}
	var expiries int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM ledger_entries WHERE billing_account_id = $1 AND kind = 'expiry'`, f.AccountID).Scan(&expiries); err != nil || expiries != 0 {
		t.Fatalf("EXPIRY entries after the rollback = %d, %v", expiries, err)
	}

	if _, err := env.DB.Pool.Exec(ctx(), `DROP TRIGGER fail_member_delete ON billing_account_members`); err != nil {
		t.Fatal(err)
	}
	env.Paddle.Fail("GET /subscriptions/", &paddletest.Failure{Status: http.StatusBadRequest, Code: "bad_request"})
	wantCode(t, del(), connect.CodeUnavailable)
	wantCancellationLookupLog(t, env, "get_subscription", f.AccountID, "paddle_subscription_id", subID)
	if got := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); got != 10*usd {
		t.Fatalf("balance after the lookup failed = %d, want %d", got, 10*usd)
	}
	sub, err := billing.LoadSubscription(ctx(), env.DB.Pool, f.AccountID, false)
	if err != nil || sub.Status != billing.SubscriptionActive || sub.PaddleSubscriptionID != subID {
		t.Fatalf("local subscription = %+v, %v, want active subscription %q", sub, err, subID)
	}
	if n := auditRowCount(t, env, f.AccountID, api.AuditBillingAccountDeleted); n != 0 {
		t.Fatalf("deletion audits after the lookup failed = %d, want 0", n)
	}

	env.Paddle.Fail("GET /subscriptions/", nil)
	wantCode(t, del(), 0)
	if n := auditRowCount(t, env, f.AccountID, api.AuditBillingAccountDeleted); n != 1 {
		t.Fatalf("deletion audits after the retry = %d, want 1", n)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT status FROM subscriptions WHERE billing_account_id = $1`, f.AccountID).Scan(&status); err != nil || status != "canceled" {
		t.Fatalf("local subscription after the retry = %q, %v", status, err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM ledger_entries WHERE billing_account_id = $1 AND kind = 'expiry'`, f.AccountID).Scan(&expiries); err != nil || expiries != 1 {
		t.Fatalf("EXPIRY entries after the retry = %d, %v; want the credit forfeited once", expiries, err)
	}
}

// A 2xx answer of Paddle that does not confirm the cancellation (here an empty
// entity and another subscription) fails DeleteBillingAccount with unavailable
// and leaves the account, the subscription and the credit as they were. The
// retry after a confirmed answer completes the deletion.
func TestDeleteBillingAccountRequiresConfirmedCancellation(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subID := subscribe(t, env, f)
	before := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros()
	del := func() error {
		_, err := f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
		return err
	}
	for name, body := range map[string]string{
		"empty entity":       `{"data":{}}`,
		"other subscription": `{"data":{"id":"sub_other","status":"canceled"}}`,
		"not canceled":       `{"data":{"id":"` + subID + `","status":"active"}}`,
	} {
		env.Paddle.Respond("POST /subscriptions/", http.StatusOK, body)
		wantCode(t, del(), connect.CodeUnavailable)
		if _, err := f.Owner.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID})); err != nil {
			t.Fatalf("%s: the billing account is gone after a failed deletion: %v", name, err)
		}
		if got := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); got != before || before == 0 {
			t.Fatalf("%s: balance = %d, want %d (non-zero)", name, got, before)
		}
		var status string
		if err := env.DB.Pool.QueryRow(ctx(), `SELECT status FROM subscriptions WHERE billing_account_id = $1`, f.AccountID).Scan(&status); err != nil || status == "canceled" {
			t.Fatalf("%s: local subscription = %q, %v; want it unchanged", name, status, err)
		}
	}
	env.Paddle.Respond("POST /subscriptions/", 0, "")
	wantCode(t, del(), 0)
	if sub, _ := env.Paddle.Subscription(subID); sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription after the deletion = %s", sub.Status)
	}
}

func TestManualTopUp(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	admin := env.Credit(f.Admin.ID)
	topUp := func(key string, amount int64) error {
		_, err := admin.CreateTopUp(ctx(), connect.NewRequest(&accountsv1.CreateTopUpRequest{
			BillingAccountId: f.AccountID, IdempotencyKey: key, AmountUsdMicros: amount,
		}))
		return err
	}
	for name, amount := range map[string]int64{
		"below the minimum": 25*usd - 10_000,
		"above the maximum": 10_000*usd + 10_000,
		"sub-cent":          25*usd + 1,
		"zero":              0,
	} {
		t.Run(name, func(t *testing.T) { wantCode(t, topUp(newKey(t), amount), connect.CodeInvalidArgument) })
	}
	wantCode(t, topUp("not-a-uuid", 25*usd), connect.CodeInvalidArgument)
	wantCode(t, topUp(newKey(t), 25*usd), connect.CodeFailedPrecondition)

	subscribe(t, env, f)
	key := newKey(t)
	if err := topUp(key, 25*usd); err != nil {
		t.Fatal(err)
	}
	if err := topUp(key, 25*usd); err != nil {
		t.Fatalf("retry: %v", err)
	}
	wantCode(t, topUp(key, 30*usd), connect.CodeInvalidArgument)
	charges := env.Paddle.Charges()
	if len(charges) != 1 || paddletest.ChargeAmountCents(charges[0]) != "2500" || charges[0].Body["effective_from"] != "immediately" {
		t.Fatalf("charges = %+v", charges)
	}
	// The credit arrives only with the verified webhook.
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID); b.GetBalanceUsdMicros() != 10*usd {
		t.Fatalf("balance before the webhook = %d", b.GetBalanceUsdMicros())
	}
	txnID := env.Paddle.NewID("txn")
	if code := env.Webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, Origin: paddle.OriginSubscriptionCharge, TopUpID: paddletest.ChargeTopUpID(charges[0]),
		SubtotalCents: 2500, TaxCents: 250,
	})); code != http.StatusOK {
		t.Fatal(code)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID); b.GetBalanceUsdMicros() != 35*usd {
		t.Fatalf("balance after the webhook = %d", b.GetBalanceUsdMicros())
	}
	if kinds := ledgerKinds(t, env, f.Viewer.ID, f.AccountID); strings.Join(kinds, ",") != "MANUAL_TOP_UP,SUBSCRIPTION_CREDIT" {
		t.Fatalf("ledger = %v", kinds)
	}

	// Paddle rejects the charge: failed_precondition; Paddle fails:
	// unavailable.
	env.Paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})
	declined := newKey(t)
	wantCode(t, topUp(declined, 25*usd), connect.CodeFailedPrecondition)
	env.Paddle.FailCharges(nil)
	wantCode(t, topUp(declined, 25*usd), connect.CodeFailedPrecondition)
	env.Paddle.FailCharges(&paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "service_unavailable"})
	wantCode(t, topUp(newKey(t), 25*usd), connect.CodeUnavailable)
	env.Paddle.FailCharges(&paddletest.Failure{Status: http.StatusTooManyRequests, Code: "too_many_requests"})
	limited := newKey(t)
	wantCode(t, topUp(limited, 25*usd), connect.CodeUnavailable)
	// Nothing was charged, so the same key may charge again.
	env.Paddle.FailCharges(nil)
	if err := topUp(limited, 25*usd); err != nil {
		t.Fatalf("retry after a rate limit: %v", err)
	}

	// Invoices list the purchases, newest first, and link to Paddle.
	owner := env.Credit(f.Owner.ID)
	inv, err := owner.ListInvoices(ctx(), connect.NewRequest(&accountsv1.ListInvoicesRequest{BillingAccountId: f.AccountID, PageSize: 1}))
	if err != nil || len(inv.Msg.GetInvoices()) != 1 || inv.Msg.GetInvoices()[0].GetPaddleTransactionId() != txnID ||
		inv.Msg.GetInvoices()[0].GetAmountUsdMicros() != 25*usd || inv.Msg.GetNextPageToken() == "" {
		t.Fatalf("ListInvoices = %v, %v", inv, err)
	}
	next, err := owner.ListInvoices(ctx(), connect.NewRequest(&accountsv1.ListInvoicesRequest{
		BillingAccountId: f.AccountID, PageSize: 1, PageToken: inv.Msg.GetNextPageToken()}))
	if err != nil || len(next.Msg.GetInvoices()) != 1 ||
		next.Msg.GetInvoices()[0].GetKind() != accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_SUBSCRIPTION_CREDIT || next.Msg.GetNextPageToken() != "" {
		t.Fatalf("second page = %v, %v", next, err)
	}
	link, err := owner.GetInvoiceUrl(ctx(), connect.NewRequest(&accountsv1.GetInvoiceUrlRequest{BillingAccountId: f.AccountID, PaddleTransactionId: txnID}))
	if err != nil || !strings.Contains(link.Msg.GetInvoiceUrl(), txnID) {
		t.Fatalf("GetInvoiceUrl = %v, %v", link, err)
	}
	_, err = owner.GetInvoiceUrl(ctx(), connect.NewRequest(&accountsv1.GetInvoiceUrlRequest{BillingAccountId: f.AccountID, PaddleTransactionId: "TXN-1"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	other := createAccount(t, f.Outsider, "Other")
	_, err = env.Credit(f.Outsider.ID).GetInvoiceUrl(ctx(), connect.NewRequest(&accountsv1.GetInvoiceUrlRequest{
		BillingAccountId: other.GetBillingAccountId(), PaddleTransactionId: txnID}))
	wantCode(t, err, connect.CodeNotFound)
	env.Paddle.Fail("GET /transactions/", &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
	_, err = owner.GetInvoiceUrl(ctx(), connect.NewRequest(&accountsv1.GetInvoiceUrlRequest{BillingAccountId: f.AccountID, PaddleTransactionId: txnID}))
	wantCode(t, err, connect.CodeUnavailable)

	// The invoice link and the Paddle API key never reach the logs.
	logs := env.Logs.String()
	if strings.Contains(logs, "token=secret") || strings.Contains(logs, paddletest.APIKey) || strings.Contains(logs, testenv.PaddleWebhookSecret) {
		t.Fatal("logs hold the invoice link or a Paddle secret")
	}
}

func TestRefundAndSettings(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	owner := env.Credit(f.Owner.ID)

	auto, err := owner.GetAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.GetAutoTopUpSettingsRequest{BillingAccountId: f.AccountID}))
	if err != nil || auto.Msg.GetSettings().GetEnabled() {
		t.Fatalf("default automatic top-up = %v, %v", auto, err)
	}
	for name, s := range map[string]*accountsv1.AutoTopUpSettings{
		"missing":       nil,
		"low threshold": {Enabled: true, ThresholdUsdMicros: 9 * usd, AmountUsdMicros: 25 * usd, MonthlyCapUsdMicros: 25 * usd},
		"low amount":    {Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 24 * usd, MonthlyCapUsdMicros: 25 * usd},
		"missing cap":   {Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd},
		"high amount":   {Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 10_000*usd + 10_000, MonthlyCapUsdMicros: 20_000 * usd},
	} {
		_, err := owner.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{BillingAccountId: f.AccountID, Settings: s}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	updated, err := owner.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{
		BillingAccountId: f.AccountID,
		Settings: &accountsv1.AutoTopUpSettings{Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd,
			MonthlyCapUsdMicros: 100 * usd, ConsecutiveFailures: 7, MonthToDateUsdMicros: 9},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s := updated.Msg.GetSettings(); !s.GetEnabled() || s.GetMonthlyCapUsdMicros() != 100*usd || s.GetConsecutiveFailures() != 0 || s.GetMonthToDateUsdMicros() != 0 {
		t.Fatalf("updated settings = %v", s)
	}
	// Exactly 10,000 USD is accepted when enabled; a larger whole-cent
	// amount is kept while the setting is disabled.
	for name, in := range map[string]*accountsv1.AutoTopUpSettings{
		"maximum amount":        {Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 10_000 * usd, MonthlyCapUsdMicros: 20_000 * usd},
		"disabled large amount": {Enabled: false, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 15_000 * usd, MonthlyCapUsdMicros: 20_000 * usd},
	} {
		res, err := owner.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{BillingAccountId: f.AccountID, Settings: in}))
		if err != nil || res.Msg.GetSettings().GetEnabled() != in.GetEnabled() || res.Msg.GetSettings().GetAmountUsdMicros() != in.GetAmountUsdMicros() {
			t.Errorf("%s: %v, %v", name, res, err)
		}
	}

	alert, err := owner.GetAlertSettings(ctx(), connect.NewRequest(&accountsv1.GetAlertSettingsRequest{BillingAccountId: f.AccountID}))
	if err != nil || alert.Msg.GetSettings().GetLowBalanceThresholdUsdMicros() != 10*usd {
		t.Fatalf("default alert = %v, %v", alert, err)
	}
	_, err = owner.UpdateAlertSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAlertSettingsRequest{
		BillingAccountId: f.AccountID, Settings: &accountsv1.AlertSettings{LowBalanceThresholdUsdMicros: -10_000}}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// A refund in the Paddle dashboard removes the credit; the balance can
	// go negative, which blocks the deletion.
	var txnID string
	for _, e := range mustLedger(t, env, f) {
		txnID = e.GetPaddleTransactionId()
	}
	// The usage-report API deducts through billing.Deduct.
	if err := env.DB.InTx(ctx(), func(tx pgx.Tx) error {
		_, err := env.Billing.Deduct(ctx(), tx, billing.Deduction{BillingAccountID: f.AccountID, AmountUSDMicros: 4 * usd})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The refund of 15 USD is capped at the 10 USD credit of the purchase.
	if code := env.Webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_1", "refund", "approved", txnID, 1500)); code != http.StatusOK {
		t.Fatal(code)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID); b.GetBalanceUsdMicros() != -4*usd || len(b.GetCreditGrants()) != 0 {
		t.Fatalf("balance after the refund = %v", b)
	}
	entries := mustLedger(t, env, f)
	if entries[0].GetKind() != accountsv1.LedgerEntryKind_LEDGER_ENTRY_KIND_REFUND || entries[0].GetAmountUsdMicros() != -10*usd ||
		entries[0].GetBalanceAfterUsdMicros() != -4*usd || entries[0].GetPaddleTransactionId() != txnID {
		t.Fatalf("refund entry = %v", entries[0])
	}
	_, err = f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

func mustLedger(t *testing.T, env *testenv.Env, f fixture) []*accountsv1.LedgerEntry {
	t.Helper()
	resp, err := env.Credit(f.Owner.ID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetLedgerEntries()
}
