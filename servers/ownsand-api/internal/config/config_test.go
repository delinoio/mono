// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"os"
	"slices"
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
		"missing database":  func(v map[string]string) { delete(v, EnvDatabaseURL) },
		"missing issuer":    func(v map[string]string) { delete(v, EnvLogtoIssuer) },
		"bad issuer":        func(v map[string]string) { v[EnvLogtoIssuer] = "tenant" },
		"bad addr":          func(v map[string]string) { v[EnvAddr] = "8080" },
		"bad level":         func(v map[string]string) { v[EnvLogLevel] = "loud" },
		"short nonce key":   func(v map[string]string) { v[EnvNonceKey] = "c2hvcnQ" },
		"previous alone":    func(v map[string]string) { v[EnvNonceKeyPrevious] = strings.Repeat("A", 43) },
		"bad origin":        func(v map[string]string) { v[EnvCORSOrigins] = "https://x/path" },
		"origin fragment":   func(v map[string]string) { v[EnvCORSOrigins] = "https://x#frag" },
		"origin bare hash":  func(v map[string]string) { v[EnvCORSOrigins] = "https://ownsand.example#" },
		"origin bare query": func(v map[string]string) { v[EnvCORSOrigins] = "https://console.example.org?" },
		"origin user info":  func(v map[string]string) { v[EnvCORSOrigins] = "https://user@console.example.org" },
		"origin in list": func(v map[string]string) {
			v[EnvCORSOrigins] = "https://ownsand.example, https://user:pw@console.example.org"
		},
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

func TestLoadCORSOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, origins string
		ok            bool
	}{
		{"https origin", "https://console.example.org", true},
		{"http with port", "http://127.0.0.1:13300", true},
		{"list", "https://a.example, http://b.example:8080", true},
		{"IPv6 literal with port", "http://[::1]:80", true},
		{"loopback with port", "http://127.0.0.1:13100", true},
		{"hostless with port", "https://:443", false},
		{"hostless after valid", "https://a.example, https://:443", false},
		{"bare query", "https://console.example.org?", false},
		{"user info", "https://user@console.example.org", false},
		{"user and password", "https://user:pw@console.example.org", false},
		{"query", "https://console.example.org?a=b", false},
		{"fragment", "https://console.example.org#f", false},
		{"path", "https://console.example.org/p", false},
		{"port out of range", "https://console.example.org:99999", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := minimal()
			v[EnvCORSOrigins] = tc.origins
			_, err := Load(env(v))
			if tc.ok != (err == nil) {
				t.Fatalf("Load(%q) err = %v, want ok=%v", tc.origins, err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), EnvCORSOrigins) {
				t.Errorf("error %q does not name %s", err, EnvCORSOrigins)
			}
		})
	}
}

func TestLoadCanonicalizesCORSOrigins(t *testing.T) {
	v := minimal()
	v[EnvCORSOrigins] = "HTTPS://Console.Example.ORG:443, http://Console.Example.org:80, https://console.example.org:8443, http://localhost:443, http://[::1]:80, http://127.0.0.1:13100"
	cfg, err := Load(env(v))
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	want := []string{
		"https://console.example.org",
		"http://console.example.org",
		"https://console.example.org:8443",
		"http://localhost:443",
		"http://[::1]",
		"http://127.0.0.1:13100",
	}
	if !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.CORSOrigins, want)
	}
}

// TestLoadCanonicalizesIPv4CORSOrigins checks that every IPv4 spelling that a
// browser normalizes is stored as the dotted decimal origin that the browser
// sends in the Origin header, and that a host that browsers reject stops the
// server at startup.
func TestLoadCanonicalizesIPv4CORSOrigins(t *testing.T) {
	v := minimal()
	v[EnvCORSOrigins] = "http://127.1:3000, http://0x7f.1:3000, http://2130706433:3000, http://0177.0.0.1:3000, http://127.0.0.1.:3000, http://127.0.0.1:80, http://127.0.0.1:3001"
	cfg, err := Load(env(v))
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	want := []string{
		"http://127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://127.0.0.1:3000",
		"http://127.0.0.1",
		"http://127.0.0.1:3001",
	}
	if !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.CORSOrigins, want)
	}
	for _, value := range []string{"http://1.2.3.4.5", "http://256.1.1.1", "http://example.1", "http://4294967296"} {
		v := minimal()
		v[EnvCORSOrigins] = value
		_, err := Load(env(v))
		if err == nil || !strings.Contains(err.Error(), EnvCORSOrigins) {
			t.Errorf("CORS_ORIGINS=%q: err = %v, want an error that names %s", value, err, EnvCORSOrigins)
		}
	}
}

