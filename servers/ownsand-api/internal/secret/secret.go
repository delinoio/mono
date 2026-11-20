// SPDX-License-Identifier: FSL-1.1-ALv2

// Package secret generates, recognizes and hashes the OwnSand secrets: API
// keys (osk_), enrollment tokens (ose_), invitation secrets (osi_) and device
// session credentials (osd_). Each secret is its prefix followed by 32 random
// bytes in unpadded base64url. The server stores only the SHA-256 hash and a
// short non-secret display prefix.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Kind identifies a secret format by its prefix.
type Kind int

const (
	KindUnknown Kind = iota
	KindAPIKey
	KindEnrollmentToken
	KindInvitation
	KindDeviceSession
)

// Prefix returns the textual prefix of the kind.
func (k Kind) Prefix() string {
	switch k {
	case KindAPIKey:
		return "osk_"
	case KindEnrollmentToken:
		return "ose_"
	case KindInvitation:
		return "osi_"
	case KindDeviceSession:
		return "osd_"
	default:
		return ""
	}
}

// RandomBytes is the number of random bytes in each secret.
const RandomBytes = 32

// encodedLen is the length of the base64url text of RandomBytes bytes.
var encodedLen = base64.RawURLEncoding.EncodedLen(RandomBytes)

// DisplayPrefixLen is the number of characters of the secret (including the
// kind prefix) that the server keeps for display. Four prefix characters plus
// six random characters reveal 36 of 256 random bits.
const DisplayPrefixLen = 10

// New returns a new secret of the kind.
func New(kind Kind) string {
	var b [RandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on supported platforms.
		panic(err)
	}
	return kind.Prefix() + base64.RawURLEncoding.EncodeToString(b[:])
}

// Classify returns the kind of a well-formed secret, or KindUnknown.
func Classify(value string) Kind {
	for _, kind := range []Kind{KindAPIKey, KindEnrollmentToken, KindInvitation, KindDeviceSession} {
		rest, ok := strings.CutPrefix(value, kind.Prefix())
		if !ok {
			continue
		}
		if len(rest) != encodedLen {
			return KindUnknown
		}
		if _, err := base64.RawURLEncoding.Strict().DecodeString(rest); err != nil {
			return KindUnknown
		}
		return kind
	}
	return KindUnknown
}

// HasPrefix reports whether value starts with the kind prefix, whether or not
// the rest is well formed. The credential selector uses it.
func HasPrefix(value string, kind Kind) bool {
	return strings.HasPrefix(value, kind.Prefix())
}

// Hash returns the SHA-256 hash that the server stores.
func Hash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// Equal compares two hashes in constant time.
func Equal(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

// DisplayPrefix returns the non-secret display prefix of a secret.
func DisplayPrefix(value string) string {
	if len(value) <= DisplayPrefixLen {
		return value
	}
	return value[:DisplayPrefixLen]
}
