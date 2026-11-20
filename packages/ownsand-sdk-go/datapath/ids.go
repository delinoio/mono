// Package datapath holds the data path primitives that the OwnSand control
// plane, the device agent, the guest helper and the SDKs share: UUID v7
// identifiers, the relay request digest and the length-prefixed DataFrame
// codec. It depends only on the committed protobuf bindings.
package datapath

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UUID is a 16-byte RFC 9562 UUID.
type UUID [16]byte

// ErrInvalidUUID reports a value that is not a canonical UUID string.
var ErrInvalidUUID = errors.New("invalid UUID")

// NewUUIDv7 returns a new UUID v7 for the current time.
func NewUUIDv7() UUID {
	return NewUUIDv7At(time.Now())
}

// NewUUIDv7At returns a new UUID v7 with the given timestamp. Tests use it to
// build IDs with old or future timestamps.
func NewUUIDv7At(t time.Time) UUID {
	var u UUID
	ms := uint64(t.UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(u[0:6], ts[2:8])
	if _, err := rand.Read(u[6:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u
}

// NewID returns a new UUID v7 in canonical text form.
func NewID() string {
	return NewUUIDv7().String()
}

// String returns the canonical lowercase text form.
func (u UUID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// Version returns the UUID version number.
func (u UUID) Version() int {
	return int(u[6] >> 4)
}

// Time returns the timestamp of a UUID v7.
func (u UUID) Time() time.Time {
	var ts [8]byte
	copy(ts[2:8], u[0:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ts[:])))
}

// ParseUUID parses a canonical UUID string. It accepts upper- and lowercase
// hex digits.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, ErrInvalidUUID
	}
	h := strings.ReplaceAll(s, "-", "")
	if _, err := hex.Decode(u[:], []byte(h)); err != nil {
		return u, ErrInvalidUUID
	}
	return u, nil
}

// ParseUUIDv7 parses a canonical UUID string and requires version 7 and the
// RFC 9562 variant.
func ParseUUIDv7(s string) (UUID, error) {
	u, err := ParseUUID(s)
	if err != nil {
		return u, err
	}
	if u.Version() != 7 || u[8]&0xc0 != 0x80 {
		return u, ErrInvalidUUID
	}
	return u, nil
}

// IsUUIDv7 reports whether s is a canonical UUID v7.
func IsUUIDv7(s string) bool {
	_, err := ParseUUIDv7(s)
	return err == nil
}

// OperationIDAge classifies the timestamp of a new caller-generated operation
// ID (exec_id, read_id or write_id) against the agent's clock, according to
// the expired operation ID rules of the protos contract.
type OperationIDAge int

const (
	// OperationIDFresh: less than 15 minutes old and at most 5 minutes in the future.
	OperationIDFresh OperationIDAge = iota
	// OperationIDExpired: 15 minutes old or older. Fails with not_found.
	OperationIDExpired
	// OperationIDFuture: more than 5 minutes in the future. Fails with failed_precondition.
	OperationIDFuture
)

const (
	// OperationIDMaxAge is the age at which a new operation ID is expired.
	OperationIDMaxAge = 15 * time.Minute
	// OperationIDMaxSkew is how far in the future a new operation ID may be.
	OperationIDMaxSkew = 5 * time.Minute
)

// ClassifyOperationID classifies the timestamp of id relative to now.
func ClassifyOperationID(id UUID, now time.Time) OperationIDAge {
	t := id.Time()
	switch {
	case t.After(now.Add(OperationIDMaxSkew)):
		return OperationIDFuture
	case !t.After(now.Add(-OperationIDMaxAge)):
		return OperationIDExpired
	default:
		return OperationIDFresh
	}
}
