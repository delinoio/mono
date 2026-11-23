// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	gib       = int64(1) << 30
	gibHour   = gib * 3600 // byte-seconds of 1 GiB stored for one hour
	gibMonth  = gibHour * 730
	relay     = ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS
	r2        = ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE
	classA    = ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC
	classB    = ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC
	unspecMtr = ownsandv1.BillingMeter_BILLING_METER_UNSPECIFIED
)

func TestWindowStart(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-11-21T10:00:00Z", "2026-11-21T10:00:00Z"},
		{"2026-11-21T10:04:59.999Z", "2026-11-21T10:00:00Z"},
		{"2026-11-21T10:05:00Z", "2026-11-21T10:05:00Z"},
		{"2026-11-21T19:07:30+09:00", "2026-11-21T10:05:00Z"},
		{"1969-12-31T23:58:00Z", "1969-12-31T23:55:00Z"},
	}
	for _, c := range cases {
		in, _ := time.Parse(time.RFC3339Nano, c.in)
		got := WindowStart(in)
		if got.Format(time.RFC3339) != c.want || got.Location() != time.UTC {
			t.Errorf("WindowStart(%s) = %s, want %s", c.in, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestSealCutoff(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		now  time.Duration
		want time.Duration // exclusive bound of the sealed window starts
	}{
		// The window [10:00, 10:05) is sealed at 10:06, not before.
		{5*time.Minute + 59*time.Second, 0},
		{6 * time.Minute, 5 * time.Minute},
		{10*time.Minute + 59*time.Second, 5 * time.Minute},
		{11 * time.Minute, 10 * time.Minute},
	}
	for _, c := range cases {
		if got := sealCutoff(base.Add(c.now)); !got.Equal(base.Add(c.want)) {
			t.Errorf("sealCutoff(+%s) = %s, want +%s", c.now, got, c.want)
		}
	}
}

func TestAmount(t *testing.T) {
	cases := []struct {
		name  string
		meter ownsandv1.BillingMeter
		qty   int64
		want  int64
	}{
		{"relay zero", relay, 0, 0},
		{"relay one byte rounds up", relay, 1, 1},
		{"relay one GiB is $0.09", relay, gib, 90_000},
		{"relay one GiB plus a byte rounds up once", relay, gib + 1, 90_001},
		{"relay 10 GiB", relay, 10 * gib, 900_000},
		// 2^30 / 90,000 = 11930.46 bytes per micro-USD.
		{"relay exactly one micro-USD boundary", relay, 11930, 1},
		{"relay just above one micro-USD", relay, 11931, 2},
		{"r2 zero", r2, 0, 0},
		{"r2 one byte-second rounds up", r2, 1, 1},
		{"r2 one GiB-month is $0.03", r2, gibMonth, 30_000},
		// GiB-hours / 730: one GiB-hour is 30,000 / 730 = 41.0958... micro-USD.
		{"r2 one GiB-hour", r2, gibHour, 42},
		{"r2 730 GiB-hours equal one GiB-month", r2, 730 * gibHour, 30_000},
		// One GiB stored for one 5-minute window: 300 × 30,000 / (3,600 × 730) = 3.42.
		{"r2 one GiB for one window", r2, gib * 300, 4},
		{"r2 100 GiB for one window", r2, 100 * gib * 300, 343},
		{"class A one call rounds up", classA, 1, 5},
		{"class A two calls", classA, 2, 9},
		{"class A one million calls is $4.50", classA, 1_000_000, 4_500_000},
		{"class B one call rounds up", classB, 1, 1},
		{"class B 100 calls", classB, 100, 36},
		{"class B 101 calls", classB, 101, 37},
		{"class B one million calls is $0.36", classB, 1_000_000, 360_000},
		// No int64 overflow: the 128-bit intermediate keeps the exact product.
		{"relay max quantity", relay, math.MaxInt64, 773_094_113_280_000}, // ceil((2^63-1) × 90,000 / 2^30)
		{"r2 max quantity", r2, math.MaxInt64, 98_058_615},                // ceil((2^63-1) × 30,000 / (2^30 × 3,600 × 730))
		{"class B max quantity", classB, math.MaxInt64, 3_320_413_933_267_719_291},
		// 4.5 × (2^63-1) exceeds int64: saturates instead of wrapping.
		{"class A max quantity saturates", classA, math.MaxInt64, math.MaxInt64},
	}
	for _, c := range cases {
		got, err := Amount(c.meter, c.qty)
		if err != nil || got != c.want {
			t.Errorf("%s: Amount(%v, %d) = %d, %v; want %d", c.name, c.meter, c.qty, got, err, c.want)
		}
	}
	if _, err := Amount(unspecMtr, 1); err != ErrUnknownMeter {
		t.Errorf("unknown meter: %v", err)
	}
	if _, err := Amount(relay, -1); err != ErrNegativeQuantity {
		t.Errorf("negative quantity: %v", err)
	}
}

func TestCeilMulDiv(t *testing.T) {
	if q, ok := ceilMulDiv(math.MaxUint64, math.MaxUint64, 1); ok || q != 0 {
		t.Fatalf("overflowing quotient: %d %v", q, ok)
	}
	if q, ok := ceilMulDiv(math.MaxUint64, 2, 2); !ok || q != math.MaxUint64 {
		t.Fatalf("exact max quotient: %d %v", q, ok)
	}
	if q, ok := ceilMulDiv(7, 1, 7); !ok || q != 1 {
		t.Fatalf("exact division: %d %v", q, ok)
	}
}

func TestMeterMappings(t *testing.T) {
	want := map[ownsandv1.BillingMeter]struct {
		accounts accountsv1.Meter
		id       string
	}{
		relay:  {accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS, "ownsand.relay_egress"},
		r2:     {accountsv1.Meter_METER_OWNSAND_R2_STORAGE, "ownsand.r2_storage"},
		classA: {accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, "ownsand.class_a_rpc"},
		classB: {accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC, "ownsand.class_b_rpc"},
	}
	if len(Meters) != len(want) {
		t.Fatalf("Meters = %v", Meters)
	}
	for _, m := range Meters {
		if AccountsMeter(m) != want[m].accounts || MeterID(m) != want[m].id {
			t.Errorf("meter %v maps to %v %q", m, AccountsMeter(m), MeterID(m))
		}
		if _, ok := priceOf(m); !ok {
			t.Errorf("meter %v has no price", m)
		}
	}
	if AccountsMeter(unspecMtr) != accountsv1.Meter_METER_UNSPECIFIED {
		t.Error("unspecified meter maps to an accounts meter")
	}
}

func TestRecorder(t *testing.T) {
	r := NewRecorder(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	org := "0190f3a4-0000-7000-8000-000000000001"
	at := time.Date(2026, 11, 21, 10, 2, 0, 0, time.UTC)
	r.Add(org, classA, at, 1)
	r.Add("0190F3A4-0000-7000-8000-000000000001", classA, at.Add(2*time.Minute), 2) // same window, other case
	r.Add(org, classA, at.Add(3*time.Minute), 1)                                    // next window
	r.Add(org, relay, at, math.MaxInt64)
	r.Add(org, relay, at, 5) // saturates
	r.Add(org, classB, at, 0)
	r.Add(org, classB, at, -3)
	r.Add("not-a-uuid", classB, at, 1)
	r.Add(org, unspecMtr, at, 1)
	counts := takeBatch(t, r)
	w := WindowStart(at).Unix()
	want := map[counterKey]int64{
		{org, classA, w}:       3,
		{org, classA, w + 300}: 1,
		{org, relay, w}:        math.MaxInt64,
	}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v", counts)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%v] = %d, want %d", k, counts[k], v)
		}
	}
	if r.Pending() != 0 || takeBatch(t, r) != nil {
		t.Fatal("sealing left counters behind")
	}
}

