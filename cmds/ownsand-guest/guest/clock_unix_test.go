// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guest

import (
	"math"
	"testing"
	"time"
)

func TestSplitTimevalRoundsUpAndCarries(t *testing.T) {
	cases := []struct {
		name     string
		in       time.Time
		wantSec  int64
		wantUsec int64
	}{
		{"year 2300 does not overflow", time.Unix(10413792000, 0).UTC(), 10413792000, 0},
		{"year 2300 with microseconds", time.Unix(10413792000, 123456000).UTC(), 10413792000, 123456},
		{"sub-microsecond remainder rounds up", time.Date(2026, 1, 1, 0, 0, 0, 123456001, time.UTC), 1767225600, 123457},
		{"exact microsecond stays", time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC), 1767225600, 123456},
		{"999999999 ns carries into seconds", time.Date(2026, 1, 1, 0, 0, 0, 999999999, time.UTC), 1767225601, 0},
		{"999999001 ns carries into seconds", time.Date(2026, 1, 1, 0, 0, 0, 999999001, time.UTC), 1767225601, 0},
		{"999999000 ns does not carry", time.Date(2026, 1, 1, 0, 0, 0, 999999000, time.UTC), 1767225600, 999999},
		{"before the epoch", time.Unix(-1, 500000000).UTC(), -1, 500000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec, usec := splitTimeval(tc.in)
			if sec != tc.wantSec || usec != tc.wantUsec {
				t.Fatalf("splitTimeval = (%d, %d), want (%d, %d)", sec, usec, tc.wantSec, tc.wantUsec)
			}
		})
	}
}

func TestTimevalForFillsPlatformStruct(t *testing.T) {
	tv, err := timevalFor(time.Unix(10413792000, 0).UTC())
	if err == nil {
		if int64(tv.Sec) != 10413792000 || int64(tv.Usec) != 0 {
			t.Fatalf("timevalFor = (%d, %d), want (10413792000, 0)", tv.Sec, tv.Usec)
		}
		return
	}
	// A platform whose Timeval seconds are 32-bit must reject the value
	// instead of truncating it.
	if secondsFitInt32(10413792000) {
		t.Fatalf("timevalFor returned an error although the value fits")
	}
	if tv.Sec != 0 || tv.Usec != 0 {
		t.Fatalf("timevalFor returned a non-zero timeval with an error")
	}
}

func TestTimevalForCarryReachesStruct(t *testing.T) {
	tv, err := timevalFor(time.Date(2026, 1, 1, 0, 0, 0, 999999999, time.UTC))
	if err != nil {
		t.Fatalf("timevalFor returned an error: %v", err)
	}
	if int64(tv.Sec) != 1767225601 || int64(tv.Usec) != 0 {
		t.Fatalf("timevalFor = (%d, %d), want (1767225601, 0)", tv.Sec, tv.Usec)
	}
}

func TestFitsIntegerNarrowFields(t *testing.T) {
	var narrow int32 = 7
	if fitsInteger(&narrow, 10413792000) {
		t.Fatal("fitsInteger accepted a value that does not fit int32")
	}
	if narrow != 7 {
		t.Fatalf("fitsInteger changed the destination to %d on failure", narrow)
	}
	if !fitsInteger(&narrow, math.MaxInt32) || narrow != math.MaxInt32 {
		t.Fatalf("fitsInteger rejected math.MaxInt32 or stored %d", narrow)
	}
	if fitsInteger(&narrow, math.MinInt32-1) {
		t.Fatal("fitsInteger accepted a value below int32")
	}
	var wide int64
	if !fitsInteger(&wide, 10413792000) || wide != 10413792000 {
		t.Fatalf("fitsInteger rejected or mis-stored an int64 value: %d", wide)
	}
}

func secondsFitInt32(v int64) bool {
	var dst int32
	return fitsInteger(&dst, v)
}
