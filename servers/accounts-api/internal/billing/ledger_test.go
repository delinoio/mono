// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestLedgerGrantAtMinimumBalance checks that a grant commits at the smallest
// signed balance, where negating the balance would overflow: the whole grant
// pays off debt and no credit remains.
func TestLedgerGrantAtMinimumBalance(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE billing_accounts SET balance_usd_micros = $2 WHERE billing_account_id = $1`,
		acct, int64(math.MinInt64)); err != nil {
		t.Fatal(err)
	}
	grantID := f.grant(acct, LedgerManualTopUp, 10*usd)
	if f.remaining(grantID) != 0 || f.balance(acct) != math.MinInt64+10*usd {
		t.Fatalf("after the grant: remaining %d, balance %d", f.remaining(grantID), f.balance(acct))
	}
	entries := f.ledger(acct)
	if len(entries) != 1 || entries[0].Kind != LedgerManualTopUp || entries[0].Amount != 10*usd ||
		entries[0].BalanceAfter != math.MinInt64+10*usd {
		t.Fatalf("ledger = %+v", entries)
	}
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

// remainders returns the remaining credit of the grants.
func remainders(grants []CreditGrant) []int64 {
	out := []int64{}
	for _, g := range grants {
		out = append(out, g.Remaining)
	}
	return out
}

// wantBalance checks a Balance result: the effective balance, the remaining
// credit of the grants in order and the snapshot invariant that a
// nonnegative balance equals the sum of the remainders and a negative one
// has no grants.
func wantBalance(t *testing.T, label string, balance int64, grants []CreditGrant, want int64, wantRemaining ...int64) {
	t.Helper()
	if wantRemaining == nil {
		wantRemaining = []int64{}
	}
	got := remainders(grants)
	if balance != want || !slices.Equal(got, wantRemaining) {
		t.Fatalf("%s: balance %d with grant remainders %v, want %d with %v", label, balance, got, want, wantRemaining)
	}
	var sum int64
	for _, r := range got {
		sum += r
	}
	if (balance >= 0 && sum != balance) || (balance < 0 && len(grants) != 0) {
		t.Fatalf("%s: balance %d does not match the grants %v", label, balance, got)
	}
}

// A credit-changing commit between the reads of Balance must not pair the
// balance of one state with the grants of another: both come from one
// statement and so from one snapshot. The querier commits the change after
// the first statement of Balance ran, so that a read with several
// statements would see the old balance and the new grants.
func TestBalanceIsOneSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		change        func(f *fixture, acct string, first string)
		wantBalance   int64
		wantRemaining []int64
	}{
		{"deduction of part of a grant", func(f *fixture, acct, _ string) { f.deduct(acct, 4_000_000) }, 11_000_000, []int64{6_000_000, 5_000_000}},
		{"deduction of a whole grant", func(f *fixture, acct, _ string) { f.deduct(acct, 14_000_000) }, 1_000_000, []int64{1_000_000}},
		{"new grant", func(f *fixture, acct, _ string) { f.grant(acct, LedgerManualTopUp, 25_000_000) }, 40_000_000,
			[]int64{10_000_000, 5_000_000, 25_000_000}},
		{"refund", func(f *fixture, acct, first string) {
			f.tx(func(tx pgx.Tx) error {
				a, err := lockAccount(ctx(), tx, acct)
				if err != nil {
					return err
				}
				_, err = f.s.removeCredit(ctx(), tx, a, first, "txn_refund", 4_000_000, f.s.timestamp())
				return err
			})
		}, 11_000_000, []int64{6_000_000, 5_000_000}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			first := f.grant(acct, LedgerSubscriptionCredit, 10_000_000)
			f.clock.Advance(time.Hour)
			f.grant(acct, LedgerManualTopUp, 5_000_000)
			q := &interleavingQuerier{inner: f.db.Pool, afterFirst: func() { tc.change(f, acct, first) }}
			balance, grants, err := Balance(ctx(), q, acct, f.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !q.done {
				t.Fatal("the commit did not run")
			}
			// The result is the state before the commit...
			wantBalance(t, "during the commit", balance, grants, 15_000_000, 10_000_000, 5_000_000)
			// ...and the next read is the state after it.
			balance, grants, err = Balance(ctx(), f.db.Pool, acct, f.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			wantBalance(t, "after the commit", balance, grants, tc.wantBalance, tc.wantRemaining...)
		})
	}
}

func TestBalanceWithoutUsableGrants(t *testing.T) {
	f := newFixture(t)

	// An account without grants has a zero balance and no grants.
	empty := f.account()
	balance, grants, err := Balance(ctx(), f.db.Pool, empty, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantBalance(t, "empty account", balance, grants, 0)

	// An account in debt has a negative balance and no grants.
	debt := f.account()
	grant := f.grant(debt, LedgerManualTopUp, 10_000_000)
	f.tx(func(tx pgx.Tx) error {
		a, err := lockAccount(ctx(), tx, debt)
		if err != nil {
			return err
		}
		_, err = f.s.removeCredit(ctx(), tx, a, grant, "txn_refund", 25_000_000, f.s.timestamp())
		return err
	})
	balance, grants, err = Balance(ctx(), f.db.Pool, debt, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	wantBalance(t, "debt", balance, grants, -15_000_000)

	// Credit that expired but that the expiry job has not removed yet is
	// left out of the balance and the grants.
	expired := f.account()
	f.grant(expired, LedgerSubscriptionCredit, 10_000_000)
	f.grant(expired, LedgerManualTopUp, 5_000_000)
	balance, grants, err = Balance(ctx(), f.db.Pool, expired, GrantExpireTime(f.clock.Now()).Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wantBalance(t, "all expired", balance, grants, 0)

	// An absent account is no row.
	absent, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Balance(ctx(), f.db.Pool, absent, f.clock.Now()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("absent account: error %v, want pgx.ErrNoRows", err)
	}
}

// Grants with the same expiry are consumed in ID order, and so listed.
func TestBalanceOrdersEqualExpiryByID(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	now := f.clock.Now()
	ids := []string{
		"00000000-0000-7000-8000-000000000003",
		"00000000-0000-7000-8000-000000000001",
		"00000000-0000-7000-8000-000000000002",
	}
	for i, id := range ids {
		if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO credit_grants (credit_grant_id, billing_account_id, source,
				original_usd_micros, remaining_usd_micros, paddle_transaction_id, create_time, expire_time)
			VALUES ($1, $2, 'manual_top_up', $3, $3, $4, $5, $6)`,
			id, acct, int64(i+1)*usd, fmt.Sprintf("txn_order_%d", i), now, GrantExpireTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	_, grants, err := Balance(ctx(), f.db.Pool, acct, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range grants {
		got = append(got, g.ID)
	}
	want := []string{ids[1], ids[2], ids[0]}
	if !slices.Equal(got, want) {
		t.Fatalf("grant order %v, want %v", got, want)
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

// lowBalanceState returns the number of queued low-balance notifications and
// the low_balance_alerted flag of the billing account.
func (f *fixture) lowBalanceState(accountID string) (queued int64, alerted bool) {
	f.t.Helper()
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications
		WHERE billing_account_id = $1 AND kind = $2`, accountID, string(NotifyLowBalance)).Scan(&queued); err != nil {
		f.t.Fatal(err)
	}
	st, err := LoadSettings(ctx(), f.db.Pool, accountID)
	if err != nil {
		f.t.Fatal(err)
	}
	return queued, st.LowBalanceAlerted
}

// wantLowBalance checks the low-balance notifications and alert flag.
func (f *fixture) wantLowBalance(accountID string, queued int64, alerted bool) {
	f.t.Helper()
	if gotQueued, gotAlerted := f.lowBalanceState(accountID); gotQueued != queued || gotAlerted != alerted {
		f.t.Fatalf("low-balance notifications = %d, alerted = %v, want %d and %v", gotQueued, gotAlerted, queued, alerted)
	}
}

// expiringFixture returns a billing account with a 20 USD grant that is due
// to expire and a 5 USD grant that is not, and sets the clock just after the
// expiry of the first grant. The balance of 25 USD still includes the due
// grant, because no job has expired it yet.
func (f *fixture) expiringFixture() string {
	f.t.Helper()
	acct := f.account()
	f.grant(acct, LedgerSubscriptionCredit, 20*usd)
	f.clock.Advance(30 * 24 * time.Hour)
	f.grant(acct, LedgerManualTopUp, 5*usd)
	f.clock.Set(GrantExpireTime(f.clock.Now().Add(-30 * 24 * time.Hour)).Add(time.Second))
	f.wantLowBalance(acct, 0, false)
	return acct
}

func TestZeroDeductionInlineExpiryQueuesLowBalance(t *testing.T) {
	f := newFixture(t)
	acct := f.expiringFixture()
	res := f.deduct(acct, 0)
	if res.AutoTopUpQueued || res.BalanceAfterUSDMicros != 5*usd {
		t.Fatalf("zero deduction result %+v", res)
	}
	f.wantLowBalance(acct, 1, true)
	// The alert is not repeated while the balance stays below the threshold.
	f.deduct(acct, 0)
	f.deduct(acct, usd)
	f.wantLowBalance(acct, 1, true)
}

func TestZeroDeductionInlineExpiryNeverQueuesAutoTopUp(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerSubscriptionCredit, 20*usd)
	f.clock.Advance(30 * 24 * time.Hour)
	f.grant(acct, LedgerManualTopUp, 5*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 50*usd)
	f.clock.Set(GrantExpireTime(f.clock.Now().Add(-30 * 24 * time.Hour)).Add(time.Second))

	// With automatic top-up on, an expiry is no usage: it queues neither an
	// automatic top-up nor a low-balance email.
	if res := f.deduct(acct, 0); res.AutoTopUpQueued || res.BalanceAfterUSDMicros != 5*usd {
		t.Fatalf("zero deduction result %+v", res)
	}
	if got := f.topUpStatuses(acct); len(got) != 0 {
		t.Fatalf("top-ups after a zero deduction = %v", got)
	}
	f.wantLowBalance(acct, 0, false)
	// A positive usage below the threshold still queues it.
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("a usage deduction below the threshold queued no top-up")
	}
}

func TestGrantAfterInlineExpiryQueuesLowBalance(t *testing.T) {
	f := newFixture(t)
	acct := f.expiringFixture()
	// 25 USD before, 5 USD after the expiry, 8 USD after the grant: a
	// decrease that ends below the threshold.
	f.grant(acct, LedgerManualTopUp, 3*usd)
	if f.balance(acct) != 8*usd {
		t.Fatalf("balance = %d", f.balance(acct))
	}
	f.wantLowBalance(acct, 1, true)
}

func TestGrantAfterInlineExpiryDoesNotQueueAutoTopUp(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerSubscriptionCredit, 20*usd)
	f.clock.Advance(30 * 24 * time.Hour)
	f.grant(acct, LedgerManualTopUp, 5*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 50*usd)
	f.clock.Set(GrantExpireTime(f.clock.Now().Add(-30 * 24 * time.Hour)).Add(time.Second))

	f.grant(acct, LedgerManualTopUp, 3*usd)
	if got := f.topUpStatuses(acct); len(got) != 0 {
		t.Fatalf("top-ups after a grant = %v", got)
	}
	f.wantLowBalance(acct, 0, false)
}

func TestGrantRestoringBalanceRearmsLowBalance(t *testing.T) {
	f := newFixture(t)
	acct := f.expiringFixture()
	f.deduct(acct, 0) // the inline expiry brings the balance to 5 USD and alerts
	f.wantLowBalance(acct, 1, true)

	// A grant that reaches the threshold re-arms the alert and queues nothing.
	f.grant(acct, LedgerManualTopUp, 5*usd)
	f.wantLowBalance(acct, 1, false)
}

func TestInlineExpiryAboveThresholdQueuesNoLowBalance(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.grant(acct, LedgerSubscriptionCredit, 20*usd)
	f.clock.Advance(30 * 24 * time.Hour)
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.clock.Set(GrantExpireTime(f.clock.Now().Add(-30 * 24 * time.Hour)).Add(time.Second))

	// The expiry decreases the balance from 45 to 25 USD, above the threshold.
	f.grant(acct, LedgerManualTopUp, usd)
	f.wantLowBalance(acct, 0, false)
	f.clock.Advance(time.Hour)
	f.deduct(acct, 0)
	f.wantLowBalance(acct, 0, false)
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

// updateAutoTopUp replaces the automatic top-up settings, as an owner or an
// admin does while a top-up waits in the queue.
func (f *fixture) updateAutoTopUp(accountID string, enabled bool, threshold, amount, limit int64) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		_, err := UpdateAutoTopUp(ctx(), tx, accountID, autoSettings(enabled, threshold, amount, limit), f.s.timestamp())
		return err
	})
}

// wantObsoleteTopUp checks that the only top-up was closed as obsolete
// without a Paddle call, did not count as a failed automatic top-up and sent
// no email.
func (f *fixture) wantObsoleteTopUp(accountID string) {
	f.t.Helper()
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		f.t.Fatal(err)
	}
	if charges := f.paddle.Charges(); len(charges) != 0 {
		f.t.Fatalf("an obsolete top-up called Paddle: %+v", charges)
	}
	if got := f.topUpStatuses(accountID); len(got) != 1 || got[0] != "failed:settings_changed" {
		f.t.Fatalf("top-ups = %v", got)
	}
	if st, _ := loadSettings(ctx(), f.db.Pool, accountID); st.ConsecutiveFailures != 0 {
		f.t.Fatalf("the obsolete top-up counted as a failure: %+v", st)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		f.t.Fatal(err)
	}
	for _, m := range f.email.Messages() {
		if m.Kind == email.KindPaymentFailed || m.Kind == email.KindAutoTopUpDisabled {
			f.t.Fatalf("the obsolete top-up sent an email: %+v", m)
		}
	}
}

func TestPendingAutoTopUpClosedWhenDisabled(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.updateAutoTopUp(acct, false, 0, 0, 0)
	f.wantObsoleteTopUp(acct)
	// The closed top-up no longer blocks a later one under the new settings.
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued after the obsolete one closed")
	}
}

func TestPendingAutoTopUpClosedWhenAmountLowered(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 100*usd, 200*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.updateAutoTopUp(acct, true, 10*usd, 25*usd, 200*usd)
	f.wantObsoleteTopUp(acct)
}

func TestPendingAutoTopUpClosedWhenCapLowered(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 50*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	f.completeCharge(f.paddle.Charges()[0], 2500)
	if res := f.deduct(acct, 20*usd); !res.AutoTopUpQueued {
		t.Fatal("second top-up not queued")
	}
	// 25 USD of the month is spent; a cap of 40 USD leaves 15 USD, which
	// does not fit the second 25 USD top-up.
	f.updateAutoTopUp(acct, true, 10*usd, 25*usd, 40*usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want only the first", n)
	}
	if got := f.topUpStatuses(acct); len(got) != 2 || got[0] != "completed" || got[1] != "failed:settings_changed" {
		t.Fatalf("top-ups = %v", got)
	}
	if st, _ := loadSettings(ctx(), f.db.Pool, acct); !st.AutoEnabled || st.ConsecutiveFailures != 0 {
		t.Fatalf("settings = %+v", st)
	}
}

func TestPendingAutoTopUpStillValidChargesOnce(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 25*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	// The pending top-up uses the whole cap and still fits it; a larger
	// current amount and a new threshold do not change the queued amount.
	f.updateAutoTopUp(acct, true, 15*usd, 50*usd, 50*usd)
	for range 2 {
		if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
			t.Fatal(err)
		}
	}
	charges := f.paddle.Charges()
	if len(charges) != 1 || paddletest.ChargeAmountCents(charges[0]) != "2500" {
		t.Fatalf("charges = %+v", charges)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "charged" {
		t.Fatalf("top-ups = %v", got)
	}
}

func TestSubmittedAutoTopUpSurvivesSettingsChange(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	f.paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusActive})
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "unknown" {
		t.Fatalf("top-ups = %v", got)
	}
	// Disabling the setting leaves the submitted charge to its webhook.
	f.updateAutoTopUp(acct, false, 0, 0, 0)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "unknown" {
		t.Fatalf("top-ups after the settings change = %v", got)
	}
	charges := f.paddle.Charges()
	f.completeCharge(charges[len(charges)-1], 2500)
	if got := f.topUpStatuses(acct); got[0] != "completed" {
		t.Fatalf("top-ups after the webhook = %v", got)
	}
}

func autoSettings(enabled bool, threshold, amount, limit int64) *accountsv1.AutoTopUpSettings {
	return &accountsv1.AutoTopUpSettings{Enabled: enabled, ThresholdUsdMicros: threshold, AmountUsdMicros: amount, MonthlyCapUsdMicros: limit}
}