// takeBatch seals the live counters into a batch, acknowledges it as a
// successful flush would and returns its counters (nil without counters).
func takeBatch(t *testing.T, r *Recorder) map[counterKey]int64 {
	t.Helper()
	if err := r.sealBatch(false, time.Now()); err != nil {
		t.Fatal(err)
	}
	b := r.firstPending()
	if b == nil {
		return nil
	}
	r.ack(b)
	return b.counts
}

// TestRecorderBatchSurvivesAFailedFlush checks that a pending batch keeps its
// ID and counters until it is acknowledged, and that counts recorded
// meanwhile form a separate live batch instead of joining it.
func TestRecorderBatchSurvivesAFailedFlush(t *testing.T) {
	r := NewRecorder(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	org := "0190f3a4-0000-7000-8000-000000000001"
	at := time.Date(2026, 11, 21, 10, 2, 0, 0, time.UTC)
	k := counterKey{org, classA, WindowStart(at).Unix()}
	r.Add(org, classA, at, 4)
	if err := r.sealBatch(false, at); err != nil {
		t.Fatal(err)
	}
	first := r.firstPending()
	id := first.id
	r.Add(org, classA, at, 3) // arrives while the batch is pending
	if err := r.sealBatch(false, at); err != nil {
		t.Fatal(err)
	}
	if again := r.firstPending(); again != first || again.id != id || again.counts[k] != 4 {
		t.Fatalf("pending batch changed: %+v", again)
	}
	if r.Pending() != 2 {
		t.Fatalf("pending counters = %d, want 2", r.Pending())
	}
	r.ack(first)
	second := r.firstPending()
	if second == nil || second.id == id || second.counts[k] != 3 {
		t.Fatalf("second batch = %+v", second)
	}
}
