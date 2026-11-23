// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"testing"
	"time"
)

// expiryNoticeState returns the number of credit expiry notifications of a
// grant and whether the grant has an expiry notice time.
func (f *fixture) expiryNoticeState(grantID string) (notices int, marked bool) {
	f.t.Helper()
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications WHERE dedup_key = $1`,
		"credit_expiry/"+grantID).Scan(&notices); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.Pool.QueryRow(ctx(), `SELECT expiry_notice_time IS NOT NULL FROM credit_grants WHERE credit_grant_id = $1`,
		grantID).Scan(&marked); err != nil {
		f.t.Fatal(err)
	}
	return notices, marked
}

// TestQueueExpiryNoticesSkipsGrantThatExpiresWhileWaitingForLocks checks that
// a notice job that selected a grant before its expiry and waits for the
// billing account lock queues no "expires in 7 days" notice when the grant
// expires during the wait, and does not mark the grant as noticed.
func TestQueueExpiryNoticesSkipsGrantThatExpiresWhileWaitingForLocks(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	grantID := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	expire := GrantExpireTime(f.clock.Now())
	f.clock.Set(expire.Add(-time.Hour))

	release := f.holdAccountLock(acct)
	result := make(chan error, 1)
	go func() { result <- f.s.QueueExpiryNotices(ctx()) }()
	f.waitForAccountLockWait()
	f.clock.Set(expire.Add(time.Second))
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	if notices, marked := f.expiryNoticeState(grantID); notices != 0 || marked {
		t.Fatalf("expired grant: %d notices, marked %v; want none", notices, marked)
	}
	// The expiry job still removes the credit and later runs queue nothing.
	if err := f.s.ExpireGrants(ctx()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.QueueExpiryNotices(ctx()); err != nil {
		t.Fatal(err)
	}
	if f.remaining(grantID) != 0 {
		t.Fatalf("remaining %d after the expiry, want 0", f.remaining(grantID))
	}
	if notices, marked := f.expiryNoticeState(grantID); notices != 0 || marked {
		t.Fatalf("after the expiry: %d notices, marked %v; want none", notices, marked)
	}
}

// TestQueueExpiryNoticesQueuesOneNoticeForFutureExpiry is the control: a
// grant that still expires in the future after the lock wait gets exactly one
// notice, and a second run adds none.
func TestQueueExpiryNoticesQueuesOneNoticeForFutureExpiry(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	grantID := f.grant(acct, LedgerSubscriptionCredit, 10*usd)
	expire := GrantExpireTime(f.clock.Now())
	f.clock.Set(expire.Add(-time.Hour))

	release := f.holdAccountLock(acct)
	result := make(chan error, 1)
	go func() { result <- f.s.QueueExpiryNotices(ctx()) }()
	f.waitForAccountLockWait()
	f.clock.Set(expire.Add(-time.Minute))
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := f.s.QueueExpiryNotices(ctx()); err != nil {
		t.Fatal(err)
	}
	if notices, marked := f.expiryNoticeState(grantID); notices != 1 || !marked {
		t.Fatalf("future grant: %d notices, marked %v; want 1 and marked", notices, marked)
	}
}
