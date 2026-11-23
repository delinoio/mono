// SPDX-License-Identifier: FSL-1.1-ALv2

// Package secrets encrypts the organization credentials that tide-api
// stores: the OwnSand API key, the agent integration credentials and the
// Claude Code work orders of queued requests. It uses AES-256-GCM with the
// keys of the service secret TIDE_SECRET_KEYS. Each sealed value records the
// ID of the key that encrypted it, so that the operator can add a new key,
// re-encrypt the stored values with it and then remove the old key. The
// owning contract is the Secrets section of
// docs/servers-tide-api-foundation.md.
//
// Key rotation:
//
//  1. Add a new key in front of TIDE_SECRET_KEYS and keep the old keys
//     after it. The first key is the primary key: Seal always uses it, and
//     Open accepts every listed key.
//  2. Deploy. The daily re-encryption jobs of the lease holder (registered
//     by the layers that store sealed values, see Keyring.Reseal) re-encrypt
//     every value whose key is not the primary key. Their log records give
//     the number of re-encrypted values.
//  3. When no stored value uses an old key, remove that key from
//     TIDE_SECRET_KEYS and deploy again.
//
// Storage: a layer stores a Sealed value either in two columns (a text key
// ID and a bytea ciphertext, which lets SQL find values of an old key) or in
// one bytea column with MarshalBinary. Always pass associated data that
// names the owner of the value (see AssociatedData), so a value that is
// copied to another row does not decrypt there.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the size of an AES-256 key in bytes.
const KeySize = 32

// maxKeyIDLength bounds a key ID, which every sealed value stores.
const maxKeyIDLength = 32

// envelopeVersion is the first byte of MarshalBinary output.
const envelopeVersion = 1

// Errors. Their messages never contain key material or plaintext.
var (
	// ErrUnknownKey reports a sealed value whose key is not configured.
	ErrUnknownKey = errors.New("secrets: the key of the sealed value is not configured")
	// ErrDecrypt reports a sealed value that does not decrypt with its key
	// and associated data.
	ErrDecrypt = errors.New("secrets: the sealed value does not decrypt")
	// ErrMalformed reports an envelope that UnmarshalBinary cannot read.
	ErrMalformed = errors.New("secrets: malformed sealed value")
)

// Key is one AES-256 key with its ID.
type Key struct {
	ID       string
	material [KeySize]byte
}

// NewKey returns a key from 32 bytes of key material. The ID is 1 to 32
// characters of ASCII letters, digits, `-` and `_`.
func NewKey(id string, material []byte) (Key, error) {
	if !validKeyID(id) {
		return Key{}, fmt.Errorf("secrets: key ID %q must be 1 to %d characters of letters, digits, '-' and '_'", id, maxKeyIDLength)
	}
	if len(material) != KeySize {
		return Key{}, fmt.Errorf("secrets: key %q must be %d bytes, got %d", id, KeySize, len(material))
	}
	k := Key{ID: id}
	copy(k.material[:], material)
	return k, nil
}

// GenerateKey returns a key with random material (tests and development).
func GenerateKey(id string) (Key, error) {
	var material [KeySize]byte
	if _, err := rand.Read(material[:]); err != nil {
		return Key{}, err
	}
	return NewKey(id, material[:])
}

func validKeyID(id string) bool {
	if id == "" || len(id) > maxKeyIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Keyring holds the configured keys. The first key is the primary key. It is
// safe for concurrent use.
type Keyring struct {
	primary string
	ids     []string
	aeads   map[string]cipher.AEAD
	// deriveKey is HMAC-SHA256(primary key, "tide-derive-v1"), the key of
	// Derive. The primary key itself is used only for AES-GCM.
	deriveKey []byte
}

// NewKeyring returns a keyring whose primary key is the first key. Key IDs
// must be unique.
func NewKeyring(keys ...Key) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("secrets: at least one key is required")
	}
	r := &Keyring{primary: keys[0].ID, aeads: make(map[string]cipher.AEAD, len(keys))}
	for _, k := range keys {
		if !validKeyID(k.ID) {
			return nil, fmt.Errorf("secrets: invalid key ID %q", k.ID)
		}
		if _, dup := r.aeads[k.ID]; dup {
			return nil, fmt.Errorf("secrets: key ID %q is listed twice", k.ID)
		}
		block, err := aes.NewCipher(k.material[:])
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		r.aeads[k.ID] = aead
		r.ids = append(r.ids, k.ID)
		if k.ID == r.primary {
			mac := hmac.New(sha256.New, k.material[:])
			mac.Write([]byte("tide-derive-v1"))
			r.deriveKey = mac.Sum(nil)
		}
	}
	return r, nil
}

