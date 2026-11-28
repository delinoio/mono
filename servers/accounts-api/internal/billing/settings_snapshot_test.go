// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func TestAutoTopUpSettingsReadOneSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})

	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("first automatic top-up was not queued")
	}
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("second automatic top-up was not queued")
	}

	// Hold the read connection so the worker commits on another connection.
	readConn, err := f.db.Pool.Acquire(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer readConn.Release()
	var declineRan bool
	q := &interleavingQuerier{inner: readConn, afterFirst: func() {
		if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
			t.Fatal(err)
		}
		declineRan = true
	}}
	now := f.clock.Now()
	read := func(q db.Querier) *accountsv1.AutoTopUpSettings {
		t.Helper()
		settings, err := LoadAutoTopUpSettings(ctx(), q, acct, now)
		if err != nil {
			t.Fatal(err)
		}
		return settings
	}
	before := read(q)
	if !declineRan {
		t.Fatal("the second decline did not run between the buffered read and its result")
	}
	after := read(readConn)
	want := &accountsv1.AutoTopUpSettings{
		Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd,
		MonthlyCapUsdMicros: 100 * usd, ConsecutiveFailures: 1, MonthToDateUsdMicros: 25 * usd,
	}
	if !proto.Equal(before, want) {
		t.Errorf("read during the second decline = %v, want %v", before, want)
	}
	want.Enabled, want.ConsecutiveFailures, want.MonthToDateUsdMicros = false, 2, 0
	if !proto.Equal(after, want) {
		t.Errorf("read after the second decline = %v, want %v", after, want)
	}
}
