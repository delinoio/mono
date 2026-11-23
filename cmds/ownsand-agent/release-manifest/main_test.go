// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/releasekeys"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
)

const urlPrefix = "https://github.com/delinoio/mono/releases/download/ownsand-agent%400.1.0"

type fixture struct {
	dir     string
	keyFile string
	pemText string
	pub     ed25519.PublicKey
}

// newFixture generates a key pair with `keygen` and makes its public key the
// only built-in key.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, keyFile: filepath.Join(dir, "release.pem")}
	var out bytes.Buffer
	if err := run([]string{"keygen", f.keyFile}, nil, &out, os.Getenv, quietLogger()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits: Go reports a writable file as
	// 0666 there, so only Linux and macOS can check the mode.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v, want 0600", info.Mode().Perm())
	}
	pemBytes, err := os.ReadFile(f.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	f.pemText = string(pemBytes)
	keys, err := releasekeys.Decode([]string{strings.TrimSpace(out.String())})
	if err != nil {
		t.Fatal(err)
	}
	f.pub = keys[0]
	setBuiltinKeys(t, f.pub)
	return f
}

func setBuiltinKeys(t *testing.T, keys ...ed25519.PublicKey) {
	t.Helper()
	previous := builtinKeys
	builtinKeys = func() ([]ed25519.PublicKey, error) { return keys, nil }
	t.Cleanup(func() { builtinKeys = previous })
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (f *fixture) run(t *testing.T, args ...string) error {
	t.Helper()
	getenv := func(name string) string {
		if name == EnvSigningKey {
			return f.pemText
		}
		return ""
	}
	return run(args, nil, io.Discard, getenv, quietLogger())
}

// artifacts writes one executable per platform and returns the directory.
func (f *fixture) artifacts(t *testing.T, name, content string, ps ...Platform) string {
	t.Helper()
	dir := filepath.Join(f.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if err := os.WriteFile(filepath.Join(dir, assetName("ownsand-agent", p)), []byte(content+string(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (f *fixture) addRelease(t *testing.T, current, version, artifactDir, out string, ps string) error {
	t.Helper()
	args := []string{"add-release", "--version", version, "--binary", "ownsand-agent", "--platforms", ps,
		"--artifacts", artifactDir, "--url-prefix", urlPrefix, "--out", out}
	if current != "" {
		args = append(args, "--current", current, "--current-signature", current+".sig")
	}
	return f.run(t, args...)
}

func TestManifestSchemaMatchesTheAgent(t *testing.T) {
	if manifestSchema != update.ManifestSchema {
		t.Fatalf("manifestSchema = %d, update.ManifestSchema = %d", manifestSchema, update.ManifestSchema)
	}
}

func TestReleaseRoundTripVerifiesWithTheAgent(t *testing.T) {
	f := newFixture(t)
	all := "linux-amd64,linux-arm64,darwin-arm64,windows-amd64,windows-arm64"
	first := filepath.Join(f.dir, "first.json")
	art1 := f.artifacts(t, "a1", "one", PlatformLinuxAmd64, PlatformLinuxArm64, PlatformDarwinArm64, PlatformWindowsAmd64, PlatformWindowsArm64)
	if err := f.addRelease(t, "", "0.1.0", art1, first, all); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "sign", "--manifest", first, "--out", first+".sig"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "verify", "--manifest", first, "--signature", first+".sig"); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(f.dir, "second.json")
	art2 := f.artifacts(t, "a2", "two", PlatformLinuxAmd64, PlatformLinuxArm64, PlatformDarwinArm64, PlatformWindowsAmd64, PlatformWindowsArm64)
	if err := f.addRelease(t, first, "0.2.0", art2, second, all); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "sign", "--manifest", second, "--out", second+".sig"); err != nil {
		t.Fatal(err)
	}

	manifestBytes, _ := os.ReadFile(second)
	sigText, _ := os.ReadFile(second + ".sig")
	sig, err := update.DecodeSignature(sigText)
	if err != nil {
		t.Fatal(err)
	}
	m, err := update.Verify(manifestBytes, sig, []ed25519.PublicKey{f.pub})
	if err != nil {
		t.Fatalf("the agent rejects the manifest: %v", err)
	}
	if len(m.Releases) != 2 || m.Releases[0].Default || !m.Releases[1].Default {
		t.Fatalf("releases %+v, want 0.1.0 then the default 0.2.0", m.Releases)
	}
	if m.RevokedVersions == nil {
		t.Fatal("revoked_versions is null")
	}
	a, err := m.Artifact("0.2.0", "windows-arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("twowindows-arm64"))
	if a.SHA256 != hex.EncodeToString(want[:]) || a.URL != urlPrefix+"/ownsand-agent-windows-arm64.exe" {
		t.Fatalf("artifact %+v", a)
	}
	if _, err := m.Artifact("0.1.0", "darwin-arm64"); err != nil {
		t.Fatalf("the earlier release was dropped: %v", err)
	}
}

func TestAddReleaseKeepsRevokedVersionsAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	current := filepath.Join(f.dir, "current.json")
	art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
	seed := []byte(`{"schema":1,"releases":[{"version":"0.1.0","default":true,"artifacts":[{"platform":"linux-amd64","url":"https://example.test/a","sha256":"` + strings.Repeat("a", 64) + `"}]}],"revoked_versions":["0.0.9"]}`)
	if err := os.WriteFile(current, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "sign", "--manifest", current, "--out", current+".sig"); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(f.dir, "next.json")
	if err := f.addRelease(t, current, "0.2.0", art, next, "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	var m manifest
	b, _ := os.ReadFile(next)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.RevokedVersions) != 1 || m.RevokedVersions[0] != "0.0.9" || len(m.Releases) != 2 {
		t.Fatalf("manifest %+v", m)
	}
	// A rerun of the publish job adds the same release to the manifest that
	// it already uploaded: the result is byte for byte the same.
	if err := f.run(t, "sign", "--manifest", next, "--out", next+".sig"); err != nil {
		t.Fatal(err)
	}
	rerun := filepath.Join(f.dir, "rerun.json")
	if err := f.addRelease(t, next, "0.2.0", art, rerun, "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(rerun)
	if !bytes.Equal(again, b) {
		t.Fatalf("rerun changed the manifest:\n%s\nwant\n%s", again, b)
	}
}

func TestAddReleaseRejections(t *testing.T) {
	f := newFixture(t)
	current := filepath.Join(f.dir, "current.json")
	seed := []byte(`{"schema":1,"releases":[{"version":"0.3.0","default":true,"artifacts":[{"platform":"linux-amd64","url":"https://example.test/a","sha256":"` + strings.Repeat("a", 64) + `"}]}],"revoked_versions":["0.4.0"]}`)
	os.WriteFile(current, seed, 0o644)
	if err := f.run(t, "sign", "--manifest", current, "--out", current+".sig"); err != nil {
		t.Fatal(err)
	}
	art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
	out := filepath.Join(f.dir, "out.json")
	cases := []struct {
		version string
		want    error
	}{
		{"0.2.0", errNewerRelease},
		{"0.3.0", errChangedRelease},
		{"0.4.0", errRevokedRelease},
	}
	for _, c := range cases {
		if err := f.addRelease(t, current, c.version, art, out, "linux-amd64"); !errors.Is(err, c.want) {
			t.Errorf("version %s: error %v, want %v", c.version, err, c.want)
		}
	}
	for _, version := range []string{"v1.0.0", "1.0.0-rc.1", "1.0", "01.0.0"} {
		if err := f.addRelease(t, current, version, art, out, "linux-amd64"); err == nil {
			t.Errorf("version %q accepted", version)
		}
	}
	// A missing executable fails.
	if err := f.addRelease(t, current, "1.0.0", art, out, "linux-amd64,linux-arm64"); err == nil {
		t.Error("missing artifact accepted")
	}
	// A tampered manifest is never extended.
	os.WriteFile(current, append(seed, ' '), 0o644)
	if err := f.addRelease(t, current, "1.0.0", art, out, "linux-amd64"); !errors.Is(err, errUnverified) {
		t.Errorf("tampered manifest: error %v, want %v", err, errUnverified)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a rejected release wrote %s", out)
	}
}

func TestReleaseVersionRejectsOversizedComponents(t *testing.T) {
	const oversized = "9223372036854775808"
	for _, v := range []string{oversized + ".0.0", "1." + oversized + ".0", "1.0." + oversized} {
		if err := checkReleaseVersion(v); err == nil {
			t.Errorf("checkReleaseVersion(%q) accepted an oversized component", v)
		}
		manifestJSON := `{"schema":1,"releases":[{"version":"` + v + `","default":true,"artifacts":[{"platform":"linux-amd64","url":"https://example.test/a","sha256":"` + strings.Repeat("a", 64) + `"}]}],"revoked_versions":[]}`
		if _, err := decodeManifest([]byte(manifestJSON)); err == nil {
			t.Errorf("decodeManifest accepted release version %q", v)
		}

		// The first release has no current manifest to compare with, so
		// only the version check can refuse it, and it must refuse before
		// the tool writes any output.
		f := newFixture(t)
		art := f.artifacts(t, "a", "x", PlatformLinuxAmd64)
		out := filepath.Join(f.dir, "out.json")
		if err := f.addRelease(t, "", v, art, out, "linux-amd64"); err == nil {
			t.Errorf("first release %q accepted", v)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("a rejected first release %q wrote %s", v, out)
		}
	}
	for _, v := range []string{"0.0.0", "1.2.3", "10.20.30", "9223372036854775807.0.0"} {
		if err := checkReleaseVersion(v); err != nil {
			t.Errorf("checkReleaseVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"v1.2.3", "1.2.3-rc.1", "01.2.3", "1.2", "1.2.3.4", ""} {
		if err := checkReleaseVersion(v); err == nil {
			t.Errorf("checkReleaseVersion(%q) accepted an invalid spelling", v)
		}
	}
}

func TestDecodeManifestRejectsUnknownFields(t *testing.T) {
	if _, err := decodeManifest([]byte(`{"schema":1,"releases":[],"revoked_versions":[],"channels":[]}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := decodeManifest([]byte(`{"schema":2,"releases":[],"revoked_versions":[]}`)); err == nil {
		t.Fatal("schema 2 accepted")
	}
}

func TestSignRequiresABuiltinKey(t *testing.T) {
	f := newFixture(t)
	manifestPath := filepath.Join(f.dir, "m.json")
	b, _ := emptyManifest().encode()
	os.WriteFile(manifestPath, b, 0o644)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	setBuiltinKeys(t, other)
	if err := f.run(t, "sign", "--manifest", manifestPath, "--out", manifestPath+".sig"); !errors.Is(err, errKeyNotBuiltin) {
		t.Fatalf("error %v, want %v", err, errKeyNotBuiltin)
	}
	setBuiltinKeys(t)
	if err := f.run(t, "sign", "--manifest", manifestPath, "--out", manifestPath+".sig"); !errors.Is(err, errNoBuiltinKeys) {
		t.Fatalf("error %v, want %v", err, errNoBuiltinKeys)
	}
	if err := f.run(t, "check-keys"); !errors.Is(err, errNoBuiltinKeys) {
		t.Fatalf("check-keys error %v, want %v", err, errNoBuiltinKeys)
	}
	f.pemText = ""
	setBuiltinKeys(t, f.pub)
	if err := f.run(t, "sign", "--manifest", manifestPath, "--out", manifestPath+".sig"); err == nil || !strings.Contains(err.Error(), EnvSigningKey) {
		t.Fatalf("missing key error %v", err)
	}
}

func TestPublicKeyMatchesKeygen(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	if err := run([]string{"public-key"}, strings.NewReader(f.pemText), &out, os.Getenv, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != releasekeys.Encode(f.pub) {
		t.Fatalf("public-key printed %q", out.String())
	}
	// keygen never overwrites a key.
	if err := run([]string{"keygen", f.keyFile}, nil, io.Discard, os.Getenv, quietLogger()); err == nil {
		t.Fatal("keygen overwrote an existing file")
	}
}

func TestUsageErrors(t *testing.T) {
	var usage usageError
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"keygen"},
		{"sign", "--manifest", "m.json"},
		{"add-release", "--version", "1.0.0"},
		{"verify", "--manifest", "m", "--signature", "s", "extra"},
	} {
		if err := run(args, nil, io.Discard, os.Getenv, quietLogger()); !errors.As(err, &usage) {
			t.Errorf("args %q: error %v, want a usage error", args, err)
		}
	}
}
