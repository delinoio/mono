// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"math"
	"slices"
	"testing"
	"time"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

func TestConsumeFIFO(t *testing.T) {
	grants := []grantBalance{{"a", 5}, {"b", 10}, {"c", 3}}
	for _, tc := range []struct {
		amount    int64
		wantTakes []int64
		wantRest  int64
	}{
		{0, []int64{0, 0, 0}, 0},
		{4, []int64{4, 0, 0}, 0},
		{5, []int64{5, 0, 0}, 0},
		{12, []int64{5, 7, 0}, 0},
		{18, []int64{5, 10, 3}, 0},
		{25, []int64{5, 10, 3}, 7},
	} {
		takes, rest := consume(grants, tc.amount)
		if !slices.Equal(takes, tc.wantTakes) || rest != tc.wantRest {
			t.Errorf("consume(%d) = %v, %d; want %v, %d", tc.amount, takes, rest, tc.wantTakes, tc.wantRest)
		}
	}
	takes, rest := consume(nil, 9)
	if len(takes) != 0 || rest != 9 {
		t.Errorf("consume without grants = %v, %d; want all debt", takes, rest)
	}
}

func TestPayoffDebt(t *testing.T) {
	for _, tc := range []struct{ balance, amount, paid, kept int64 }{
		{0, 10, 0, 10},
		{7, 10, 0, 10},
		{-3, 10, 3, 7},
		{-10, 10, 10, 0},
		{-25, 10, 10, 0},
	} {
		paid, kept := payoff(tc.balance, tc.amount)
		if paid != tc.paid || kept != tc.kept {
			t.Errorf("payoff(%d, %d) = %d, %d; want %d, %d", tc.balance, tc.amount, paid, kept, tc.paid, tc.kept)
		}
	}
}

func TestMonthlyCap(t *testing.T) {
	const usd = MicrosPerUSD
	for _, tc := range []struct {
		mtd, amount, limit int64
		want               bool
	}{
		{0, 25 * usd, 25 * usd, true},
		{0, 25 * usd, 24 * usd, false},
		{50 * usd, 25 * usd, 100 * usd, true},
		{75 * usd, 25 * usd, 100 * usd, true},
		{80 * usd, 25 * usd, 100 * usd, false},
		{100 * usd, 25 * usd, 100 * usd, false},
	} {
		if got := capAllows(tc.mtd, tc.amount, tc.limit); got != tc.want {
			t.Errorf("capAllows(%d, %d, %d) = %v, want %v", tc.mtd, tc.amount, tc.limit, got, tc.want)
		}
	}
	at := time.Date(2026, 3, 31, 23, 59, 59, 0, time.FixedZone("x", -5*3600))
	if got := MonthStart(at); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("MonthStart uses UTC: %v", got)
	}
}

func TestFailureShutoff(t *testing.T) {
	n, off := nextFailureState(0, true)
	if n != 1 || off {
		t.Fatalf("first failure = %d, %v", n, off)
	}
	n, off = nextFailureState(n, true)
	if n != 2 || !off {
		t.Fatalf("second failure = %d, %v; want the shutoff", n, off)
	}
	if _, off := nextFailureState(5, false); off {
		t.Fatal("a disabled setting is turned off again")
	}
	if FailureNotApplied.CountsAsFailure() || !FailureDeclined.CountsAsFailure() || !FailureTimedOut.CountsAsFailure() {
		t.Fatal("CountsAsFailure")
	}
	for code, want := range map[string]FailureReason{
		paddle.CodePaymentDeclined:     FailureDeclined,
		paddle.CodeUpdateWhenPastDue:   FailurePastDue,
		paddle.CodeNextBilledAtTooSoon: FailureTooSoon,
		"something_else":               FailureRejected,
	} {
		if got := failureReason(paddle.OutcomeRejected, code); got != want {
			t.Errorf("failureReason(%s) = %s", code, got)
		}
	}
	if failureReason(paddle.OutcomeNotFound, "") != FailureNoSubscription {
		t.Error("not found")
	}
}

