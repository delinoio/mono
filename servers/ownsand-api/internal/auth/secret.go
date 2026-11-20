// SPDX-License-Identifier: FSL-1.1-ALv2

// Package auth authenticates the credentials that the control plane
// accepts: Logto access tokens, organization API keys (`osk_`) and device
// session credentials (`osd_`), and it issues and checks the other secrets
// (`ose_` enrollment tokens and `osi_` invitation secrets) and the stateless
// device nonces. The owning contract is the Authentication section of
// docs/servers-ownsand-api-foundation.md.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// SecretKind is the kind of an OwnSand secret, recognized by its prefix.
type SecretKind int

const (
	SecretUnknown SecretKind = iota
	// SecretAPIKey is an organization API key, `osk_`.
	SecretAPIKey
	// SecretEnrollmentToken is a one-time enrollment token, `ose_`.
	SecretEnrollmentToken
	// SecretInvitation is a one-time invitation secret, `osi_`.
	SecretInvitation
	// SecretDeviceSession is a device session credential, `osd_`.
	SecretDeviceSession
)

// Prefix returns the fixed prefix of the secret kind.
func (k SecretKind) Prefix() string {
	switch k {
	case SecretAPIKey:
		return "osk_"
	case SecretEnrollmentToken:
		return "ose_"
	case SecretInvitation:
		return "osi_"
	case SecretDeviceSession:
		return "osd_"
	}
	return ""
}

// secretRandomBytes is the number of random bytes after the prefix.
const secretRandomBytes = 32

// secretEncodedLen is the length of the unpadded base64url random part.
var secretEncodedLen = base64.RawURLEncoding.EncodedLen(secretRandomBytes)

// displayPrefixChars is the number of random characters that the display
// prefix keeps after the kind prefix. 4 characters (24 bits) identify a
// secret in a list without weakening it.
const displayPrefixChars = 4

// Secret is a newly issued secret. Value is returned to the caller once and
// never stored or logged.
type Secret struct {
	Value         string
	Hash          []byte
	DisplayPrefix string
}

// NewSecret returns a new secret of kind: the prefix followed by 32 random
// bytes from crypto/rand in unpadded base64url.
func NewSecret(kind SecretKind) (Secret, error) {
	var b [secretRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Secret{}, err
	}
	value := kind.Prefix() + base64.RawURLEncoding.EncodeToString(b[:])
	return Secret{Value: value, Hash: HashSecret(value), DisplayPrefix: DisplayPrefix(value)}, nil
}

// HashSecret returns the SHA-256 of a secret, which is what the database
// stores.
func HashSecret(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// HashEqual compares two hashes in constant time.
func HashEqual(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

// DisplayPrefix returns the short non-secret prefix of a secret, which the
// API shows and the rate limiter keys on.
func DisplayPrefix(value string) string {
	n := 4 + displayPrefixChars
	if len(value) < n {
		return value
	}
	return value[:n]
}

// ClassifySecret returns the kind of a secret value from its prefix and
// shape. A value with a known prefix but a wrong shape is still classified
// by its prefix, so the matching validator rejects it; it never falls back
// to another validator.
func ClassifySecret(value string) SecretKind {
	for _, kind := range []SecretKind{SecretAPIKey, SecretEnrollmentToken, SecretInvitation, SecretDeviceSession} {
		if strings.HasPrefix(value, kind.Prefix()) {
			return kind
		}
	}
	return SecretUnknown
}

// WellFormed reports whether value has the exact shape of a secret of kind.
func WellFormed(kind SecretKind, value string) bool {
	if !strings.HasPrefix(value, kind.Prefix()) || len(value) != 4+secretEncodedLen {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(value[4:])
	return err == nil
}
