// SPDX-License-Identifier: FSL-1.1-ALv2

// Package billing meters the billable usage of the control plane and
// reports it to delino-accounts: the four OwnSand meters, the aligned
// 5-minute UTC windows, the amounts in micro-USD, the RPC classification,
// the shared usage counters and the usage outbox with its reporter. The
// owning contracts are the Billing section of
// docs/servers-ownsand-api-foundation.md (metering and reporting),
// docs/project-ownsand.md#billing (meters and prices) and
// docs/protos-ownsand-api-contract.md#billing (RPC classification).
package billing

import (
	"errors"
	"math"
	"math/bits"
	"time"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Durations of the contract.
const (
	// WindowLength is the length of an aligned UTC usage window.
	WindowLength = 5 * time.Minute
	// SealDelay is how long after its end a window is sealed.
	SealDelay = time.Minute
)

// ServiceID is the delino-accounts service ID of OwnSand usage reports.
const ServiceID = accountsv1.ServiceId_SERVICE_ID_OWNSAND

// windowSeconds is WindowLength in seconds.
const windowSeconds = int64(WindowLength / time.Second)

// WindowStart returns the start of the aligned 5-minute UTC window that
// contains t.
func WindowStart(t time.Time) time.Time {
	s := t.Unix()
	m := s % windowSeconds
	if m < 0 {
		m += windowSeconds
	}
	return time.Unix(s-m, 0).UTC()
}

// sealCutoff returns the exclusive upper bound of the window starts that can
// be sealed at now: a window [s, s+5m) is sealed once s+5m+1m <= now.
func sealCutoff(now time.Time) time.Time {
	return WindowStart(now.Add(-WindowLength - SealDelay)).Add(WindowLength)
}

// Meters lists the OwnSand meters in BillingMeter order.
var Meters = []ownsandv1.BillingMeter{
	ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS,
	ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE,
	ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC,
	ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC,
}

// AccountsMeter returns the delino-accounts meter of an OwnSand meter, or
// METER_UNSPECIFIED for a value that is not an OwnSand meter.
func AccountsMeter(m ownsandv1.BillingMeter) accountsv1.Meter {
	switch m {
	case ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS:
		return accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS
	case ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE:
		return accountsv1.Meter_METER_OWNSAND_R2_STORAGE
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC:
		return accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC:
		return accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC
	}
	return accountsv1.Meter_METER_UNSPECIFIED
}

// MeterID returns the stable identifier of a meter for logs, for example
// `ownsand.relay_egress`.
func MeterID(m ownsandv1.BillingMeter) string {
	switch m {
	case ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS:
		return "ownsand.relay_egress"
	case ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE:
		return "ownsand.r2_storage"
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC:
		return "ownsand.class_a_rpc"
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC:
		return "ownsand.class_b_rpc"
	}
	return "unspecified"
}

// Prices. A GiB is 2^30 bytes and a GiB-month is 730 GiB-hours.
const (
	bytesPerGiB      = uint64(1) << 30
	hoursPerMonth    = 730
	secondsPerHour   = 3600
	relayEgressPrice = 90_000 // micro-USD per GiB ($0.09)
	r2StoragePrice   = 30_000 // micro-USD per GiB-month ($0.03)
)

// price is a unit price as the fraction num/den micro-USD per quantity unit.
type price struct{ num, den uint64 }

func priceOf(m ownsandv1.BillingMeter) (price, bool) {
	switch m {
	case ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS:
		// Quantity: bytes. ceil(bytes × 90,000 / 2^30).
		return price{num: relayEgressPrice, den: bytesPerGiB}, true
	case ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE:
		// Quantity: byte-seconds. ceil(byte-seconds × 30,000 / (2^30 × 3,600 × 730)).
		return price{num: r2StoragePrice, den: bytesPerGiB * secondsPerHour * hoursPerMonth}, true
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC:
		// Quantity: calls. $4.50 per million calls = 4.5 micro-USD per call.
		return price{num: 45, den: 10}, true
	case ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC:
		// Quantity: calls. $0.36 per million calls = 0.36 micro-USD per call.
		return price{num: 36, den: 100}, true
	}
	return price{}, false
}

// Errors of Amount.
var (
	ErrUnknownMeter     = errors.New("billing: unknown meter")
	ErrNegativeQuantity = errors.New("billing: negative quantity")
)

// Amount returns the amount in micro-USD of one window of a meter: the exact
// quantity times the unit price, rounded up (ceil) to a whole micro-USD once.
// The product uses a 128-bit intermediate, so it never overflows. A result
// above the largest int64 (more than 9.2 trillion USD in one window, which no
// meter can reach) saturates at math.MaxInt64 instead of wrapping.
func Amount(m ownsandv1.BillingMeter, quantity int64) (int64, error) {
	p, ok := priceOf(m)
	if !ok {
		return 0, ErrUnknownMeter
	}
	if quantity < 0 {
		return 0, ErrNegativeQuantity
	}
	q, ok := ceilMulDiv(uint64(quantity), p.num, p.den)
	if !ok || q > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(q), nil
}

// ceilMulDiv returns ceil(a × b / d) for d > 0 and reports false when the
// result does not fit in 64 bits.
func ceilMulDiv(a, b, d uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		// The quotient needs more than 64 bits (bits.Div64 would panic).
		return 0, false
	}
	q, rem := bits.Div64(hi, lo, d)
	if rem != 0 {
		if q == math.MaxUint64 {
			return 0, false
		}
		q++
	}
	return q, true
}

// saturatingAdd returns a + b for non-negative values, capped at
// math.MaxInt64.
func saturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
