// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
)

// NonceLifetime is the lifetime of a device challenge nonce.
const NonceLifetime = 60 * time.Second

// Signature purpose labels. The device signs the label bytes followed by
// the nonce, and the server checks that the label matches the RPC.
const (
	PurposeAuthenticate   = "ownsand-authenticate"
	PurposeConfirmRemoval = "ownsand-confirm-removal"
)

// Nonce layout: device ID (16 bytes) | random (16) | expiry Unix
// milliseconds (8, big endian) | HMAC-SHA256 of the previous 40 bytes (32).
const (
	nonceRandomSize = 16
	nonceBodySize   = 16 + nonceRandomSize + 8
	nonceSize       = nonceBodySize + sha256.Size
)

// ErrInvalidNonce reports a nonce that is malformed, expired, for another
// device or not authenticated by a current nonce key.
var ErrInvalidNonce = errors.New("invalid nonce")

// NonceIssuer issues and checks stateless nonces. A nonce is authenticated
// with the nonce key that every instance shares; the previous key is
// accepted during a rotation. CreateChallenge stores nothing.
type NonceIssuer struct {
	key      []byte
	previous []byte
	now      func() time.Time
}

// NewNonceIssuer returns an issuer. previous may be nil.
func NewNonceIssuer(key, previous []byte, now func() time.Time) *NonceIssuer {
	if now == nil {
		now = time.Now
	}
	return &NonceIssuer{key: key, previous: previous, now: now}
}

// Issue returns a new nonce for deviceID and its expiry.
func (n *NonceIssuer) Issue(deviceID string) ([]byte, time.Time, error) {
	id, err := uuidv7.Parse(deviceID)
	if err != nil {
		return nil, time.Time{}, err
	}
	expiry := n.now().Add(NonceLifetime).Truncate(time.Millisecond)
	out := make([]byte, nonceBodySize, nonceSize)
	copy(out, id[:])
	if _, err := rand.Read(out[16 : 16+nonceRandomSize]); err != nil {
		return nil, time.Time{}, err
	}
	binary.BigEndian.PutUint64(out[16+nonceRandomSize:], uint64(expiry.UnixMilli()))
	out = append(out, mac(n.key, out)...)
	return out, expiry, nil
}

// Check verifies that nonce was issued for deviceID by a current key and has
// not expired. It returns the nonce expiry, which bounds the replay record.
func (n *NonceIssuer) Check(deviceID string, nonce []byte) (time.Time, error) {
	if len(nonce) != nonceSize {
		return time.Time{}, ErrInvalidNonce
	}
	body, tag := nonce[:nonceBodySize], nonce[nonceBodySize:]
	if !hmac.Equal(tag, mac(n.key, body)) && (n.previous == nil || !hmac.Equal(tag, mac(n.previous, body))) {
		return time.Time{}, ErrInvalidNonce
	}
	id, err := uuidv7.Parse(deviceID)
	if err != nil || !hmac.Equal(id[:], body[:16]) {
		return time.Time{}, ErrInvalidNonce
	}
	expiry := time.UnixMilli(int64(binary.BigEndian.Uint64(body[16+nonceRandomSize:])))
	if NonceExpired(n.now(), expiry) {
		return time.Time{}, ErrInvalidNonce
	}
	return expiry, nil
}

// NonceExpired reports whether a nonce with the given expiry is no longer
// valid at now. The expiry instant itself is already expired. Check and the
// locked validation in the API use this one predicate, so both agree on the
// exact deadline.
func NonceExpired(now, expiry time.Time) bool {
	return !now.Before(expiry)
}

func mac(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// VerifyDeviceSignature checks an Ed25519 signature of the purpose label
// followed by the nonce.
func VerifyDeviceSignature(publicKey []byte, purpose string, nonce, signature []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return false
	}
	msg := make([]byte, 0, len(purpose)+len(nonce))
	msg = append(msg, purpose...)
	msg = append(msg, nonce...)
	return ed25519.Verify(publicKey, msg, signature)
}

// NonceHash is the replay record key of a consumed nonce.
func NonceHash(nonce []byte) []byte {
	sum := sha256.Sum256(nonce)
	return sum[:]
}
