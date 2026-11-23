// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/delinoio/mono/internal/semver"
)

// manifestSchema is the manifest schema that this tool writes. It must equal
// update.ManifestSchema; the tests check it.
const manifestSchema = 1

// maxManifestBytes is the largest manifest that the agent downloads
// (update.maxManifestBytes). A larger manifest would stop every update.
const maxManifestBytes = 1 << 20

// manifest mirrors update.Manifest. The tool does not import package update,
// because that package pulls in the generated protocol bindings and their
// third-party modules, and the tool runs next to the signing key. The tests
// check that every manifest the tool writes verifies and decodes with
// package update.
type manifest struct {
	Schema          int       `json:"schema"`
	Releases        []release `json:"releases"`
	RevokedVersions []string  `json:"revoked_versions"`
}

type release struct {
	Version   string     `json:"version"`
	Default   bool       `json:"default"`
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

// Platform is a release platform, `<GOOS>-<GOARCH>`.
type Platform string

// Supported release platforms. The planner (scripts/ci/deploy-plan.mjs)
// accepts the same set.
const (
	PlatformLinuxAmd64   Platform = "linux-amd64"
	PlatformLinuxArm64   Platform = "linux-arm64"
	PlatformDarwinAmd64  Platform = "darwin-amd64"
	PlatformDarwinArm64  Platform = "darwin-arm64"
	PlatformWindowsAmd64 Platform = "windows-amd64"
	PlatformWindowsArm64 Platform = "windows-arm64"
)

var platforms = []Platform{
	PlatformLinuxAmd64,
	PlatformLinuxArm64,
	PlatformDarwinAmd64,
	PlatformDarwinArm64,
	PlatformWindowsAmd64,
	PlatformWindowsArm64,
}

func parsePlatform(s string) (Platform, error) {
	for _, p := range platforms {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("unsupported platform %q", s)
}

// assetName is the release asset name of the executable of a platform. The
// release workflow (scripts/ci/release-binaries.sh) uses the same names.
func assetName(binary string, p Platform) string {
	if strings.HasPrefix(string(p), "windows-") {
		return binary + "-" + string(p) + ".exe"
	}
	return binary + "-" + string(p)
}

// releaseVersion is the version form that the agent accepts as an update
// target: MAJOR.MINOR.PATCH without a prefix or a pre-release suffix.
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// checkReleaseVersion reports whether v is a release version the agent can
// use as an update target. The spelling check alone accepts a component such
// as 9223372036854775808 that the agent's semver parser rejects as too large,
// so a manifest with such a version would pass the tool and then fail every
// comparison. The regular expression is stricter than semver.Parse (which also
// accepts a "v" prefix and a pre-release suffix), so both checks are needed.
func checkReleaseVersion(v string) error {
	if !releaseVersion.MatchString(v) {
		return fmt.Errorf("release version %q is not MAJOR.MINOR.PATCH", v)
	}
	if _, err := semver.Parse(v); err != nil {
		return fmt.Errorf("release version %q: %w", v, err)
	}
	return nil
}

var binaryName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Errors of the manifest update.
var (
	errNewerRelease    = errors.New("the manifest already lists a newer release")
	errChangedRelease  = errors.New("the manifest already lists this version with other artifacts")
	errRevokedRelease  = errors.New("the manifest revokes this version")
	errUnverified      = errors.New("the current manifest signature does not verify with a built-in release key")
	errNoBuiltinKeys   = errors.New("the agent has no built-in release public key; add one to cmds/ownsand-agent/internal/releasekeys")
	errKeyNotBuiltin   = errors.New("the signing key is not a built-in release key of the agent")
	errManifestTooLong = fmt.Errorf("the manifest is larger than %d bytes, the most that the agent downloads", maxManifestBytes)
)

// decodeManifest decodes and validates manifest bytes. Unknown fields fail,
// so that the tool never drops a field that a newer schema added.
func decodeManifest(b []byte) (*manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("decode manifest: data after the manifest object")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *manifest) validate() error {
	if m.Schema != manifestSchema {
		return fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	seen := map[string]bool{}
	for _, r := range m.Releases {
		if err := checkReleaseVersion(r.Version); err != nil {
			return err
		}
		if seen[r.Version] {
			return fmt.Errorf("release %s is listed twice", r.Version)
		}
		seen[r.Version] = true
		if len(r.Artifacts) == 0 {
			return fmt.Errorf("release %s has no artifacts", r.Version)
		}
		platformsSeen := map[string]bool{}
		for _, a := range r.Artifacts {
			if _, err := parsePlatform(a.Platform); err != nil {
				return fmt.Errorf("release %s: %w", r.Version, err)
			}
			if platformsSeen[a.Platform] {
				return fmt.Errorf("release %s lists platform %s twice", r.Version, a.Platform)
			}
			platformsSeen[a.Platform] = true
			if !strings.HasPrefix(a.URL, "https://") {
				return fmt.Errorf("release %s: artifact URL %q is not an https URL", r.Version, a.URL)
			}
			if !sha256Hex.MatchString(a.SHA256) {
				return fmt.Errorf("release %s: artifact SHA-256 %q is not lowercase hex", r.Version, a.SHA256)
			}
		}
	}
	for _, v := range m.RevokedVersions {
		if _, err := semver.Parse(v); err != nil {
			return fmt.Errorf("revoked version %q: %w", v, err)
		}
	}
	return nil
}

// encode returns the manifest bytes that the release workflow signs and
// uploads: indented JSON with a final newline.
func (m *manifest) encode() ([]byte, error) {
	if m.Releases == nil {
		m.Releases = []release{}
	}
	if m.RevokedVersions == nil {
		m.RevokedVersions = []string{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > maxManifestBytes {
		return nil, errManifestTooLong
	}
	return b, nil
}

func emptyManifest() *manifest {
	return &manifest{Schema: manifestSchema, Releases: []release{}, RevokedVersions: []string{}}
}

// releaseInput is one new release.
type releaseInput struct {
	Version   string
	Binary    string
	Platforms []Platform
	// ArtifactDir holds the executables, named by assetName.
	ArtifactDir string
	// URLPrefix is the download URL of the release assets, without a
	// trailing slash.
	URLPrefix string
}

// newRelease builds the release entry: the SHA-256 and download URL of the
// executable of every platform.
func newRelease(in releaseInput) (release, error) {
	if err := checkReleaseVersion(in.Version); err != nil {
		return release{}, err
	}
	if !binaryName.MatchString(in.Binary) {
		return release{}, fmt.Errorf("binary name %q is not a lowercase kebab-case name", in.Binary)
	}
	if len(in.Platforms) == 0 {
		return release{}, errors.New("no platforms")
	}
	prefix := strings.TrimSuffix(in.URLPrefix, "/")
	if !strings.HasPrefix(prefix, "https://") || strings.ContainsAny(prefix, " \t\r\n") {
		return release{}, fmt.Errorf("URL prefix %q is not an https URL", in.URLPrefix)
	}
	r := release{Version: in.Version, Default: true}
	seen := map[Platform]bool{}
	for _, p := range in.Platforms {
		if seen[p] {
			return release{}, fmt.Errorf("platform %s is listed twice", p)
		}
		seen[p] = true
		name := assetName(in.Binary, p)
		path := filepath.Join(in.ArtifactDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return release{}, fmt.Errorf("artifact of %s: %w", p, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return release{}, fmt.Errorf("artifact of %s (%s) is not a nonempty regular file", p, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return release{}, err
		}
		sum := sha256.Sum256(data)
		r.Artifacts = append(r.Artifacts, artifact{
			Platform: string(p),
			URL:      prefix + "/" + name,
			SHA256:   hex.EncodeToString(sum[:]),
		})
	}
	return r, nil
}

// addRelease adds r to m and makes it the only default release. It is
// idempotent: when m already lists r with the same artifacts (a rerun of the
// publish job), it only restores the default flags. It fails when m lists the
// version with other artifacts, lists a newer version or revokes the
// version, so a rerun of an old run never replaces a newer default.
func addRelease(m *manifest, r release) error {
	for _, v := range m.RevokedVersions {
		if c, err := semver.CompareStrings(v, r.Version); err == nil && c == 0 {
			return fmt.Errorf("%s: %w", r.Version, errRevokedRelease)
		}
	}
	index := -1
	for i, existing := range m.Releases {
		c, err := semver.CompareStrings(existing.Version, r.Version)
		if err != nil {
			return err
		}
		switch {
		case c > 0:
			return fmt.Errorf("%s is newer than %s: %w", existing.Version, r.Version, errNewerRelease)
		case c == 0:
			if !sameArtifacts(existing.Artifacts, r.Artifacts) {
				return fmt.Errorf("%s: %w", r.Version, errChangedRelease)
			}
			index = i
		}
	}
	if index < 0 {
		m.Releases = append(m.Releases, r)
		index = len(m.Releases) - 1
	}
	for i := range m.Releases {
		m.Releases[i].Default = i == index
	}
	return m.validate()
}

func sameArtifacts(a, b []artifact) bool {
	if len(a) != len(b) {
		return false
	}
	byPlatform := map[string]artifact{}
	for _, x := range a {
		byPlatform[x.Platform] = x
	}
	for _, y := range b {
		if x, ok := byPlatform[y.Platform]; !ok || x != y {
			return false
		}
	}
	return true
}

// verifySignature checks a base64 detached Ed25519 signature over the exact
// manifest bytes with any of keys, as update.Verify does.
func verifySignature(manifestBytes, sigText []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return errNoBuiltinKeys
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errUnverified
	}
	for _, k := range keys {
		if ed25519.Verify(k, manifestBytes, sig) {
			return nil
		}
	}
	return errUnverified
}

// sign returns the content of the `.sig` file: the base64 detached Ed25519
// signature of the manifest bytes and a newline. The key must be one of the
// built-in keys.
func sign(manifestBytes []byte, key ed25519.PrivateKey, keys []ed25519.PublicKey) ([]byte, error) {
	if len(keys) == 0 {
		return nil, errNoBuiltinKeys
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the signing key is not an Ed25519 key")
	}
	builtin := false
	for _, k := range keys {
		if k.Equal(pub) {
			builtin = true
			break
		}
	}
	if !builtin {
		return nil, errKeyNotBuiltin
	}
	sigText := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, manifestBytes)) + "\n")
	if err := verifySignature(manifestBytes, sigText, keys); err != nil {
		return nil, err
	}
	return sigText, nil
}
