// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

const (
	usd      = MicrosPerUSD
	priceID  = "pri_subscription"
	product  = "pro_topup"
	whSecret = "pdl_ntfset_secret"
)

// testClock is a settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type fixture struct {
	t      *testing.T
	s      *Service
	db     *db.DB
	clock  *testClock
	paddle *paddletest.Server
	email  *email.Fake
	dir    *directory.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database := dbtest.New(t)
	f := &fixture{
		t:      t,
		db:     database,
		clock:  &testClock{now: time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)},
		paddle: paddletest.New(t),
		email:  email.NewFake(),
		dir:    directory.NewFake(),
	}
	f.s = New(Config{
		DB:                  database,
		Paddle:              paddle.New(paddle.Config{BaseURL: f.paddle.URL, APIKey: paddletest.APIKey}),
		Directory:           f.dir,
		Email:               f.email,
		Logger:              slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:                 f.clock.Now,
		WebhookSecret:       whSecret,
		SubscriptionPriceID: priceID,
		TopUpProductID:      product,
		WebOrigin:           "https://accounts.example.test",
	})
	return f
}

func ctx() context.Context { return context.Background() }

var accountSeq atomic.Int64

// account creates a billing account with an owner, an admin and a viewer
// that have primary emails.
func (f *fixture) account() string {
	f.t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		f.t.Fatal(err)
	}
	n := accountSeq.Add(1)
	now := f.clock.Now()
	_, err = f.db.Pool.Exec(ctx(), `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time)
		VALUES ($1, $2, $3, $3)`, id, fmt.Sprintf("Account %d", n), now)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, role := range []string{"owner", "admin", "viewer"} {
		user := fmt.Sprintf("%s-%d", role, n)
		if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO billing_account_members (billing_account_id, user_id, role, create_time)
			VALUES ($1, $2, $3, $4)`, id, user, role, now); err != nil {
			f.t.Fatal(err)
		}
		f.dir.Set(user, directory.User{PrimaryEmail: user + "@example.com"})
	}
	return id
}

func (f *fixture) tx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := f.db.InTx(ctx(), fn); err != nil {
		f.t.Fatal(err)
	}
}

var txnSeq atomic.Int64

func (f *fixture) grant(accountID string, kind LedgerKind, amount int64) string {
	f.t.Helper()
	var id string
	f.tx(func(tx pgx.Tx) error {
		acct, err := lockAccount(ctx(), tx, accountID)
		if err != nil {
			return err
		}
		id, _, err = f.s.addGrant(ctx(), tx, acct, grantInput{
			Kind: kind, Amount: amount, PaddleTransactionID: fmt.Sprintf("txn_test_%d", txnSeq.Add(1)),
		}, f.s.timestamp())
		return err
	})
	return id
}

func (f *fixture) deduct(accountID string, amount int64) DeductionResult {
	f.t.Helper()
	var res DeductionResult
	f.tx(func(tx pgx.Tx) error {
		var err error
		res, err = f.s.Deduct(ctx(), tx, Deduction{
			BillingAccountID: accountID, AmountUSDMicros: amount, Service: "ownsand", OrganizationID: "org",
			Meter: "ownsand.relay_egress", WindowStart: f.clock.Now().Truncate(5 * time.Minute), Quantity: 1,
		})
		return err
	})
	return res
}

func (f *fixture) balance(accountID string) int64 {
	f.t.Helper()
	var b int64
	if err := f.db.Pool.QueryRow(ctx(), `SELECT balance_usd_micros FROM billing_accounts WHERE billing_account_id = $1`,
		accountID).Scan(&b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) remaining(grantID string) int64 {
	f.t.Helper()
	var r int64
	if err := f.db.Pool.QueryRow(ctx(), `SELECT remaining_usd_micros FROM credit_grants WHERE credit_grant_id = $1`,
		grantID).Scan(&r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// ledger returns the entries oldest first.
func (f *fixture) ledger(accountID string) []LedgerEntry {
	f.t.Helper()
	entries, err := ListLedger(ctx(), f.db.Pool, accountID, "", 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// checkInvariant checks that the balance equals the remaining credit while it
// is not negative, and that no grant has credit while it is negative.
func (f *fixture) checkInvariant(accountID string) {
	f.t.Helper()
	var sum int64
	if err := f.db.Pool.QueryRow(ctx(), `SELECT COALESCE(SUM(remaining_usd_micros), 0) FROM credit_grants
		WHERE billing_account_id = $1`, accountID).Scan(&sum); err != nil {
		f.t.Fatal(err)
	}
	b := f.balance(accountID)
	if (b >= 0 && sum != b) || (b < 0 && sum != 0) {
		f.t.Fatalf("balance %d with remaining credit %d", b, sum)
	}
	entries := f.ledger(accountID)
	var running int64
	for _, e := range entries {
		running += e.Amount
		if e.BalanceAfter != running {
			f.t.Fatalf("entry %s balance_after %d, running sum %d", e.ID, e.BalanceAfter, running)
		}
	}
	if running != b {
		f.t.Fatalf("ledger sum %d, balance %d", running, b)
	}
}

type wantEntry struct {
	Kind    LedgerKind
	Amount  int64
	Balance int64
}

func (f *fixture) wantLedger(accountID string, want ...wantEntry) {
	f.t.Helper()
	entries := f.ledger(accountID)
	if len(entries) != len(want) {
		f.t.Fatalf("ledger has %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, e := range entries {
		if e.Kind != want[i].Kind || e.Amount != want[i].Amount || e.BalanceAfter != want[i].Balance {
			f.t.Errorf("entry %d = %s %d -> %d, want %s %d -> %d", i, e.Kind, e.Amount, e.BalanceAfter,
				want[i].Kind, want[i].Amount, want[i].Balance)
		}
	}
	f.checkInvariant(accountID)
}

func TestLedgerFIFOAndDebtPayoff(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	first := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	f.clock.Advance(24 * time.Hour)
	second := f.grant(acct, LedgerManualTopUp, 25*usd)

	// The first grant expires first, so usage takes it first.
	f.deduct(acct, 15*usd)
	if f.remaining(first) != 0 || f.remaining(second) != 20*usd {
		t.Fatalf("after 15 USD: first %d, second %d", f.remaining(first), f.remaining(second))
	}
	// Usage is deducted also below zero.
	res := f.deduct(acct, 30*usd)
	if res.BalanceAfterUSDMicros != -10*usd || f.remaining(second) != 0 {
		t.Fatalf("after 30 USD: balance %d, second %d", res.BalanceAfterUSDMicros, f.remaining(second))
	}
	// A new grant pays off the debt first.
	third := f.grant(acct, LedgerAutoTopUp, 25*usd)
	if f.remaining(third) != 15*usd || f.balance(acct) != 15*usd {
		t.Fatalf("after the payoff: third %d, balance %d", f.remaining(third), f.balance(acct))
	}
	// A grant smaller than the debt is used up by it.
	f.deduct(acct, 40*usd)
	fourth := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	if f.remaining(fourth) != 0 || f.balance(acct) != -15*usd {
		t.Fatalf("partial payoff: fourth %d, balance %d", f.remaining(fourth), f.balance(acct))
	}
	// A zero deduction writes an entry and changes nothing.
	f.deduct(acct, 0)
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerManualTopUp, 25 * usd, 35 * usd},
		wantEntry{LedgerUsage, -15 * usd, 20 * usd},
		wantEntry{LedgerUsage, -30 * usd, -10 * usd},
		wantEntry{LedgerAutoTopUp, 25 * usd, 15 * usd},
		wantEntry{LedgerUsage, -40 * usd, -25 * usd},
		wantEntry{LedgerSubscriptionCredit, 10 * usd, -15 * usd},
		wantEntry{LedgerUsage, 0, -15 * usd},
	)
	usage := f.ledger(acct)[2]
	if usage.Service == nil || *usage.Service != "ownsand" || usage.Meter == nil || *usage.Meter != "ownsand.relay_egress" ||
		usage.WindowStart == nil || usage.Quantity != 1 || usage.CreditGrantID != nil {
		t.Fatalf("usage entry fields: %+v", usage)
	}
}

func TestLedgerConcurrentDeductions(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.grant(acct, LedgerManualTopUp, 10*usd)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.db.InTx(ctx(), func(tx pgx.Tx) error {
				_, err := f.s.Deduct(ctx(), tx, Deduction{BillingAccountID: acct, AmountUSDMicros: usd})
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.balance(acct) != -10*usd {
		t.Fatalf("balance %d, want -10 USD", f.balance(acct))
	}
	f.checkInvariant(acct)
}

func TestLedgerExpiry(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	old := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	f.clock.Advance(30 * 24 * time.Hour)
	young := f.grant(acct, LedgerManualTopUp, 25*usd)
	f.deduct(acct, 4*usd)

	// 7 days before the expiry the owners and admins get one email.
	f.clock.Set(GrantExpireTime(f.clock.Now().Add(-30 * 24 * time.Hour)).Add(-6 * 24 * time.Hour))
	for range 2 {
		if err := f.s.QueueExpiryNotices(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	msgs := f.email.Messages()
	if len(msgs) != 2 || msgs[0].Kind != email.KindCreditExpiry {
		t.Fatalf("expiry notices = %+v", msgs)
	}
	for _, m := range msgs {
		if m.To == "" || m.To[:6] == "viewer" {
			t.Fatalf("expiry notice to %q", m.To)
		}
	}

	// At the expiry the rest leaves the balance as an EXPIRY entry.
	f.clock.Advance(6*24*time.Hour + time.Second)
	if err := f.s.ExpireGrants(ctx()); err != nil {
		t.Fatal(err)
	}
	if f.remaining(old) != 0 || f.remaining(young) != 25*usd {
		t.Fatalf("after the expiry: old %d, young %d", f.remaining(old), f.remaining(young))
	}
	// The job is idempotent.
	if err := f.s.ExpireGrants(ctx()); err != nil {
		t.Fatal(err)
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerManualTopUp, 25 * usd, 35 * usd},
		wantEntry{LedgerUsage, -4 * usd, 31 * usd},
		wantEntry{LedgerExpiry, -6 * usd, 25 * usd},
	)
	if e := f.ledger(acct)[3]; e.CreditGrantID == nil || *e.CreditGrantID != old || e.PaddleTransactionID != nil {
		t.Fatalf("expiry entry %+v", e)
	}

	// A deduction never consumes expired credit, even before the job ran.
	f.clock.Set(GrantExpireTime(f.clock.Now()).Add(48 * time.Hour))
	f.deduct(acct, usd)
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerManualTopUp, 25 * usd, 35 * usd},
		wantEntry{LedgerUsage, -4 * usd, 31 * usd},
		wantEntry{LedgerExpiry, -6 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
		wantEntry{LedgerUsage, -usd, -usd},
	)
}

func TestBalanceHidesDueExpiry(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	f.clock.Advance(24 * time.Hour)
	f.grant(acct, LedgerManualTopUp, 25*usd)
	balance, grants, err := Balance(ctx(), f.db.Pool, acct, GrantExpireTime(f.clock.Now().Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if balance != 25*usd || len(grants) != 1 || grants[0].Source != LedgerManualTopUp {
		t.Fatalf("balance %d, grants %+v", balance, grants)
	}
}

func TestRefundMakesBalanceNegative(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	first := f.grant(acct, LedgerManualTopUp, 25*usd)
	f.clock.Advance(time.Hour)
	second := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	f.deduct(acct, 20*usd)
	// The refunded grant gives its rest first, then the other grants, then
	// the balance goes negative.
	f.tx(func(tx pgx.Tx) error {
		a, err := lockAccount(ctx(), tx, acct)
		if err != nil {
			return err
		}
		_, err = f.s.removeCredit(ctx(), tx, a, first, "txn_refund", 25*usd, f.s.timestamp())
		return err
	})
	if f.remaining(first) != 0 || f.remaining(second) != 0 || f.balance(acct) != -10*usd {
		t.Fatalf("after the refund: first %d, second %d, balance %d", f.remaining(first), f.remaining(second), f.balance(acct))
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 35 * usd},
		wantEntry{LedgerUsage, -20 * usd, 15 * usd},
		wantEntry{LedgerRefund, -25 * usd, -10 * usd},
	)
}

func TestLowBalanceAlertOncePerCrossing(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.deduct(acct, 10*usd) // 15: above the default 10 USD threshold
	f.deduct(acct, 10*usd) // 5: below
	f.deduct(acct, usd)    // 4: still below, no second alert
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.deduct(acct, 25*usd) // 4 again after reaching the threshold
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	var low int
	for _, m := range f.email.Messages() {
		if m.Kind == email.KindLowBalance {
			low++
		}
	}
	// Two crossings, two recipients (owner and admin) each.
	if low != 4 {
		t.Fatalf("low-balance emails = %d, want 4", low)
	}
}

// activate gives the billing account an active Paddle subscription.
func (f *fixture) activate(accountID string) string {
	f.t.Helper()
	subID := f.paddle.NewID("sub")
	f.paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusActive})
	f.tx(func(tx pgx.Tx) error {
		if err := recordPaddleSubscription(ctx(), tx, subID, accountID, f.s.timestamp()); err != nil {
			return err
		}
		return saveSubscription(ctx(), tx, accountID, Subscription{Status: SubscriptionActive, PaddleSubscriptionID: subID}, "", f.s.timestamp())
	})
	return subID
}

func (f *fixture) enableAutoTopUp(accountID string, threshold, amount, limit int64) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		_, err := UpdateAutoTopUp(ctx(), tx, accountID, autoSettings(true, threshold, amount, limit), f.s.timestamp())
		return err
	})
}

// webhook applies a signed event through the HTTP handler and returns the
// status.
func (f *fixture) webhook(eventType string, data any) int {
	f.t.Helper()
	body := paddletest.Event(fmt.Sprintf("evt_%d", txnSeq.Add(1)), eventType, f.clock.Now(), data)
	return f.deliver(body, paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body))
}

func (f *fixture) deliver(body []byte, signature string) int {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "/webhooks/paddle", bytes.NewReader(body))
	if signature != "" {
		req.Header.Set(paddle.SignatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	f.s.WebhookHandler().ServeHTTP(rec, req)
	return rec.Code
}

// completeCharges answers every recorded charge that has no completion yet
// with a completed transaction webhook.
func (f *fixture) completeCharge(charge paddletest.Request, cents int64) {
	f.t.Helper()
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge, TopUpID: paddletest.ChargeTopUpID(charge),
		SubtotalCents: cents, TaxCents: cents / 10,
	})); code != http.StatusOK {
		f.t.Fatalf("webhook status %d", code)
	}
}

func (f *fixture) topUpStatuses(accountID string) []string {
	f.t.Helper()
	rows, err := f.db.Pool.Query(ctx(), `SELECT status || COALESCE(':' || failure_reason, '') FROM top_ups
		WHERE billing_account_id = $1 ORDER BY create_time, top_up_id`, accountID)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestAutoTopUpMonthlyCap(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 30*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 50*usd)

	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("a deduction below the threshold queued no top-up")
	}
	// At most one is in flight.
	if res := f.deduct(acct, usd); res.AutoTopUpQueued {
		t.Fatal("a second top-up was queued while one is in flight")
	}
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	charges := f.paddle.Charges()
	if len(charges) != 1 || paddletest.ChargeAmountCents(charges[0]) != "2500" {
		t.Fatalf("charges = %+v", charges)
	}
	// The credit arrives only with the verified webhook.
	if f.balance(acct) != 4*usd {
		t.Fatalf("balance before the webhook = %d", f.balance(acct))
	}
	f.completeCharge(charges[0], 2500)
	if f.balance(acct) != 29*usd {
		t.Fatalf("balance after the webhook = %d", f.balance(acct))
	}

	if res := f.deduct(acct, 20*usd); !res.AutoTopUpQueued {
		t.Fatal("second top-up of the month not queued")
	}
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	f.completeCharge(f.paddle.Charges()[1], 2500)
	mtd, err := MonthToDate(ctx(), f.db.Pool, acct, f.clock.Now())
	if err != nil || mtd != 50*usd {
		t.Fatalf("month to date = %d, %v", mtd, err)
	}
	// A third top-up would exceed the 50 USD cap.
	if res := f.deduct(acct, 30*usd); res.AutoTopUpQueued {
		t.Fatal("a top-up over the monthly cap was queued")
	}
	// Manual top-ups do not count toward the cap, and a new UTC month starts
	// at zero.
	f.clock.Set(MonthStart(f.clock.Now()).AddDate(0, 1, 0))
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up in the new month")
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 30 * usd, 30 * usd},
		wantEntry{LedgerUsage, -25 * usd, 5 * usd},
		wantEntry{LedgerUsage, -usd, 4 * usd},
		wantEntry{LedgerAutoTopUp, 25 * usd, 29 * usd},
		wantEntry{LedgerUsage, -20 * usd, 9 * usd},
		wantEntry{LedgerAutoTopUp, 25 * usd, 34 * usd},
		wantEntry{LedgerUsage, -30 * usd, 4 * usd},
		wantEntry{LedgerUsage, -usd, 3 * usd},
	)
}

func TestAutoTopUpShutoffAfterTwoFailures(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})

	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	st, _ := loadSettings(ctx(), f.db.Pool, acct)
	if !st.AutoEnabled || st.ConsecutiveFailures != 1 {
		t.Fatalf("after one failure: %+v", st)
	}
	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	st, _ = loadSettings(ctx(), f.db.Pool, acct)
	if st.AutoEnabled || st.ConsecutiveFailures != 2 {
		t.Fatalf("after two failures: %+v", st)
	}
	if res := f.deduct(acct, usd); res.AutoTopUpQueued {
		t.Fatal("a top-up was queued after the shutoff")
	}
	if got := f.topUpStatuses(acct); len(got) != 2 || got[0] != "failed:payment_declined" || got[1] != "failed:payment_declined" {
		t.Fatalf("top-ups = %v", got)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	kinds := map[email.Kind]int{}
	for _, m := range f.email.Messages() {
		kinds[m.Kind]++
	}
	// Two failure emails and one shutoff email, each to the owner and the
	// admin. The low-balance alert is off while automatic top-up is on; the
	// last deduction after the shutoff sends it.
	if kinds[email.KindPaymentFailed] != 4 || kinds[email.KindAutoTopUpDisabled] != 2 || kinds[email.KindLowBalance] != 2 {
		t.Fatalf("emails = %v", kinds)
	}
	// Enabling again resets the count; a success resets it too.
	f.paddle.FailCharges(nil)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	st, _ = loadSettings(ctx(), f.db.Pool, acct)
	if !st.AutoEnabled || st.ConsecutiveFailures != 0 {
		t.Fatalf("after enabling: %+v", st)
	}
}

func TestAutoTopUpPastDueAndTimeout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)

	// Paddle rejects charges of a past_due subscription: a failed top-up.
	f.paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusPastDue})
	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	// A charge whose result is unknown waits for the webhook, then times out
	// and counts as the second failure.
	f.paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusActive})
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := f.topUpStatuses(acct); got[1] != "unknown" {
		t.Fatalf("top-ups = %v", got)
	}
	f.clock.Advance(TopUpTimeout + time.Minute)
	if err := f.s.TimeOutTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := f.topUpStatuses(acct); got[0] != "failed:subscription_past_due" || got[1] != "failed:timed_out" {
		t.Fatalf("top-ups = %v", got)
	}
	st, _ := loadSettings(ctx(), f.db.Pool, acct)
	if st.AutoEnabled {
		t.Fatalf("still enabled after two failures: %+v", st)
	}
	// A late webhook still adds the credit and completes the top-up.
	charges := f.paddle.Charges()
	f.completeCharge(charges[len(charges)-1], 2500)
	if got := f.topUpStatuses(acct); got[1] != "completed" || f.balance(acct) != 23*usd {
		t.Fatalf("after the late webhook: %v, balance %d", got, f.balance(acct))
	}
}

func autoSettings(enabled bool, threshold, amount, limit int64) *accountsv1.AutoTopUpSettings {
	return &accountsv1.AutoTopUpSettings{Enabled: enabled, ThresholdUsdMicros: threshold, AmountUsdMicros: amount, MonthlyCapUsdMicros: limit}
}
