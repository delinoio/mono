// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
)

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestEnvNamesPinTheDocumentedVariables(t *testing.T) {
	want := []string{
		"OWNSAND_AGENT_STATE_DIR",
		"OWNSAND_AGENT_API_URL",
		"OWNSAND_AGENT_RUNTIME",
		"OWNSAND_AGENT_GUEST_BINARY",
		"OWNSAND_AGENT_RELEASE_MANIFEST_URL",
		"OWNSAND_AGENT_RELEASE_PUBLIC_KEYS",
		"OWNSAND_AGENT_LOG_LEVEL",
		"OWNSAND_AGENT_FIRECRACKER_BINARY",
		"OWNSAND_AGENT_LINUX_KERNEL",
		"OWNSAND_AGENT_LINUX_ROOTFS",
	}
	if !slices.Equal(EnvNames, want) {
		t.Fatalf("EnvNames = %v, want %v", EnvNames, want)
	}
}

func TestFromEnvLogLevel(t *testing.T) {
	valid := map[string]string{
		"debug": "DEBUG",
		"DEBUG": "DEBUG",
		"Warn":  "WARN",
		"Error": "ERROR",
		"iNfO":  "INFO",
		" info": "INFO", // FromEnv trims white space first
	}
	for value, want := range valid {
		cfg, err := FromEnv(env(map[string]string{EnvLogLevel: value}))
		if err != nil {
			t.Errorf("log level %q rejected: %v", value, err)
			continue
		}
		if cfg.LogLevel.String() != want {
			t.Errorf("log level %q = %s, want %s", value, cfg.LogLevel, want)
		}
	}
	for _, value := range []string{"ınfo", "ınfo+1", "info+1", "warn-2", "İNFO", "verbose"} {
		_, err := FromEnv(env(map[string]string{EnvLogLevel: value}))
		if err == nil {
			t.Errorf("log level %q accepted", value)
			continue
		}
		want := fmt.Sprintf("config: invalid %s %q", EnvLogLevel, value)
		if err.Error() != want {
			t.Errorf("log level %q error = %q, want %q", value, err, want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cfg, err := FromEnv(env(map[string]string{
		EnvStateDir:          "/tmp/x",
		EnvRuntime:           "process",
		EnvReleasePublicKeys: base64.StdEncoding.EncodeToString(pub) + ", " + base64.RawURLEncoding.EncodeToString(pub),
		EnvLogLevel:          "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/tmp/x" || cfg.Runtime != RuntimeKindProcess || len(cfg.ReleasePublicKeys) != 2 || cfg.LogLevel.String() != "DEBUG" {
		t.Fatalf("config %+v", cfg)
	}
	if _, err := FromEnv(env(map[string]string{EnvRuntime: "qemu"})); err == nil {
		t.Fatal("unknown runtime accepted")
	}
	if _, err := FromEnv(env(map[string]string{EnvReleasePublicKeys: "abc"})); err == nil {
		t.Fatal("bad key accepted")
	}
	cfg, _ = FromEnv(env(nil))
	if cfg.StateDir != DefaultStateDir() || cfg.Runtime != DefaultRuntime() || cfg.ReleaseManifestURL != DefaultReleaseManifestURL {
		t.Fatalf("defaults %+v", cfg)
	}
	cfg, _ = FromEnv(env(map[string]string{EnvReleaseManifestURL: " https://example.test/manifest.json "}))
	if cfg.ReleaseManifestURL != "https://example.test/manifest.json" {
		t.Fatalf("manifest URL override %q", cfg.ReleaseManifestURL)
	}
}

// The release workflow publishes the manifest at the URL in the package
// manifest; the default must name the same object.
func TestDefaultReleaseManifestURLMatchesThePackageManifest(t *testing.T) {
	b, err := os.ReadFile("../../package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Deploy struct {
			UpdateManifest struct {
				PublicURL string `json:"publicUrl"`
			} `json:"updateManifest"`
		} `json:"deploy"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	if got := pkg.Deploy.UpdateManifest.PublicURL; got != DefaultReleaseManifestURL {
		t.Fatalf("package.json deploy.updateManifest.publicUrl = %q, DefaultReleaseManifestURL = %q", got, DefaultReleaseManifestURL)
	}
}