// TestLoadCanonicalizesIPv6CORSOrigins checks that every spelling of an IPv6
// host is stored in the compressed lower-case form that a browser sends in the
// Origin header, and that an address with a zone, which a browser rejects, stops
// the server at startup.
func TestLoadCanonicalizesIPv6CORSOrigins(t *testing.T) {
	v := minimal()
	v[EnvCORSOrigins] = "https://[0:0:0:0:0:0:0:1]:443, https://[::1]:8443, https://[::ffff:192.0.2.1]:8443, HTTP://[2001:DB8:0:0:1:0:0:1]:80"
	cfg, err := Load(env(v))
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	want := []string{
		"https://[::1]",
		"https://[::1]:8443",
		"https://[::ffff:c000:201]:8443",
		"http://[2001:db8::1:0:0:1]",
	}
	if !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.CORSOrigins, want)
	}
	for _, value := range []string{"https://[fe80::1%25eth0]", "https://[::1%25eth0]:443"} {
		v := minimal()
		v[EnvCORSOrigins] = value
		if _, err := Load(env(v)); err == nil || !strings.Contains(err.Error(), EnvCORSOrigins) {
			t.Errorf("CORS_ORIGINS=%q: err = %v, want an error that names %s", value, err, EnvCORSOrigins)
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

func TestLoadR2Endpoint(t *testing.T) {
	setR2 := func(v map[string]string, endpoint string) {
		v[EnvR2Endpoint], v[EnvR2Bucket], v[EnvR2AccessKeyID], v[EnvR2SecretAccessKey] = endpoint, "b", "k", "s"
	}
	for _, endpoint := range []string{
		"https://acct.example.test/storage",
		"https://acct.example.test/storage/",
		"https://acct.example.test//",
		"http://127.0.0.1:13100/s3",
		"https://acct.example.test?source=copy",
		"https://acct.example.test?",
		"https://acct.example.test#copy",
		"https://acct.example.test#",
		"https://acct.example.test/?source=copy",
		"https://acct.example.test/?",
		"https://acct.example.test/#copy",
		"https://acct.example.test/#",
	} {
		v := minimal()
		setR2(v, endpoint)
		_, err := Load(env(v))
		if err == nil || !strings.Contains(err.Error(), EnvR2Endpoint) {
			t.Errorf("%q: err = %v, want an error that names %s", endpoint, err, EnvR2Endpoint)
			continue
		}
		for _, value := range []string{"example.test", "127.0.0.1", "/s3", "source=", "copy", "?", "#"} {
			if strings.Contains(err.Error(), value) {
				t.Errorf("%q: error %q contains %q from the rejected value", endpoint, err, value)
			}
		}
	}
	for _, endpoint := range []string{
		"https://acct.example.test",
		"https://acct.example.test/",
		"http://127.0.0.1:13100",
		"http://127.0.0.1:13100/",
	} {
		v := minimal()
		setR2(v, endpoint)
		cfg, err := Load(env(v))
		if err != nil || !cfg.R2.Enabled {
			t.Errorf("%q: R2.Enabled = %v, err = %v", endpoint, cfg.R2.Enabled, err)
		}
	}
	if cfg, err := Load(env(minimal())); err != nil || cfg.R2.Enabled {
		t.Errorf("unset R2: R2.Enabled = %v, err = %v", cfg.R2.Enabled, err)
	}
	v := minimal()
	setR2(v, "https://acct.example.test")
	delete(v, EnvR2SecretAccessKey)
	if _, err := Load(env(v)); err == nil {
		t.Error("partial R2 configuration accepted")
	}
}

func TestLoadSTUNRateBurst(t *testing.T) {
	for _, value := range []string{"1", "799"} {
		v := minimal()
		v[EnvSTUNRateBurstBytes] = value
		_, err := Load(env(v))
		if err == nil || err.Error() != EnvSTUNRateBurstBytes+" must be at least 800 bytes" {
			t.Errorf("burst %s: error %v", value, err)
		}
	}
	v := minimal()
	cfg, err := Load(env(v))
	if err != nil || cfg.STUNRateBurstBytes != 4096 || cfg.STUNRateBytesPerSecond != 1024 {
		t.Fatalf("defaults: burst %d, rate %d, error %v", cfg.STUNRateBurstBytes, cfg.STUNRateBytesPerSecond, err)
	}
	v[EnvSTUNRateBurstBytes] = "800"
	cfg, err = Load(env(v))
	if err != nil || cfg.STUNRateBurstBytes != 800 {
		t.Fatalf("minimum burst: %d, error %v", cfg.STUNRateBurstBytes, err)
	}
	for _, value := range []string{"0", "-1", "abc"} {
		v[EnvSTUNRateBurstBytes] = value
		_, err := Load(env(v))
		if err == nil || err.Error() != EnvSTUNRateBurstBytes+" must be a positive integer" {
			t.Errorf("burst %s: error %v", value, err)
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
		{"empty", "", false},
		{"whitespace only", " \t\n\v\f\r", false},
		{"delimiter only", ",", false},
		{"blank items only", " , ", false},
		{"hostless", "stun:", false},
		{"hostless after valid", "stun:a:3478, stun:", false},
		{"hostless with port", "stun::3478", false},
		{"query", "stun:a:3478?transport=udp", false},
		{"turn", "turn:a:3478", false},
		{"stuns", "stuns:a:5349", false},
		{"highest port", "stun:a:65535", true},
		{"lowest port", "stun:a:1", true},
		{"port above range", "stun:a:65536", false},
		{"port above range after valid", "stun:a:3478, stun:a:65536", false},
		{"negative port", "stun:a:-1", false},
		{"negative port after valid", "stun:a:3478, stun:a:-1", false},
		{"port zero", "stun:a:0", false},
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

func TestLoadSTUNURLsOverride(t *testing.T) {
	v := minimal()
	v[EnvSTUNURLs] = " stun:127.0.0.1:13478, stun:[::1]:3478 "
	cfg, err := Load(env(v))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"stun:127.0.0.1:13478", "stun:[::1]:3478"}
	if !slices.Equal(cfg.STUNURLs, want) {
		t.Fatalf("STUNURLs = %v, want %v", cfg.STUNURLs, want)
	}
}

func TestLoadSTUNURLsDefault(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.STUNURLs) != 1 || cfg.STUNURLs[0] != DefaultSTUNURL {
		t.Fatalf("STUNURLs = %v, want [%s]", cfg.STUNURLs, DefaultSTUNURL)
	}
	if DefaultSTUNURL != "stun:ownsand.api.delino.io:3478" {
		t.Fatalf("DefaultSTUNURL = %q", DefaultSTUNURL)
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
	// A Windows checkout with core.autocrlf has CRLF line endings, which
	// Docker accepts; compare the lines, not the line endings.
	if !strings.Contains(strings.ReplaceAll(string(dockerfile), "\r\n", "\n"), "\nENV "+EnvSTUNAddr+"=0.0.0.0:3478\n") {
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
		"missing client id":      func(v map[string]string) { delete(v, EnvAccountsM2MClientID) },
		"missing client secret":  func(v map[string]string) { delete(v, EnvAccountsM2MClientSecret) },
		"missing endpoint":       func(v map[string]string) { delete(v, EnvLogtoTokenEndpoint) },
		"bad endpoint":           func(v map[string]string) { v[EnvLogtoTokenEndpoint] = "tenant/oidc/token" },
		"origin with path":       func(v map[string]string) { v[EnvAccountsAPIOrigin] = "https://accounts.api.delino.io/v1" },
		"origin without scheme":  func(v map[string]string) { v[EnvAccountsAPIOrigin] = "accounts.api.delino.io" },
		"origin with bare query": func(v map[string]string) { v[EnvAccountsAPIOrigin] = "http://127.0.0.1:18100?" },
		"origin with slash and bare query": func(v map[string]string) {
			v[EnvAccountsAPIOrigin] = "http://127.0.0.1:18100/?"
		},
		"origin with query":         func(v map[string]string) { v[EnvAccountsAPIOrigin] = "http://127.0.0.1:18100/?a=b" },
		"origin with bare fragment": func(v map[string]string) { v[EnvAccountsAPIOrigin] = "http://127.0.0.1:18100#" },
		"bad metrics addr":          func(v map[string]string) { v[EnvMetricsAddr] = "9090" },
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

func TestLoadAccountsAPIOriginQuery(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:18100?",
		"http://127.0.0.1:18100/?",
		"https://accounts.api.delino.io?",
		"https://accounts.api.delino.io/?",
		"https://accounts.api.delino.io/?a=b",
	} {
		v := minimal()
		v[EnvAccountsAPIOrigin] = origin
		_, err := Load(env(v))
		if err == nil || !strings.Contains(err.Error(), EnvAccountsAPIOrigin) {
			t.Errorf("%q: err = %v, want an error that names %s", origin, err, EnvAccountsAPIOrigin)
		}
	}
	for origin, want := range map[string]string{
		"https://accounts.api.delino.io":  "https://accounts.api.delino.io",
		"https://accounts.api.delino.io/": "https://accounts.api.delino.io",
		"http://127.0.0.1:18100":          "http://127.0.0.1:18100",
		"http://127.0.0.1:18100/":         "http://127.0.0.1:18100",
	} {
		v := minimal()
		v[EnvAccountsAPIOrigin] = origin
		cfg, err := Load(env(v))
		if err != nil || cfg.Accounts.APIOrigin != want {
			t.Errorf("%q: origin = %q, err = %v, want %q", origin, cfg.Accounts.APIOrigin, err, want)
		}
	}
}

// TestLoadAccountsAPIOriginUserInfo checks that user information in the origin
// stops the server at startup, and that the error names the variable without
// echoing the credentials.
func TestLoadAccountsAPIOriginUserInfo(t *testing.T) {
	for _, origin := range []string{
		"https://user@accounts.example.test",
		"https://user:secret@accounts.example.test",
		"https://user:secret@accounts.example.test/",
		"https://:secret@accounts.example.test",
		"https://@accounts.example.test",
		"http://user:secret@127.0.0.1:18100",
	} {
		v := minimal()
		v[EnvAccountsAPIOrigin] = origin
		_, err := Load(env(v))
		if err == nil || !strings.Contains(err.Error(), EnvAccountsAPIOrigin) {
			t.Errorf("%q: err = %v, want an error that names %s", origin, err, EnvAccountsAPIOrigin)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
			t.Errorf("%q: error %q contains the credentials", origin, err)
		}
	}
	for origin, want := range map[string]string{
		"https://accounts.example.test":       "https://accounts.example.test",
		"https://accounts.example.test/":      "https://accounts.example.test",
		"https://accounts.example.test:8443":  "https://accounts.example.test:8443",
		"https://accounts.example.test:8443/": "https://accounts.example.test:8443",
		"http://127.0.0.1:18100/":             "http://127.0.0.1:18100",
	} {
		v := minimal()
		v[EnvAccountsAPIOrigin] = origin
		cfg, err := Load(env(v))
		if err != nil || cfg.Accounts.APIOrigin != want {
			t.Errorf("%q: origin = %q, err = %v, want %q", origin, cfg.Accounts.APIOrigin, err, want)
		}
	}
}

func TestLoadRejectsHostlessURLs(t *testing.T) {
	const rejected = "https://:443"
	for _, tc := range []struct {
		env, value string
	}{
		{EnvAccountsAPIOrigin, rejected},
		{EnvLogtoIssuer, "https://:443/oidc"},
		{EnvLogtoJWKSURL, "https://:443/oidc/jwks"},
		{EnvR2Endpoint, rejected},
		{EnvLogtoTokenEndpoint, "https://:443/oidc/token"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			v := minimal()
			v[EnvR2Endpoint], v[EnvR2Bucket], v[EnvR2AccessKeyID], v[EnvR2SecretAccessKey] = "https://acct.r2.cloudflarestorage.com", "b", "k", "s"
			v[EnvLogtoTokenEndpoint] = "https://logto.example.org/oidc/token"
			v[EnvAccountsM2MClientID], v[EnvAccountsM2MClientSecret] = "id", "secret"
			v[tc.env] = tc.value
			_, err := Load(env(v))
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tc.env, tc.value)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
			if strings.Contains(err.Error(), ":443") {
				t.Errorf("error %q contains the rejected value", err)
			}
		})
	}
}

func TestLoadAcceptsLoopbackURLs(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:13100", "http://[::1]:80"} {
		t.Run(value, func(t *testing.T) {
			v := minimal()
			v[EnvAccountsAPIOrigin] = value
			v[EnvLogtoIssuer] = value + "/oidc"
			v[EnvR2Endpoint], v[EnvR2Bucket], v[EnvR2AccessKeyID], v[EnvR2SecretAccessKey] = value, "b", "k", "s"
			if _, err := Load(env(v)); err != nil {
				t.Fatalf("Load(%q) = %v", value, err)
			}
		})
	}
}
