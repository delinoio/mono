// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func baseEnv(t *testing.T) map[string]string {
	return map[string]string{
		EnvDatabaseURL:  "postgres://tide@127.0.0.1/tide",
		EnvLogtoIssuer:  "https://tenant.logto.app/oidc",
		EnvSecretKeys:   "k1:" + testKey(t),
		"UNRELATED_VAR": "x",
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(lookup(baseEnv(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:18400" || cfg.LogLevel != LogLevelInfo || !cfg.RunMigrations {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.LogtoAudience != "https://tide.api.delino.io" || cfg.LogtoJWKSURL != "https://tenant.logto.app/oidc/jwks" {
		t.Fatalf("logto defaults = %q %q", cfg.LogtoAudience, cfg.LogtoJWKSURL)
	}
	if !slices.Equal(cfg.CORSOrigins, []string{"https://tide.delino.io"}) {
		t.Fatalf("cors = %v", cfg.CORSOrigins)
	}
	if cfg.ClientAddressMode != ClientAddressNone || cfg.MetricsAddr != "" {
		t.Fatalf("mode = %q metrics = %q", cfg.ClientAddressMode, cfg.MetricsAddr)
	}
	if cfg.SecretKeys == nil || cfg.SecretKeys.PrimaryKeyID() != "k1" {
		t.Fatal("secret keys not parsed")
	}
	if cfg.OwnSandAPIOrigin != "https://ownsand.api.delino.io" || cfg.PublicOrigin != "https://tide.api.delino.io" {
		t.Fatalf("origins = %q %q", cfg.OwnSandAPIOrigin, cfg.PublicOrigin)
	}
}

func TestOverrides(t *testing.T) {
	env := baseEnv(t)
	env[EnvAddr] = ":8080"
	env[EnvLogLevel] = " DEBUG "
	env[EnvRunMigrations] = "false"
	env[EnvLogtoJWKSFile] = "/tmp/jwks.json"
	env[EnvLogtoAudience] = "https://tide.example"
	env[EnvCORSOrigins] = "https://tide.delino.io, http://127.0.0.1:13400"
	env[EnvTrustProxyClientAddress] = "true"
	env[EnvAllowDirectClientAddress] = "true"
	env[EnvMetricsAddr] = "0.0.0.0:9400"
	env[EnvSecretKeys] = "new:" + testKey(t) + ",old:" + testKey(t)
	env[EnvOwnSandAPIOrigin] = "http://127.0.0.1:18300/"
	env[EnvPublicOrigin] = "https://tide.example"
	cfg, err := Load(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.LogLevel != LogLevelDebug || cfg.RunMigrations || cfg.LogtoJWKSURL != "" || cfg.LogtoJWKSFile != "/tmp/jwks.json" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.LogtoAudience != "https://tide.example" || len(cfg.CORSOrigins) != 2 || cfg.MetricsAddr != "0.0.0.0:9400" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.OwnSandAPIOrigin != "http://127.0.0.1:18300" || cfg.PublicOrigin != "https://tide.example" {
		t.Fatalf("origins = %q %q", cfg.OwnSandAPIOrigin, cfg.PublicOrigin)
	}
	if cfg.ClientAddressMode != ClientAddressVerifiedProxy {
		t.Fatalf("mode = %q", cfg.ClientAddressMode)
	}
	if strings.Join(cfg.SecretKeys.KeyIDs(), ",") != "new,old" {
		t.Fatalf("keys = %v", cfg.SecretKeys.KeyIDs())
	}
	delete(env, EnvTrustProxyClientAddress)
	if cfg, err = Load(lookup(env)); err != nil || cfg.ClientAddressMode != ClientAddressDirect {
		t.Fatalf("direct mode = %q, %v", cfg.ClientAddressMode, err)
	}
}

func TestInvalid(t *testing.T) {
	secretKey := testKey(t)
	for _, tc := range []struct {
		name, value, want string
	}{
		{EnvDatabaseURL, "", EnvDatabaseURL},
		{EnvLogtoIssuer, "", EnvLogtoIssuer},
		{EnvLogtoIssuer, "ftp://x", EnvLogtoIssuer},
		{EnvLogtoJWKSURL, "nope", EnvLogtoJWKSURL},
		{EnvAddr, "nope", EnvAddr},
		{EnvMetricsAddr, "127.0.0.1:99999", EnvMetricsAddr},
		{EnvLogLevel, "İNFO", EnvLogLevel},
		{EnvRunMigrations, "maybe", EnvRunMigrations},
		{EnvTrustProxyClientAddress, "yes please", EnvTrustProxyClientAddress},
		{EnvAllowDirectClientAddress, "2", EnvAllowDirectClientAddress},
		{EnvCORSOrigins, "https://tide.delino.io/path", EnvCORSOrigins},
		{EnvCORSOrigins, "https://tide.delino.io#", EnvCORSOrigins},
		{EnvSecretKeys, "", EnvSecretKeys},
		{EnvOwnSandAPIOrigin, "https://ownsand.example/api", EnvOwnSandAPIOrigin},
		{EnvOwnSandAPIOrigin, "ftp://x", EnvOwnSandAPIOrigin},
		{EnvPublicOrigin, "https://tide.example?", EnvPublicOrigin},
		{EnvPublicOrigin, "https://u@tide.example", EnvPublicOrigin},
		{EnvSecretKeys, "k1", EnvSecretKeys},
		{EnvSecretKeys, "k1:" + secretKey[:20], EnvSecretKeys},
		{EnvSecretKeys, "k1:" + secretKey + ",k1:" + secretKey, EnvSecretKeys},
	} {
		env := baseEnv(t)
		env[tc.name] = tc.value
		_, err := Load(lookup(env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v, want an error that names %s", tc.name, tc.value, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), secretKey) || (len(tc.value) > 20 && strings.Contains(err.Error(), secretKey[:20])) {
			t.Errorf("%s=%q: error contains the key", tc.name, tc.value)
		}
	}
}
