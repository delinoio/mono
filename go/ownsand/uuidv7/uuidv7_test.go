// SPDX-License-Identifier: Apache-2.0

package uuidv7

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewHasVersionVariantAndTime(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Millisecond)
	if id.Version() != 7 {
		t.Fatalf("version = %d", id.Version())
	}
	if id[8]&0xc0 != 0x80 {
		t.Fatalf("variant bits = %08b", id[8])
	}
	if id.Time().Before(before) || id.Time().After(after) {
		t.Fatalf("time %v not in [%v, %v]", id.Time(), before, after)
	}
	parsed, err := Parse(id.String())
	if err != nil || parsed != id {
		t.Fatalf("round trip: %v %v", parsed, err)
	}
}

func TestGeneratorIsStrictlyIncreasing(t *testing.T) {
	fixed := time.UnixMilli(1_700_000_000_000)
	g := NewGenerator(func() time.Time { return fixed }, nil)
	previous := ""
	// More than 4096 values in one millisecond forces counter overflow.
	for i := 0; i < 10000; i++ {
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		s := id.String()
		if s <= previous {
			t.Fatalf("value %d not increasing: %s <= %s", i, s, previous)
		}
		previous = s
	}
}

func TestGeneratorClockGoesBackwards(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_500)
	g := NewGenerator(func() time.Time { return now }, nil)
	first, _ := g.New()
	now = now.Add(-time.Second)
	second, _ := g.New()
	if second.String() <= first.String() {
		t.Fatalf("not increasing after clock step back")
	}
	if !second.Time().Equal(first.Time()) {
		t.Fatalf("timestamp moved with a backwards clock: %v %v", first.Time(), second.Time())
	}
}

func TestFromTime(t *testing.T) {
	at := time.Date(2026, 11, 20, 12, 0, 0, 123_000_000, time.UTC)
	id, err := FromTime(at, bytes.NewReader(make([]byte, 10)))
	if err != nil {
		t.Fatal(err)
	}
	if !id.Time().Equal(at) {
		t.Fatalf("time = %v, want %v", id.Time(), at)
	}
	if !Valid(id.String()) {
		t.Fatalf("%s is not valid", id)
	}
	if _, err := FromTime(time.UnixMilli(-1), nil); err == nil {
		t.Fatal("negative time accepted")
	}
}

func TestParse(t *testing.T) {
	valid := "0192f4b6-7c3a-7def-8abc-0123456789ab"
	id, err := Parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != valid {
		t.Fatalf("String() = %s", id)
	}
	upper, err := Parse(strings.ToUpper(valid))
	if err != nil || upper != id {
		t.Fatalf("uppercase: %v %v", upper, err)
	}
	if got, want := id.Time(), time.UnixMilli(0x0192f4b67c3a); !got.Equal(want) {
		t.Fatalf("Time() = %v, want %v", got, want)
	}
	for _, bad := range []string{
		"",
		"0192f4b6-7c3a-4def-8abc-0123456789ab", // version 4
		"0192f4b6-7c3a-7def-cabc-0123456789ab", // variant 110
		"0192f4b6-7c3a-7def-8abc-0123456789a",  // short
		"0192f4b67c3a-7def-8abc-0123456789abc", // misplaced hyphen
		"0192f4b6-7c3a-7def-8abc-0123456789ag", // not hex
		"{0192f4b6-7c3a-7def-8abc-0123456789ab}",
	} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) error = %v", bad, err)
		}
	}
	if _, err := TimeOf("nope"); err == nil {
		t.Fatal("TimeOf accepted an invalid value")
	}
}

func TestRandomReadFailure(t *testing.T) {
	g := NewGenerator(nil, bytes.NewReader(nil))
	if _, err := g.New(); err == nil {
		t.Fatal("expected error from empty random source")
	}
}
