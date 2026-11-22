// SPDX-License-Identifier: Apache-2.0

package uuidv7

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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

func TestGeneratorSeedsCounterAtUnixEpoch(t *testing.T) {
	epoch := func() time.Time { return time.UnixMilli(0) }
	counterOf := func(id UUID) uint16 {
		return uint16(id[6]&0x0f)<<8 | uint16(id[7])
	}

	// The first call at 0 ms seeds rand_a from the random bytes.
	random := make([]byte, 20)
	random[0], random[1] = 0x02, 0x34
	g := NewGenerator(epoch, bytes.NewReader(random))
	id, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	if !id.Time().Equal(time.UnixMilli(0)) {
		t.Fatalf("timestamp = %v, want the Unix epoch", id.Time())
	}
	if got := counterOf(id); got != 0x234 {
		t.Fatalf("rand_a = %#x, want 0x234", got)
	}

	// A seed of 0 still counts up on the next call in the same millisecond.
	g = NewGenerator(epoch, bytes.NewReader(make([]byte, 20)))
	first, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	if counterOf(first) != 0x000 || counterOf(second) != 0x001 {
		t.Fatalf("counters = %#x, %#x; want 0x000, 0x001", counterOf(first), counterOf(second))
	}
	if second.String() <= first.String() {
		t.Fatalf("second value does not sort after first: %s <= %s", second, first)
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

// exclusiveReader is an entropy reader that is not safe for concurrent use.
// It counts the calls that overlap in time, which a generator that holds its
// lock across the read never causes.
type exclusiveReader struct {
	inFlight   atomic.Int32
	overlaps   atomic.Int32
	reads      atomic.Int32
	source     io.Reader
	readyDelay time.Duration
}

func (r *exclusiveReader) Read(p []byte) (int, error) {
	if r.inFlight.Add(1) != 1 {
		r.overlaps.Add(1)
	}
	defer r.inFlight.Add(-1)
	r.reads.Add(1)
	// Give a concurrent caller a chance to enter the reader.
	time.Sleep(r.readyDelay)
	return r.source.Read(p)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestGeneratorSerializesEntropyReads(t *testing.T) {
	reader := &exclusiveReader{source: zeroReader{}, readyDelay: 50 * time.Microsecond}
	g := NewGenerator(nil, reader)

	const workers, perWorker = 16, 20
	ids := make(chan UUID, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id, err := g.New()
				if err != nil {
					t.Error(err)
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	if got := reader.overlaps.Load(); got != 0 {
		t.Fatalf("entropy reader calls overlapped %d times", got)
	}
	if got := reader.reads.Load(); got != workers*perWorker {
		t.Fatalf("reads = %d, want %d", got, workers*perWorker)
	}
	seen := make(map[UUID]bool, workers*perWorker)
	for id := range ids {
		if id.Version() != 7 || id[8]&0xc0 != 0x80 {
			t.Fatalf("%s is not a valid UUID v7", id)
		}
		if seen[id] {
			t.Fatalf("duplicate value %s", id)
		}
		seen[id] = true
	}
}

func TestGeneratorSequenceWithFixedClockIsPreserved(t *testing.T) {
	fixed := time.UnixMilli(1_700_000_000_000)
	g := NewGenerator(func() time.Time { return fixed }, zeroReader{})
	var previous UUID
	for i := 0; i < 5; i++ {
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		if got := uint16(id[6]&0x0f)<<8 | uint16(id[7]); got != uint16(i) {
			t.Fatalf("counter %d = %d, want %d", i, got, i)
		}
		if i > 0 && id.String() <= previous.String() {
			t.Fatalf("value %d does not sort after the previous one", i)
		}
		previous = id
	}
}

// failingOnceReader fails on its first read and then serves zero bytes.
type failingOnceReader struct{ failed bool }

func (r *failingOnceReader) Read(p []byte) (int, error) {
	if !r.failed {
		r.failed = true
		return 0, errTestEntropy
	}
	clear(p)
	return len(p), nil
}

var errTestEntropy = errors.New("entropy unavailable")

func TestGeneratorReadFailureWrapsErrorAndKeepsState(t *testing.T) {
	counterOf := func(id UUID) uint16 { return uint16(id[6]&0x0f)<<8 | uint16(id[7]) }
	fixed := time.UnixMilli(1_700_000_000_000)
	g := NewGenerator(func() time.Time { return fixed }, &failingOnceReader{})

	_, err := g.New()
	if !errors.Is(err, errTestEntropy) {
		t.Fatalf("error = %v, want it to wrap the reader error", err)
	}
	if want := "uuidv7: read random bytes: entropy unavailable"; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err, want)
	}
	if g.started || g.lastMs != 0 || g.counter != 0 {
		t.Fatalf("state changed after a read failure: started=%v lastMs=%d counter=%d", g.started, g.lastMs, g.counter)
	}

	// The first successful call after the failure seeds a fresh counter.
	id, err := g.New()
	if err != nil {
		t.Fatal(err)
	}
	if counterOf(id) != 0 || !id.Time().Equal(fixed) {
		t.Fatalf("unexpected first value after failure: counter=%d time=%v", counterOf(id), id.Time())
	}
}

func TestGeneratorOutOfRangeClockKeepsState(t *testing.T) {
	counterOf := func(id UUID) uint16 { return uint16(id[6]&0x0f)<<8 | uint16(id[7]) }
	beyond := time.UnixMilli(maxUnixMillis + 1)

	t.Run("first call is rejected without starting", func(t *testing.T) {
		now := beyond
		random := make([]byte, 30)
		random[0], random[1] = 0x01, 0x23 // would seed 0x123
		random[10], random[11] = 0x02, 0x34
		g := NewGenerator(func() time.Time { return now }, bytes.NewReader(random))
		if _, err := g.New(); err == nil {
			t.Fatal("out-of-range clock accepted")
		}
		if g.started || g.lastMs != 0 || g.counter != 0 {
			t.Fatalf("state changed by a rejected reading: started=%v lastMs=%d counter=%d", g.started, g.lastMs, g.counter)
		}
		// The next valid call seeds from its own entropy, including at the epoch.
		now = time.UnixMilli(0)
		id, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		if !id.Time().Equal(time.UnixMilli(0)) {
			t.Fatalf("timestamp = %v, want the Unix epoch", id.Time())
		}
		if got := counterOf(id); got != 0x234 {
			t.Fatalf("rand_a = %#x, want 0x234", got)
		}
	})

	t.Run("later valid readings still sort after the last value", func(t *testing.T) {
		now := time.UnixMilli(1_700_000_000_000)
		g := NewGenerator(func() time.Time { return now }, zeroReader{})
		first, err := g.New()
		if err != nil {
			t.Fatal(err)
		}
		now = beyond
		if _, err := g.New(); err == nil {
			t.Fatal("out-of-range clock accepted")
		}
		for _, valid := range []time.Time{time.UnixMilli(1_700_000_000_000), time.UnixMilli(1_699_999_999_000)} {
			now = valid
			next, err := g.New()
			if err != nil {
				t.Fatal(err)
			}
			if next.String() <= first.String() {
				t.Fatalf("value after a rejected reading does not sort after the last one: %s <= %s", next, first)
			}
			first = next
		}
		// The rejected reading did not advance the counter.
		if got := counterOf(first); got != 2 {
			t.Fatalf("counter = %d, want 2", got)
		}
	})

	t.Run("counter exhaustion at the maximum timestamp", func(t *testing.T) {
		max := time.UnixMilli(maxUnixMillis)
		g := NewGenerator(func() time.Time { return max }, zeroReader{})
		g.started, g.lastMs, g.counter = true, maxUnixMillis, 0x0fff
		if _, err := g.New(); err == nil {
			t.Fatal("counter overflow beyond the maximum timestamp accepted")
		}
		if !g.started || g.lastMs != maxUnixMillis || g.counter != 0x0fff {
			t.Fatalf("state changed by a rejected overflow: lastMs=%d counter=%#x", g.lastMs, g.counter)
		}
	})
}
