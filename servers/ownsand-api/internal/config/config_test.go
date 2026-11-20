// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"strings"
	"testing"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

func minimal() map[string]string {
	return map[string]string{
		EnvDatabaseURL: "postgres://localhost/ownsand",
		EnvLogtoIssuer: "https://tenant.logto.app/oidc",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.LogtoAudience != DefaultAudience || cfg.LogtoJWKSURL != "https://tenant.logto.app/oidc/jwks" ||
		!cfg.RunMigrations || cfg.NonceKey != nil || cfg.TrustProxyClientAddress || len(cfg.CORSOrigins) != 1 {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadValues(t *testing.T) {
	v := minimal()
	v[EnvAddr] = "0.0.0.0:8080"
	v[EnvLogLevel] = "DEBUG"
	v[EnvLogtoJWKSFile] = "/etc/jwks.json"
	v[EnvNonceKey] = strings.Repeat("A", 43)
	v[EnvCORSOrigins] = "https://ownsand.delino.io, http://127.0.0.1:3300"
	v[EnvTrustProxyClientAddress] = "true"
	v[EnvRunMigrations] = "false"
	cfg, err := Load(env(v))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "0.0.0.0:8080" || cfg.LogLevel != LogLevelDebug || cfg.LogtoJWKSFile != "/etc/jwks.json" || cfg.LogtoJWKSURL != "" ||
		len(cfg.NonceKey) != 32 || len(cfg.CORSOrigins) != 2 || !cfg.TrustProxyClientAddress || cfg.RunMigrations {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"missing database":   func(v map[string]string) { delete(v, EnvDatabaseURL) },
		"missing issuer":     func(v map[string]string) { delete(v, EnvLogtoIssuer) },
		"bad issuer":         func(v map[string]string) { v[EnvLogtoIssuer] = "tenant" },
		"bad addr":           func(v map[string]string) { v[EnvAddr] = "8080" },
		"bad level":          func(v map[string]string) { v[EnvLogLevel] = "loud" },
		"short nonce key":    func(v map[string]string) { v[EnvNonceKey] = "c2hvcnQ" },
		"previous alone":     func(v map[string]string) { v[EnvNonceKeyPrevious] = strings.Repeat("A", 43) },
		"bad origin":         func(v map[string]string) { v[EnvCORSOrigins] = "https://x/path" },
		"bad trust boolean":  func(v map[string]string) { v[EnvTrustProxyClientAddress] = "maybe" },
		"bad migrate switch": func(v map[string]string) { v[EnvRunMigrations] = "sometimes" },
	} {
		v := minimal()
		mutate(v)
		if _, err := Load(env(v)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
