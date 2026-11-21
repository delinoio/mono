// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"errors"
	"math"
	"strconv"
	"time"
)

// Money units. Every amount is an int64 count of micro-USD
// (docs/repository-billing-contract.md, Precision).
const (
	MicrosPerUSD  int64 = 1_000_000
	MicrosPerCent int64 = 10_000
)

// Amount rules of docs/servers-accounts-api-foundation.md (Payments and
// credits).
const (
	// SubscriptionCreditUSDMicros is the credit grant of each paid
	// subscription period, and SubscriptionPriceUSDMicros the price.
	SubscriptionCreditUSDMicros = 10 * MicrosPerUSD
	SubscriptionPriceUSDMicros  = 10 * MicrosPerUSD
	// MinTopUpUSDMicros and MaxTopUpUSDMicros bound manual top-ups; the
	// automatic top-up amount has the same minimum.
	MinTopUpUSDMicros = 25 * MicrosPerUSD
	MaxTopUpUSDMicros = 10_000 * MicrosPerUSD
	// MinAutoTopUpThresholdUSDMicros is the lowest automatic top-up
	// threshold.
	MinAutoTopUpThresholdUSDMicros = 10 * MicrosPerUSD
	// DefaultLowBalanceThresholdUSDMicros is the default alert threshold.
	DefaultLowBalanceThresholdUSDMicros = 10 * MicrosPerUSD
)

// ErrAmountOverflow is returned for a Paddle amount that does not fit in
// micro-USD.
var ErrAmountOverflow = errors.New("amount does not fit in micro-USD")

// CentsToMicros converts a Paddle amount in whole USD cents to micro-USD by
// multiplying by 10,000. The conversion is exact; it happens only at the
// payment boundary.
func CentsToMicros(cents int64) (int64, error) {
	if cents > math.MaxInt64/MicrosPerCent || cents < math.MinInt64/MicrosPerCent {
		return 0, ErrAmountOverflow
	}
	return cents * MicrosPerCent, nil
}

// MicrosToCents converts an amount that is a whole number of cents. ok is
// false for any other amount, which Paddle cannot charge.
func MicrosToCents(micros int64) (cents int64, ok bool) {
	if micros%MicrosPerCent != 0 {
		return 0, false
	}
	return micros / MicrosPerCent, true
}

// WholeCents reports whether micros is a whole number of cents.
func WholeCents(micros int64) bool {
	return micros%MicrosPerCent == 0
}

// GrantLifetimeMonths is the lifetime of a credit grant.
const GrantLifetimeMonths = 12

// GrantExpireTime returns the expiry of a grant created at t: 12 calendar
// months later, in UTC.
func GrantExpireTime(t time.Time) time.Time {
	return t.UTC().AddDate(0, GrantLifetimeMonths, 0)
}

// ExpiryNoticeLead is how long before the expiry of a grant the owners and
// admins get an email.
const ExpiryNoticeLead = 7 * 24 * time.Hour

// MonthStart returns the start of the UTC calendar month of t.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// FormatUSD renders micro-USD for emails, rounded toward zero to cents. It
// is for display only (docs/repository-billing-contract.md, Precision).
func FormatUSD(micros int64) string {
	sign := ""
	if micros < 0 {
		sign = "-"
		if micros == math.MinInt64 {
			micros = math.MaxInt64
		} else {
			micros = -micros
		}
	}
	cents := micros / MicrosPerCent
	return sign + "$" + formatThousands(cents/100) + "." + twoDigits(cents%100)
}

func twoDigits(v int64) string {
	return string([]byte{byte('0' + v/10), byte('0' + v%10)})
}

func formatThousands(v int64) string {
	digits := []byte{}
	s := []byte(strconv.FormatInt(v, 10))
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			digits = append(digits, ',')
		}
		digits = append(digits, s[i])
	}
	return string(digits)
}
