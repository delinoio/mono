// SPDX-License-Identifier: FSL-1.1-ALv2

// Package releasekeys holds the release public keys that are built into the
// agent. The agent verifies the signed release manifest with them (package
// update), and the release manifest tool (cmds/ownsand-agent/release-manifest)
// refuses to sign or extend a manifest with a key that is not in this list,
// so a published manifest always verifies with the built-in keys of the
// agents that read it.
//
// The package imports only the standard library, because the release
// manifest tool runs in the release signing job next to the signing key.
//
// Key rotation: a release signed with the current key ships an agent that
// lists both the current and the next key, so that the next release can be
// signed with the next key. See docs/cmds-ownsand-agent-foundation.md
// (Agent updates) for the key custody process.
package releasekeys

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// encoded lists the base64 (standard alphabet, padded) Ed25519 release
// public keys.
//
// The entry is the public key of the release key pair from the release
// provisioning checklist in docs/repository-workflow-contract.md. The
// private key is the OWNSAND_AGENT_RELEASE_SIGNING_KEY secret of the
// releases environment. Rotate it according to the key custody rules in
// docs/cmds-ownsand-agent-foundation.md.
var encoded = []string{
	"noI2kKbgHxh3ayBRHhQ5f+UL3NW0gxvbgbT+ew/1/+I=",
}

// PublicKeys returns the built-in release public keys. It fails when an entry
// is not a base64 Ed25519 public key.
func PublicKeys() ([]ed25519.PublicKey, error) {
	return Decode(encoded)
}

// Decode decodes base64 (standard alphabet, padded) Ed25519 public keys.
func Decode(values []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(values))
	for i, value := range values {
		raw, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("releasekeys: entry %d is not a base64 Ed25519 public key", i)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// Encode returns the base64 form of a public key that this package lists.
func Encode(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}
