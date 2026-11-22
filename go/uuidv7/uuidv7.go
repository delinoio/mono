// SPDX-License-Identifier: Apache-2.0

// Package uuidv7 generates and parses UUID version 7 identifiers
// (RFC 9562) in canonical text form. Repository services use them as the
// default identifier of persisted entities; OwnSand also uses them for the
// caller-generated exec_id, read_id, write_id, idempotency_key and
// authorization_id values.
package uuidv7

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// UUID is a 16-byte UUID.
type UUID [16]byte

// ErrInvalid reports a value that is not a canonical UUID v7.
var ErrInvalid = errors.New("uuidv7: not a canonical UUID v7")

// maxUnixMillis is the largest timestamp that the 48-bit field holds.
const maxUnixMillis = 1<<48 - 1

// Generator creates UUID v7 values that increase strictly within one
// process. It fills the 12-bit rand_a field with a counter that starts from
// a random value at each new millisecond (RFC 9562, section 6.2, method 1)
// and moves the timestamp forward by one millisecond when the counter
// overflows. The zero value is not usable; call NewGenerator.
type Generator struct {
	mu      sync.Mutex
	now     func() time.Time
	random  io.Reader
	lastMs  uint64
	counter uint16
	// started records that the generator has issued a value, so that a first
	// clock reading of 0 ms (the Unix epoch) is not mistaken for "no previous
	// value" through lastMs alone.
	started bool
}

// NewGenerator returns a generator that reads the clock from now and
// randomness from random. Nil arguments select time.Now and crypto/rand.
func NewGenerator(now func() time.Time, random io.Reader) *Generator {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Generator{now: now, random: random}
}

var defaultGenerator = NewGenerator(nil, nil)

// New returns a new UUID v7 from the default generator.
func New() (UUID, error) {
	return defaultGenerator.New()
}

// NewString returns a new UUID v7 in canonical text form.
func NewString() (string, error) {
	id, err := New()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// New returns a new UUID v7.
//
// The whole call runs under the generator mutex: it reads the entropy,
// computes the next (timestamp, counter) pair in locals and commits them only
// after both succeed. A read error or an out-of-range timestamp therefore
// leaves the generator state unchanged, and the random reader never sees
// overlapping calls from one generator, so it does not need to be safe for
// concurrent use.
func (g *Generator) New() (UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var id UUID
	if _, err := io.ReadFull(g.random, id[6:]); err != nil {
		return UUID{}, fmt.Errorf("uuidv7: read random bytes: %w", err)
	}
	ms := g.now().UnixMilli()
	if ms < 0 {
		ms = 0
	}
	current := uint64(ms)

	timestamp, counter := g.lastMs, g.counter
	switch {
	case !g.started || current > g.lastMs:
		timestamp = current
		// Start the counter in the lower half of its range, so that many
		// values fit in one millisecond before it overflows.
		counter = binary.BigEndian.Uint16(id[6:8]) & 0x07ff
	default:
		// The clock did not advance or went backwards: keep the last
		// timestamp and count up, so that values still increase.
		counter++
		if counter > 0x0fff {
			timestamp++
			counter = 0
		}
	}
	if timestamp > maxUnixMillis {
		return UUID{}, errors.New("uuidv7: timestamp out of range")
	}
	g.started = true
	g.lastMs = timestamp
	g.counter = counter

	id[0] = byte(timestamp >> 40)
	id[1] = byte(timestamp >> 32)
	id[2] = byte(timestamp >> 24)
	id[3] = byte(timestamp >> 16)
	id[4] = byte(timestamp >> 8)
	id[5] = byte(timestamp)
	id[6] = 0x70 | byte(counter>>8)
	id[7] = byte(counter)
	id[8] = 0x80 | id[8]&0x3f
	return id, nil
}

// FromTime returns a UUID v7 with the given timestamp and random bits from
// random (crypto/rand when nil). It does not guarantee ordering; tests use it
// to build IDs with a known timestamp.
func FromTime(t time.Time, random io.Reader) (UUID, error) {
	if random == nil {
		random = rand.Reader
	}
	ms := t.UnixMilli()
	if ms < 0 || ms > maxUnixMillis {
		return UUID{}, errors.New("uuidv7: timestamp out of range")
	}
	var id UUID
	if _, err := io.ReadFull(random, id[6:]); err != nil {
		return UUID{}, fmt.Errorf("uuidv7: read random bytes: %w", err)
	}
	timestamp := uint64(ms)
	for i := 0; i < 6; i++ {
		id[i] = byte(timestamp >> (40 - 8*i))
	}
	id[6] = 0x70 | id[6]&0x0f
	id[8] = 0x80 | id[8]&0x3f
	return id, nil
}

// Parse parses a canonical UUID v7 text form (8-4-4-4-12 hexadecimal digits,
// either case). It rejects other versions and variants.
func Parse(s string) (UUID, error) {
	var id UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return UUID{}, ErrInvalid
	}
	j := 0
	for i := 0; i < 36; {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			i++
			continue
		}
		hi, ok1 := fromHex(s[i])
		lo, ok2 := fromHex(s[i+1])
		if !ok1 || !ok2 {
			return UUID{}, ErrInvalid
		}
		id[j] = hi<<4 | lo
		j++
		i += 2
	}
	if id.Version() != 7 || id[8]&0xc0 != 0x80 {
		return UUID{}, ErrInvalid
	}
	return id, nil
}

// Valid reports whether s is a canonical UUID v7.
func Valid(s string) bool {
	_, err := Parse(s)
	return err == nil
}

// TimeOf parses s and returns its timestamp.
func TimeOf(s string) (time.Time, error) {
	id, err := Parse(s)
	if err != nil {
		return time.Time{}, err
	}
	return id.Time(), nil
}

// Version returns the UUID version number.
func (u UUID) Version() int {
	return int(u[6] >> 4)
}

// Time returns the millisecond Unix timestamp of a UUID v7.
func (u UUID) Time() time.Time {
	var ms uint64
	for i := 0; i < 6; i++ {
		ms = ms<<8 | uint64(u[i])
	}
	return time.UnixMilli(int64(ms))
}

// String returns the canonical lowercase text form.
func (u UUID) String() string {
	const digits = "0123456789abcdef"
	var buf [36]byte
	j := 0
	for i, b := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = digits[b>>4]
		buf[j+1] = digits[b&0x0f]
		j += 2
	}
	return string(buf[:])
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
