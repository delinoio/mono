// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"os"
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
		!cfg.RunMigrations || cfg.NonceKey != nil || cfg.TrustProxyClientAddress || cfg.AllowDirectClientAddress || cfg.ClientAddressMode != ClientAddressNone || len(cfg.CORSOrigins) != 1 {
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
		"origin fragment":    func(v map[string]string) { v[EnvCORSOrigins] = "https://x#frag" },
		"origin bare hash":   func(v map[string]string) { v[EnvCORSOrigins] = "https://ownsand.example#" },
		"bad trust boolean":  func(v map[string]string) { v[EnvTrustProxyClientAddress] = "maybe" },
		"bad direct boolean": func(v map[string]string) { v[EnvAllowDirectClientAddress] = "maybe" },
		"bad migrate switch": func(v map[string]string) { v[EnvRunMigrations] = "sometimes" },
	} {
		v := minimal()
		mutate(v)
		if _, err := Load(env(v)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadR2AndSTUN(t *testing.T) {
	v := minimal()
	v[EnvSTUNURLs] = "stun:a:3478, stun:b:3478"
	v[EnvR2Endpoint] = "https://acct.r2.cloudflarestorage.com"
	v[EnvR2Bucket] = "b"
	v[EnvR2AccessKeyID] = "k"
	v[EnvR2SecretAccessKey] = "s"
	v[EnvSnapshotPartSizeBytes] = "10485760"
	cfg, err := Load(env(v))
	if err != nil || len(cfg.STUNURLs) != 2 || !cfg.R2.Enabled || cfg.SnapshotPartSizeBytes != 10<<20 {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"partial r2":     func(v map[string]string) { delete(v, EnvR2Bucket) },
		"bad stun":       func(v map[string]string) { v[EnvSTUNURLs] = "turn:x" },
		"tiny part size": func(v map[string]string) { v[EnvSnapshotPartSizeBytes] = "1024" },
	} {
		w := map[string]string{}
		for k, val := range v {
			w[k] = val
		}
		mutate(w)
		if _, err := Load(env(w)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadSTUNURLs(t *testing.T) {
	for _, tc := range []struct {
		name, urls string
		ok         bool
	}{
		{"hostname and port", "stun:localhost:13478", true},
		{"hostname default port", "stun:example.com", true},
		{"bracketed IPv6", "stun:[::1]:3478", true},
		{"list", "stun:a:3478, stun:[::1]:3478", true},
		{"hostless", "stun:", false},
		{"hostless after valid", "stun:a:3478, stun:", false},
		{"hostless with port", "stun::3478", false},
		{"query", "stun:a:3478?transport=udp", false},
		{"turn", "turn:a:3478", false},
		{"stuns", "stuns:a:5349", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := minimal()
			v[EnvSTUNURLs] = tc.urls
			_, err := Load(env(v))
			if tc.ok != (err == nil) {
				t.Fatalf("Load(%q) err = %v, want ok=%v", tc.urls, err, tc.ok)
			}
			if want := EnvSTUNURLs + " must list stun: URLs with a host"; err != nil && err.Error() != want {
				t.Fatalf("error = %q, want %q (no URL echo)", err, want)
			}
		})
	}
}

func TestLoadClientAddressMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trust, allow string
		want         ClientAddressMode
	}{
		{"neither", "", "", ClientAddressNone},
		{"both false", "false", "false", ClientAddressNone},
		{"direct", "", "true", ClientAddressDirect},
		{"verified proxy", "true", "", ClientAddressVerifiedProxy},
		{"verified proxy takes precedence", "true", "true", ClientAddressVerifiedProxy},
		{"trust false allows direct", "false", "true", ClientAddressDirect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := minimal()
			if tc.trust != "" {
				v[EnvTrustProxyClientAddress] = tc.trust
			}
			if tc.allow != "" {
				v[EnvAllowDirectClientAddress] = tc.allow
			}
			cfg, err := Load(env(v))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ClientAddressMode != tc.want {
				t.Fatalf("mode = %q, want %q", cfg.ClientAddressMode, tc.want)
			}
		})
	}
}

func TestLoadRejectsNonASCIILogLevelThatFoldsToLevelName(t *testing.T) {
	for name, value := range map[string]string{
		"U+0130 NFO":            "\u0130NFO",
		"U+0130 nfo":            "\u0130nfo",
		"U+0130 NFO with space": " \u0130NFO ",
		"loud":                  "loud",
		"KNFO":                  "KNFO",
		"dotless i nfo":         "\u0131nfo",
	} {
		v := minimal()
		v[EnvLogLevel] = value
		_, err := Load(env(v))
		if err == nil || err.Error() != "LOG_LEVEL must be one of debug, info, warn, error" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestLoadAcceptsASCIILogLevelInAnyCase(t *testing.T) {
	for value, want := range map[string]LogLevel{
		"DEBUG": LogLevelDebug,
		"WARN":  LogLevelWarn,
		"Error": LogLevelError,
		"INFO":  LogLevelInfo,
		"iNfO":  LogLevelInfo,
		" Warn": LogLevelWarn,
		"":      LogLevelInfo,
		" \t ":  LogLevelInfo,
	} {
		v := minimal()
		v[EnvLogLevel] = value
		cfg, err := Load(env(v))
		if err != nil {
			t.Errorf("%q: %v", value, err)
			continue
		}
		if cfg.LogLevel != want {
			t.Errorf("%q: level = %q, want %q", value, cfg.LogLevel, want)
		}
	}
}

// TestSTUNAddrDefaults pins the standalone binary default and requires the
// runtime image to override it with the wildcard address, because a
// loopback-bound listener cannot receive datagrams from a published UDP port.
func TestSTUNAddrDefaults(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil || cfg.STUNAddr != "127.0.0.1:13478" {
		t.Fatalf("default STUNAddr = %q, %v", cfg.STUNAddr, err)
	}
	v := minimal()
	v[EnvSTUNAddr] = "0.0.0.0:3478"
	cfg, err = Load(env(v))
	if err != nil || cfg.STUNAddr != "0.0.0.0:3478" {
		t.Fatalf("explicit STUNAddr = %q, %v", cfg.STUNAddr, err)
	}
	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "\nENV "+EnvSTUNAddr+"=0.0.0.0:3478\n") {
		t.Errorf("Dockerfile does not set %s=0.0.0.0:3478", EnvSTUNAddr)
	}
}

func TestLoadAccountsAndMetrics(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	// Without the machine-to-machine credential, usage reporting is off and
	// the metrics listener is off.
	if cfg.Accounts.Enabled || cfg.Accounts.APIOrigin != DefaultAccountsAPIOrigin || cfg.MetricsAddr != "" {
		t.Fatalf("defaults = %+v %q", cfg.Accounts, cfg.MetricsAddr)
	}
	v := minimal()
	v[EnvAccountsAPIOrigin] = "http://127.0.0.1:18100/"
	v[EnvLogtoTokenEndpoint] = "https://tenant.logto.app/oidc/token"
	v[EnvAccountsM2MClientID] = "m2m-id"
	v[EnvAccountsM2MClientSecret] = "m2m-secret"
	v[EnvMetricsAddr] = "0.0.0.0:9090"
	cfg, err = Load(env(v))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Accounts.Enabled || cfg.Accounts.APIOrigin != "http://127.0.0.1:18100" || cfg.Accounts.TokenEndpoint != "https://tenant.logto.app/oidc/token" ||
		cfg.Accounts.ClientID != "m2m-id" || cfg.Accounts.ClientSecret != "m2m-secret" || cfg.MetricsAddr != "0.0.0.0:9090" {
		t.Fatalf("cfg = %+v %q", cfg.Accounts, cfg.MetricsAddr)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing client id":     func(v map[string]string) { delete(v, EnvAccountsM2MClientID) },
		"missing client secret": func(v map[string]string) { delete(v, EnvAccountsM2MClientSecret) },
		"missing endpoint":      func(v map[string]string) { delete(v, EnvLogtoTokenEndpoint) },
		"bad endpoint":          func(v map[string]string) { v[EnvLogtoTokenEndpoint] = "tenant/oidc/token" },
		"origin with path":      func(v map[string]string) { v[EnvAccountsAPIOrigin] = "https://accounts.api.delino.io/v1" },
		"origin without scheme": func(v map[string]string) { v[EnvAccountsAPIOrigin] = "accounts.api.delino.io" },
		"bad metrics addr":      func(v map[string]string) { v[EnvMetricsAddr] = "9090" },
	} {
		w := map[string]string{}
		for k, val := range v {
			w[k] = val
		}
		mutate(w)
		if _, err := Load(env(w)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
