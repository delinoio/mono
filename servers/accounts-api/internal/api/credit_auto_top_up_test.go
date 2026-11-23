// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// storedAutoTopUp reads the saved automatic top-up columns directly.
func storedAutoTopUp(t *testing.T, env *testenv.Env, accountID string) (enabled bool, threshold, amount, cap int64, found bool) {
	t.Helper()
	rows, err := env.DB.Pool.Query(ctx(), `SELECT auto_top_up_enabled, auto_top_up_threshold_usd_micros,
		auto_top_up_amount_usd_micros, auto_top_up_monthly_cap_usd_micros
		FROM credit_settings WHERE billing_account_id = $1`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&enabled, &threshold, &amount, &cap); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return enabled, threshold, amount, cap, found
}

// TestUpdateAutoTopUpSettingsReadFailureRollsBack makes the month-to-date
// read in the update transaction fail. The RPC must fail and leave neither
// the settings change nor its audit event behind, because the response is
// read before the commit.
func TestUpdateAutoTopUpSettingsReadFailureRollsBack(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	owner := env.Credit(f.Owner.ID)
	update := func(capUSD int64) (*connect.Response[accountsv1.UpdateAutoTopUpSettingsResponse], error) {
		return owner.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{
			BillingAccountId: f.AccountID,
			Settings: &accountsv1.AutoTopUpSettings{Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd,
				MonthlyCapUsdMicros: capUSD * usd},
		}))
	}
	if _, err := update(100); err != nil {
		t.Fatal(err)
	}
	before := readBusinessState(t, env, f.AccountID)

	// The month-to-date total reads top_ups; without the table the read
	// fails after the settings row was updated in the same transaction.
	if _, err := env.DB.Pool.Exec(ctx(), `ALTER TABLE top_ups RENAME TO top_ups_unavailable`); err != nil {
		t.Fatal(err)
	}
	_, err := update(200)
	wantCode(t, err, connect.CodeInternal)
	if _, err := env.DB.Pool.Exec(ctx(), `ALTER TABLE top_ups_unavailable RENAME TO top_ups`); err != nil {
		t.Fatal(err)
	}

	enabled, threshold, amount, capMicros, found := storedAutoTopUp(t, env, f.AccountID)
	if !found || !enabled || threshold != 10*usd || amount != 25*usd || capMicros != 100*usd {
		t.Fatalf("stored settings after the failed update = %v %d %d %d (found %v), want the previous 100 USD cap", enabled, threshold, amount, capMicros, found)
	}
	if after := readBusinessState(t, env, f.AccountID); after != before {
		t.Fatalf("business state after the failed update = %+v, want %+v", after, before)
	}

	// The same update succeeds once the read works, and records one audit
	// event.
	res, err := update(200)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetSettings().GetMonthlyCapUsdMicros(); got != 200*usd {
		t.Fatalf("cap in the response = %d", got)
	}
	if after := readBusinessState(t, env, f.AccountID); after.AuditEvents != before.AuditEvents+1 {
		t.Fatalf("audit events = %d, want %d", after.AuditEvents, before.AuditEvents+1)
	}
}

// TestUpdateAutoTopUpSettingsResponsesMatchOwnWrites runs concurrent updates
// with distinct caps. Each response must hold the settings that its own
// request wrote, not those of a writer that committed after it.
func TestUpdateAutoTopUpSettingsResponsesMatchOwnWrites(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	owner := env.Credit(f.Owner.ID)

	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := int64(100+i) * usd
			res, err := owner.UpdateAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.UpdateAutoTopUpSettingsRequest{
				BillingAccountId: f.AccountID,
				Settings: &accountsv1.AutoTopUpSettings{Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd,
					MonthlyCapUsdMicros: want},
			}))
			if err != nil {
				errs <- fmt.Errorf("writer %d: %w", i, err)
				return
			}
			if got := res.Msg.GetSettings().GetMonthlyCapUsdMicros(); got != want {
				errs <- fmt.Errorf("writer %d: response cap = %d, want its own write %d", i, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The last committed write is what the read RPC returns.
	got, err := owner.GetAutoTopUpSettings(ctx(), connect.NewRequest(&accountsv1.GetAutoTopUpSettingsRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, capMicros, _ := storedAutoTopUp(t, env, f.AccountID)
	if got.Msg.GetSettings().GetMonthlyCapUsdMicros() != capMicros {
		t.Fatalf("GetAutoTopUpSettings cap = %d, stored %d", got.Msg.GetSettings().GetMonthlyCapUsdMicros(), capMicros)
	}
}
