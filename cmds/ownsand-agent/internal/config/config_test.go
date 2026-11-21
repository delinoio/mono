// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
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
	if cfg.StateDir != DefaultStateDir() || cfg.Runtime != DefaultRuntime() {
		t.Fatalf("defaults %+v", cfg)
	}
}
