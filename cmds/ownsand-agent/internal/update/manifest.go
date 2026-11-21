// SPDX-License-Identifier: Apache-2.0

// Package update implements the signed agent update flow: it downloads the
// signed release manifest and the release artifact of the platform,
// verifies the manifest signature with the release public keys and the
// artifact SHA-256, waits until the device is idle and installs the new
// executable.
//
// Release signing (recorded decision; the signing tool and key custody stay
// open): the manifest is JSON, and its signature is a detached Ed25519
// signature over the exact manifest bytes, served base64-encoded at the
// manifest URL plus ".sig". BuiltinKeys is empty until the release process
// has key custody; until then an operator configures the public keys with
// OWNSAND_AGENT_RELEASE_PUBLIC_KEYS, and without any key every update fails
// verification and reports FAILED.
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/delinoio/mono/internal/semver"
)

// BuiltinKeys are the release public keys built into the agent. A release
// signed with the current key can introduce the next key by shipping an
// agent with both keys here.
var BuiltinKeys []ed25519.PublicKey

// ManifestSchema is the manifest schema version this agent reads.
const ManifestSchema = 1

// Manifest is the signed release manifest.
type Manifest struct {
	Schema          int       `json:"schema"`
	Releases        []Release `json:"releases"`
	RevokedVersions []string  `json:"revoked_versions"`
}

// Release is one agent version.
type Release struct {
	Version string `json:"version"`
	// Default marks the default target of its platforms.
	Default   bool       `json:"default"`
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact is the agent executable of one platform.
type Artifact struct {
	// Platform is GOOS-GOARCH, for example "linux-amd64".
	Platform string `json:"platform"`
	URL      string `json:"url"`
	// SHA256 is the lowercase hex SHA-256 of the executable.
	SHA256 string `json:"sha256"`
}

// Verification errors.
var (
	ErrBadSignature = errors.New("update: release manifest signature does not verify")
	ErrNoKeys       = errors.New("update: no release public key is configured")
	ErrHashMismatch = errors.New("update: release artifact SHA-256 does not match the manifest")
	ErrRevoked      = errors.New("update: target version is revoked")
	ErrNoArtifact   = errors.New("update: manifest has no artifact of this platform for the target version")
)

// DecodeSignature decodes the base64 content of a .sig file.
func DecodeSignature(b []byte) ([]byte, error) {
	s := strings.TrimSpace(string(b))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if sig, err := enc.DecodeString(s); err == nil && len(sig) == ed25519.SignatureSize {
			return sig, nil
		}
	}
	return nil, ErrBadSignature
}

// Verify checks the signature of the manifest bytes with any of keys and
// decodes the manifest.
func Verify(manifest, signature []byte, keys []ed25519.PublicKey) (*Manifest, error) {
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	ok := false
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, manifest, signature) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("update: decode manifest: %w", err)
	}
	if m.Schema != ManifestSchema {
		return nil, fmt.Errorf("update: unsupported manifest schema %d", m.Schema)
	}
	return &m, nil
}

// Revoked reports whether v is a revoked version.
func (m *Manifest) Revoked(v string) bool {
	for _, r := range m.RevokedVersions {
		if c, err := semver.CompareStrings(r, v); err == nil && c == 0 {
			return true
		}
	}
	return false
}

// Artifact returns the artifact of platform for version v.
func (m *Manifest) Artifact(v, platform string) (Artifact, error) {
	for _, r := range m.Releases {
		if c, err := semver.CompareStrings(r.Version, v); err != nil || c != 0 {
			continue
		}
		for _, a := range r.Artifacts {
			if a.Platform == platform {
				return a, nil
			}
		}
	}
	return Artifact{}, ErrNoArtifact
}
