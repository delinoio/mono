// SPDX-License-Identifier: FSL-1.1-ALv2

// Package nonce issues and checks the stateless device challenge nonces of
// AgentService.CreateChallenge. A nonce is
//
//	version (1 byte, 1) || device ID (16 bytes) || random (16 bytes) ||
//	expiry (8 bytes, big-endian Unix seconds) || HMAC-SHA256 (32 bytes)
//
// where the HMAC covers the preceding bytes with the nonce key that every
// instance shares. CreateChallenge stores nothing; Authenticate and
// ConfirmRemoval record a nonce only when they consume it.
package nonce

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
)

// Lifetime of a nonce.
const Lifetime = 60 * time.Second

const (
	version   = 1
	bodyLen   = 1 + 16 + 16 + 8
	totalLen  = bodyLen + sha256.Size
	MinKeyLen = 32
)

// Errors of Check. Callers map all of them to unauthenticated.
var (
	ErrMalformed = errors.New("nonce is malformed")
	ErrMAC       = errors.New("nonce was not issued by this service")
	ErrExpired   = errors.New("nonce expired")
	ErrDevice    = errors.New("nonce belongs to another device")
)

// Keys are the current nonce key and, during a rotation, the previous one.
type Keys struct {
	Current  []byte
	Previous []byte
}

// Issue returns a new nonce for the device and its expiry.
func (k Keys) Issue(deviceID string, now time.Time) ([]byte, time.Time, error) {
	id, err := datapath.ParseUUID(deviceID)
	if err != nil {
		return nil, time.Time{}, ErrMalformed
	}
	exp := now.Add(Lifetime).Truncate(time.Second)
	b := make([]byte, bodyLen, totalLen)
	b[0] = version
	copy(b[1:17], id[:])
	if _, err := rand.Read(b[17:33]); err != nil {
		return nil, time.Time{}, err
	}
	binary.BigEndian.PutUint64(b[33:41], uint64(exp.Unix()))
	return append(b, mac(k.Current, b)...), exp, nil
}

func mac(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// Check verifies a nonce for the device and returns its expiry.
func (k Keys) Check(n []byte, deviceID string, now time.Time) (time.Time, error) {
	if len(n) != totalLen || n[0] != version {
		return time.Time{}, ErrMalformed
	}
	body, sum := n[:bodyLen], n[bodyLen:]
	ok := hmac.Equal(sum, mac(k.Current, body))
	if !ok && len(k.Previous) > 0 {
		ok = hmac.Equal(sum, mac(k.Previous, body))
	}
	if !ok {
		return time.Time{}, ErrMAC
	}
	id, err := datapath.ParseUUID(deviceID)
	if err != nil || !hmac.Equal(id[:], body[1:17]) {
		return time.Time{}, ErrDevice
	}
	exp := time.Unix(int64(binary.BigEndian.Uint64(body[33:41])), 0)
	if !now.Before(exp) {
		return time.Time{}, ErrExpired
	}
	return exp, nil
}

// Purpose labels of the signed message.
const (
	LabelAuthenticate   = "ownsand-authenticate"
	LabelConfirmRemoval = "ownsand-confirm-removal"
)

// SignedMessage returns label || nonce.
func SignedMessage(label string, n []byte) []byte {
	return append([]byte(label), n...)
}

// Hash returns the value that the consumed nonce table stores.
func Hash(n []byte) []byte {
	s := sha256.Sum256(n)
	return s[:]
}