// Derive returns 32 bytes that only holders of the primary key can compute
// for (purpose, value): HMAC-SHA256 with a key derived from the primary key.
// Every instance with the same TIDE_SECRET_KEYS derives the same bytes, so
// for example the OwnSand binding salt of a relayed file operation is the
// same when another instance continues the operation after a deployment.
// The bytes change when the primary key changes.
func (r *Keyring) Derive(purpose, value string) []byte {
	mac := hmac.New(sha256.New, r.deriveKey)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return mac.Sum(nil)
}

// ParseKeys parses the TIDE_SECRET_KEYS format: a comma-separated list of
// `<key ID>:<key>` entries, where the key is 32 bytes in base64 (standard or
// URL alphabet, with or without padding). The first entry is the primary
// key. Spaces around entries are ignored. Error messages name the entry by
// its position or key ID, never by its key.
func ParseKeys(value string) (*Keyring, error) {
	var keys []Key
	for i, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, encoded, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("secrets: entry %d must have the form <key ID>:<base64 key>", i+1)
		}
		id = strings.TrimSpace(id)
		material, err := decodeKey(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("secrets: key %q must be %d bytes in base64", id, KeySize)
		}
		k, err := NewKey(id, material)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return NewKeyring(keys...)
}

func decodeKey(s string) ([]byte, error) {
	trimmed := strings.TrimRight(s, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(trimmed); err == nil {
			if len(b) != KeySize {
				return nil, errors.New("wrong key size")
			}
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}

// PrimaryKeyID returns the ID of the key that Seal uses.
func (r *Keyring) PrimaryKeyID() string { return r.primary }

// KeyIDs returns the configured key IDs, primary first.
func (r *Keyring) KeyIDs() []string { return append([]string(nil), r.ids...) }

// Sealed is an encrypted value and the ID of its key. Ciphertext is the GCM
// nonce followed by the GCM output (ciphertext and tag).
type Sealed struct {
	KeyID      string
	Ciphertext []byte
}

// AssociatedData joins the parts that name the owner of a sealed value, for
// example AssociatedData("ownsand_link", organizationID). The parts are
// separated by a zero byte, so ("a", "bc") and ("ab", "c") differ.
func AssociatedData(parts ...string) []byte {
	return []byte(strings.Join(parts, "\x00"))
}

// Seal encrypts plaintext with the primary key. associatedData is
// authenticated but not encrypted; Open needs the same value.
func (r *Keyring) Seal(plaintext, associatedData []byte) (Sealed, error) {
	aead := r.aeads[r.primary]
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyID: r.primary, Ciphertext: aead.Seal(nonce, nonce, plaintext, associatedData)}, nil
}

// Open decrypts a sealed value with the key that it names.
func (r *Keyring) Open(s Sealed, associatedData []byte) ([]byte, error) {
	aead, ok := r.aeads[s.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	n := aead.NonceSize()
	if len(s.Ciphertext) < n+aead.Overhead() {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, s.Ciphertext[:n], s.Ciphertext[n:], associatedData)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// NeedsReseal reports whether s uses a key other than the primary key.
func (r *Keyring) NeedsReseal(s Sealed) bool { return s.KeyID != r.primary }

// Reseal re-encrypts s with the primary key when it uses another key, and
// reports whether it did. A value of the primary key is returned unchanged.
// Re-encryption jobs call it for each stored value and write the result back
// only when changed is true.
func (r *Keyring) Reseal(s Sealed, associatedData []byte) (out Sealed, changed bool, err error) {
	if !r.NeedsReseal(s) {
		return s, false, nil
	}
	plaintext, err := r.Open(s, associatedData)
	if err != nil {
		return Sealed{}, false, err
	}
	defer clear(plaintext)
	out, err = r.Seal(plaintext, associatedData)
	if err != nil {
		return Sealed{}, false, err
	}
	return out, true, nil
}

// MarshalBinary encodes s for a single bytea column: a version byte, the
// key ID length, the key ID and the ciphertext.
func (s Sealed) MarshalBinary() ([]byte, error) {
	if !validKeyID(s.KeyID) {
		return nil, ErrMalformed
	}
	var b bytes.Buffer
	b.Grow(2 + len(s.KeyID) + len(s.Ciphertext))
	b.WriteByte(envelopeVersion)
	b.WriteByte(byte(len(s.KeyID)))
	b.WriteString(s.KeyID)
	b.Write(s.Ciphertext)
	return b.Bytes(), nil
}

// UnmarshalBinary decodes the output of MarshalBinary.
func (s *Sealed) UnmarshalBinary(data []byte) error {
	if len(data) < 2 || data[0] != envelopeVersion {
		return ErrMalformed
	}
	n := int(data[1])
	if len(data) < 2+n {
		return ErrMalformed
	}
	id := string(data[2 : 2+n])
	if !validKeyID(id) {
		return ErrMalformed
	}
	s.KeyID = id
	s.Ciphertext = append([]byte(nil), data[2+n:]...)
	return nil
}
