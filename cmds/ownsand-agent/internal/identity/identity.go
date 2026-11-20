// SPDX-License-Identifier: Apache-2.0

// Package identity holds the device key pair and the signatures of the
// agent channel: Ed25519 over a purpose label followed by the nonce.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
)

// Purpose labels of signed nonces, as the comments of agent.proto define.
const (
	PurposeAuthenticate   = "ownsand-authenticate"
	PurposeConfirmRemoval = "ownsand-confirm-removal"
)

// Key is the device key pair. Secret: never log it.
type Key struct {
	private ed25519.PrivateKey
}

// Generate creates a new device key pair.
func Generate() (*Key, []byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return &Key{private: priv}, priv.Seed(), nil
}

// FromSeed restores the key from its 32-byte seed.
func FromSeed(seed []byte) (*Key, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity: device key seed must be 32 bytes")
	}
	return &Key{private: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicKey returns the 32-byte public key.
func (k *Key) PublicKey() ed25519.PublicKey {
	return k.private.Public().(ed25519.PublicKey)
}

// Message returns the signed bytes: the purpose label followed by the nonce.
func Message(purpose string, nonce []byte) []byte {
	return append([]byte(purpose), nonce...)
}

// Sign signs the purpose label followed by nonce.
func (k *Key) Sign(purpose string, nonce []byte) []byte {
	return ed25519.Sign(k.private, Message(purpose, nonce))
}
