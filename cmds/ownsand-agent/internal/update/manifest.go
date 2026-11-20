// Package update verifies signed release manifests and runs the agent update
// state machine: PENDING_IDLE, DOWNLOADING, INSTALLING, FAILED and
// REVOKED_PENDING_IDLE.
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ManifestSignatureContext prefixes the payload bytes that a release
// signature covers, so a manifest signature cannot be reused for another
// message.
const ManifestSignatureContext = "ownsand-release-manifest-v1\n"

// ReleaseKey is a public key that may sign release manifests.
type ReleaseKey struct {
	ID        string
	PublicKey ed25519.PublicKey
}

// BuiltinReleaseKeys is the list of public keys built into the agent. A
// release signed with the current key can ship the next key in this list, so
// keys rotate without a gap.
//
// The release signing tool and the key custody process are open decisions of
// the agent contract, so no production key exists yet and the list is empty:
// every manifest fails verification and every update reports FAILED. Add the
// first key here when the key custody process is recorded.
var BuiltinReleaseKeys = []ReleaseKey{}

// SignedManifest is the file that the release host serves.
type SignedManifest struct {
	// Payload is the base64 encoding of the Manifest JSON.
	Payload    string      `json:"payload"`
	Signatures []Signature `json:"signatures"`
}

// Signature is one Ed25519 signature over ManifestSignatureContext followed
// by the decoded payload bytes.
type Signature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// Manifest is the signed content.
type Manifest struct {
	Releases []Release `json:"releases"`
	// Revoked lists the revoked agent versions.
	Revoked []string `json:"revoked"`
}

// Release is one agent version.
type Release struct {
	Version string `json:"version"`
	// Default marks the default target version of the release channel.
	Default   bool                `json:"default"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Artifact is the release artifact of one platform ("<goos>-<goarch>").
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// ErrBadSignature reports a manifest without a valid signature of a
// built-in key.
var ErrBadSignature = errors.New("release manifest signature is not valid for any built-in key")

// VerifyManifest checks the signatures and decodes the manifest.
func VerifyManifest(data []byte, keys []ReleaseKey) (*Manifest, error) {
	var sm SignedManifest
	if err := json.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("decode signed manifest: %w", err)
	}
	payload, err := base64.StdEncoding.DecodeString(sm.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode manifest payload: %w", err)
	}
	msg := append([]byte(ManifestSignatureContext), payload...)
	verified := false
	for _, s := range sm.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if k.ID == s.KeyID && len(k.PublicKey) == ed25519.PublicKeySize && ed25519.Verify(k.PublicKey, msg, sig) {
				verified = true
			}
		}
	}
	if !verified {
		return nil, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// SignManifest signs a manifest. Tests and the future release tool use it.
func SignManifest(m *Manifest, keyID string, key ed25519.PrivateKey) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, append([]byte(ManifestSignatureContext), payload...))
	return json.Marshal(SignedManifest{
		Payload:    base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: keyID, Signature: base64.StdEncoding.EncodeToString(sig)}},
	})
}

// IsRevoked reports whether version is revoked.
func (m *Manifest) IsRevoked(version string) bool {
	for _, v := range m.Revoked {
		if v == version {
			return true
		}
	}
	return false
}

// Release returns the release of a version.
func (m *Manifest) Release(version string) (*Release, bool) {
	for i := range m.Releases {
		if m.Releases[i].Version == version {
			return &m.Releases[i], true
		}
	}
	return nil, false
}

// DefaultVersion returns the default target version, or "".
func (m *Manifest) DefaultVersion() string {
	for _, r := range m.Releases {
		if r.Default {
			return r.Version
		}
	}
	return ""
}

// ArtifactFor returns the artifact of a platform and its decoded hash.
func (r *Release) ArtifactFor(platform string) (Artifact, []byte, error) {
	a, ok := r.Artifacts[platform]
	if !ok {
		return Artifact{}, nil, fmt.Errorf("release %s has no artifact for %s", r.Version, platform)
	}
	sum, err := hex.DecodeString(a.SHA256)
	if err != nil || len(sum) != 32 {
		return Artifact{}, nil, fmt.Errorf("release %s has an invalid artifact hash", r.Version)
	}
	return a, sum, nil
}

// CompareVersions compares two dotted numeric versions ("1.2.3", optionally
// with a "-suffix" that sorts before the plain version). It returns -1, 0
// or 1.
func CompareVersions(a, b string) int {
	ac, as, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bc, bs, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	ap, bp := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < max(len(ap), len(bp)); i++ {
		var x, y int
		if i < len(ap) {
			x, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			y, _ = strconv.Atoi(bp[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case as == bs:
		return 0
	case as == "":
		return 1
	case bs == "":
		return -1
	case as < bs:
		return -1
	default:
		return 1
	}
}
