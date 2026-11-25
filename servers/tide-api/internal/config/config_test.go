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
	if cfg.Addr != "127.0.0.1:18400" || cfg.LogLevel != LogLevelInfo || !cfg.RunMigrations || cfg.ReclaimLegacyOwnSandRemovals {
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
	env[EnvReclaimLegacyOwnSandRemovals] = "true"
	env[EnvLogtoJWKSFile] = "/tmp/jwks.json"
	env[EnvLogtoAudience] = "https://tide.example"
	env[EnvCORSOrigins] = "https://tide.delino.io, http://127.0.0.1:13400"
	env[EnvTrustProxyClientAddress] = "true"
	env[EnvAllowDirectClientAddress] = "true"
	env[EnvMetricsAddr] = "0.0.0.0:9400"
	env[EnvSecretKeys] = "new:" + testKey(t) + ",old:" + testKey(t)
	env[EnvOwnSandAPIOrigin] = "http://127.0.0.1:18300/"
	env[EnvPublicOrigin] = "http://[::1]:18400/"
	cfg, err := Load(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.LogLevel != LogLevelDebug || cfg.RunMigrations || !cfg.ReclaimLegacyOwnSandRemovals || cfg.LogtoJWKSURL != "" || cfg.LogtoJWKSFile != "/tmp/jwks.json" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.LogtoAudience != "https://tide.example" || len(cfg.CORSOrigins) != 2 || cfg.MetricsAddr != "0.0.0.0:9400" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.OwnSandAPIOrigin != "http://127.0.0.1:18300" || cfg.PublicOrigin != "http://[::1]:18400" {
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

func TestLoadCanonicalizesCORSOrigins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins string
		want    []string
	}{
		{
			name:    "mixed origins",
			origins: "HTTPS://Tide.Delino.io:443, http://Tide.Delino.io:80, https://tide.delino.io:8443, http://localhost:443, http://[0:0:0:0:0:0:0:1]:80, http://127.1:3000",
			want: []string{
				"https://tide.delino.io",
				"http://tide.delino.io",
				"https://tide.delino.io:8443",
				"http://localhost:443",
				"http://[::1]",
				"http://127.0.0.1:3000",
			},
		},
		{
			name:    "alternate IP forms",
			origins: "http://0x7f.1:3000, http://2130706433:3000, https://[0:0:0:0:0:0:0:1]:443",
			want:    []string{"http://127.0.0.1:3000", "http://127.0.0.1:3000", "https://[::1]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(t)
			env[EnvCORSOrigins] = tc.origins
			cfg, err := Load(lookup(env))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.CORSOrigins, tc.want) {
				t.Errorf("CORSOrigins = %q, want %q", cfg.CORSOrigins, tc.want)
			}
		})
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
		{EnvLogtoIssuer, "https://:443/oidc", EnvLogtoIssuer},
		{EnvLogtoJWKSURL, "nope", EnvLogtoJWKSURL},
		{EnvLogtoJWKSURL, "https://:443/oidc/jwks", EnvLogtoJWKSURL},
		{EnvAddr, "nope", EnvAddr},
		{EnvMetricsAddr, "127.0.0.1:99999", EnvMetricsAddr},
		{EnvLogLevel, "İNFO", EnvLogLevel},
		{EnvRunMigrations, "maybe", EnvRunMigrations},
		{EnvTrustProxyClientAddress, "yes please", EnvTrustProxyClientAddress},
		{EnvAllowDirectClientAddress, "2", EnvAllowDirectClientAddress},
		{EnvCORSOrigins, "https://u@tide.delino.io", EnvCORSOrigins},
		{EnvCORSOrigins, "https://:443", EnvCORSOrigins},
		{EnvCORSOrigins, "https://exa%25mple.test", EnvCORSOrigins},
		{EnvCORSOrigins, "https://1.2.3.4.5", EnvCORSOrigins},
		{EnvCORSOrigins, "https://tide.delino.io?", EnvCORSOrigins},
		{EnvCORSOrigins, "https://tide.delino.io/path", EnvCORSOrigins},
		{EnvCORSOrigins, "https://tide.delino.io#", EnvCORSOrigins},
		{EnvCORSOrigins, "ftp://tide.delino.io", EnvCORSOrigins},
		{EnvCORSOrigins, "http://[fe80::1%25eth0]", EnvCORSOrigins},
		{EnvSecretKeys, "", EnvSecretKeys},
		{EnvOwnSandAPIOrigin, "https://ownsand.example/api", EnvOwnSandAPIOrigin},
		{EnvOwnSandAPIOrigin, "ftp://x", EnvOwnSandAPIOrigin},
		{EnvOwnSandAPIOrigin, "http://:18300", EnvOwnSandAPIOrigin},
		{EnvPublicOrigin, "https://tide.example?", EnvPublicOrigin},
		{EnvPublicOrigin, "https://u@tide.example", EnvPublicOrigin},
		{EnvPublicOrigin, "http://:18400/", EnvPublicOrigin},
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
		if strings.Contains(tc.value, "://") && strings.Contains(err.Error(), tc.value) {
			t.Errorf("%s=%q: error contains the configured URL", tc.name, tc.value)
		}
	}
}