func TestMoney(t *testing.T) {
	if v, err := CentsToMicros(2500); err != nil || v != 25*MicrosPerUSD {
		t.Fatal(v, err)
	}
	if _, err := CentsToMicros(math.MaxInt64 / 1000); err == nil {
		t.Fatal("overflow accepted")
	}
	if c, ok := MicrosToCents(25 * MicrosPerUSD); !ok || c != 2500 {
		t.Fatal(c, ok)
	}
	if _, ok := MicrosToCents(25*MicrosPerUSD + 1); ok {
		t.Fatal("sub-cent accepted")
	}
	for micros, want := range map[int64]string{
		0:                     "$0.00",
		10 * MicrosPerUSD:     "$10.00",
		1_234_567_890_000:     "$1,234,567.89",
		-25*MicrosPerUSD - 99: "-$25.00",
		5_000:                 "$0.00",
		1_005_000:             "$1.00",
	} {
		if got := FormatUSD(micros); got != want {
			t.Errorf("FormatUSD(%d) = %s, want %s", micros, got, want)
		}
	}
	created := time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC)
	if got := GrantExpireTime(created); !got.Equal(time.Date(2027, 2, 28, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("GrantExpireTime = %v", got)
	}
}

func TestValidateTopUpAmount(t *testing.T) {
	for amount, ok := range map[int64]bool{
		MinTopUpUSDMicros:          true,
		MinTopUpUSDMicros - 10_000: false,
		MaxTopUpUSDMicros:          true,
		MaxTopUpUSDMicros + 10_000: false,
		MinTopUpUSDMicros + 10_001: false,
		MinTopUpUSDMicros + 10_000: true,
	} {
		if err := ValidateTopUpAmount(amount); (err == nil) != ok {
			t.Errorf("ValidateTopUpAmount(%d) = %v", amount, err)
		}
	}
}

func TestValidateAutoTopUp(t *testing.T) {
	const usd = MicrosPerUSD
	valid := func() *accountsv1.AutoTopUpSettings {
		return &accountsv1.AutoTopUpSettings{Enabled: true, ThresholdUsdMicros: 10 * usd, AmountUsdMicros: 25 * usd, MonthlyCapUsdMicros: 25 * usd}
	}
	if err := ValidateAutoTopUp(valid()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*accountsv1.AutoTopUpSettings){
		"low threshold": func(s *accountsv1.AutoTopUpSettings) { s.ThresholdUsdMicros = 10*usd - 10_000 },
		"low amount":    func(s *accountsv1.AutoTopUpSettings) { s.AmountUsdMicros = 25*usd - 10_000 },
		"cap below":     func(s *accountsv1.AutoTopUpSettings) { s.MonthlyCapUsdMicros = 25*usd - 10_000 },
		"missing cap":   func(s *accountsv1.AutoTopUpSettings) { s.MonthlyCapUsdMicros = 0 },
		"sub-cent":      func(s *accountsv1.AutoTopUpSettings) { s.AmountUsdMicros = 25*usd + 1 },
		"negative":      func(s *accountsv1.AutoTopUpSettings) { s.ThresholdUsdMicros = -10_000 },
		"too large": func(s *accountsv1.AutoTopUpSettings) {
			s.AmountUsdMicros, s.MonthlyCapUsdMicros = 10_001*usd, 20_000*usd
		},
		"off, sub-cent":  func(s *accountsv1.AutoTopUpSettings) { s.Enabled = false; s.AmountUsdMicros = 1 },
		"off, negative":  func(s *accountsv1.AutoTopUpSettings) { s.Enabled = false; s.MonthlyCapUsdMicros = -10_000 },
		"nil (replaced)": nil,
	} {
		in := valid()
		if mutate == nil {
			in = nil
		} else {
			mutate(in)
		}
		if err := ValidateAutoTopUp(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateAutoTopUp(&accountsv1.AutoTopUpSettings{Enabled: false}); err != nil {
		t.Errorf("off with zeros: %v", err)
	}
	if err := ValidateAutoTopUp(&accountsv1.AutoTopUpSettings{Enabled: false, ThresholdUsdMicros: usd}); err != nil {
		t.Errorf("off with kept values: %v", err)
	}
}

func TestMapPaddleStatus(t *testing.T) {
	for in, want := range map[string]SubscriptionStatus{
		"active": SubscriptionActive, "trialing": SubscriptionActive, "past_due": SubscriptionPastDue,
		"paused": SubscriptionPastDue, "canceled": SubscriptionCanceled,
	} {
		if got, ok := mapPaddleStatus(in); !ok || got != want {
			t.Errorf("mapPaddleStatus(%s) = %s", in, got)
		}
	}
	if _, ok := mapPaddleStatus("other"); ok {
		t.Error("unknown status mapped")
	}
}
