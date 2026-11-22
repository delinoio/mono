// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// requiredValues are valid values of every required variable. lookupFrom
// adds them so that a test only names the variables it checks.
var requiredValues = map[string]string{
	EnvDatabaseURL:             "postgres://accounts@127.0.0.1:5432/accounts",
	EnvLogtoIssuer:             "https://tenant.logto.app/oidc",
	EnvLogtoManagementEndpoint: "https://tenant.logto.app",
	EnvLogtoM2MClientID:        "m2m-client",
	EnvLogtoM2MClientSecret:    "m2m-secret",
	EnvResendAPIKey:            "re_test",
	EnvEmailFrom:               "Delino <billing@delino.io>",
	EnvPaddleEnvironment:       "sandbox",
	EnvPaddleAPIKey:            "pdl_sdbx_apikey_test",
	EnvPaddleWebhookSecret:     "pdl_ntfset_test",
	EnvPaddlePriceID:           "pri_01aaaaaaaaaaaaaaaaaaaaaaaa",
	EnvPaddleTopUpProductID:    "pro_01bbbbbbbbbbbbbbbbbbbbbbbb",
}

// lookupFrom returns a lookup over values on top of requiredValues. A value
// in values replaces the required value; use rawLookup to leave one unset.
func lookupFrom(values map[string]string) func(string) (string, bool) {
	merged := map[string]string{}
	for name, value := range requiredValues {
		merged[name] = value
	}
	for name, value := range values {
		merged[name] = value
	}
	return rawLookup(merged)
}

func rawLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{EnvAddr: "  ", EnvLogLevel: ""}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.LogLevel != LogLevelInfo {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelInfo)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{EnvAddr: "0.0.0.0:9000", EnvLogLevel: "DEBUG"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "0.0.0.0:9000")
	}
	if cfg.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelDebug)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"missing port":   {EnvAddr: "127.0.0.1"},
		"port too large": {EnvAddr: "127.0.0.1:70000"},
		"named port":     {EnvAddr: "127.0.0.1:http"},
		"unknown level":  {EnvLogLevel: "verbose"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(lookupFrom(values)); err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
		})
	}
}

func TestLoadRejectsInvalidAddrHost(t *testing.T) {
	for name, tc := range map[string]struct{ addr, host string }{
		"space in host":        {"not a host:8100", "not a host"},
		"userinfo":             {"user@127.0.0.1:8100", "user@127.0.0.1"},
		"path":                 {"127.0.0.1/path:8100", "127.0.0.1/path"},
		"trailing space":       {"127.0.0.1 :8100", "127.0.0.1 "},
		"empty label":          {"127..0.1:8100", "127..0.1"},
		"leading hyphen":       {"-bad.example.com:8100", "-bad.example.com"},
		"trailing hyphen":      {"bad-.example.com:8100", "bad-.example.com"},
		"underscore":           {"bad_host:8100", "bad_host"},
		"non-ASCII":            {"höst:8100", "höst"},
		"only a dot":           {".:8100", "."},
		"label too long":       {strings.Repeat("a", 64) + ":8100", strings.Repeat("a", 64)},
		"hostname too long":    {strings.Repeat("a.", 127) + "a:8100", strings.Repeat("a.", 127) + "a"},
		"bad IPv6 in brackets": {"[::1%]:8100", "::1%"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: tc.addr}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.HasPrefix(err.Error(), EnvAddr+" host ") {
				t.Errorf("error = %q, want prefix %q", err, EnvAddr+" host ")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.host)) {
				t.Errorf("error = %q, want it to name host %q", err, tc.host)
			}
		})
	}
}

func TestLoadAcceptsValidAddrHost(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:8100",
		"0.0.0.0:9000",
		":8100",
		"[::1]:8100",
		"[::]:8100",
		"[fe80::1%lo0]:8100",
		"localhost:8100",
		"example.com:8100",
		"example.com.:8100",
		"my-host.internal:0",
		"A1.b2.C3:65535",
		strings.Repeat("a", 63) + ":8100",
	} {
		t.Run(addr, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Addr != addr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, addr)
			}
		})
	}
}

func TestLoadKeepsExistingAddrMessages(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":       "ADDR must be a host:port address",
		":70000":          "ADDR port must be a number from 0 to 65535",
		":http":           "ADDR port must be a number from 0 to 65535",
		"not a host:http": "ADDR port must be a number from 0 to 65535",
	} {
		t.Run(addr, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Load() error = %v, want prefix %q", err, want)
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	for level, want := range map[LogLevel]string{
		LogLevelDebug: "DEBUG",
		LogLevelInfo:  "INFO",
		LogLevelWarn:  "WARN",
		LogLevelError: "ERROR",
	} {
		if got := level.SlogLevel().String(); got != want {
			t.Errorf("%q.SlogLevel() = %s, want %s", level, got, want)
		}
	}
}

// unicodeWhiteSpace lists Unicode white space characters that
// strings.TrimSpace removes but that are not ASCII white space.
var unicodeWhiteSpace = map[string]string{
	"U+00A0": " ",
	"U+0085": "\u0085",
	"U+1680": " ",
	"U+2000": " ",
	"U+200A": " ",
	"U+2028": " ",
	"U+2029": " ",
	"U+202F": " ",
	"U+205F": " ",
	"U+3000": "　",
}

func TestLoadRejectsUnicodeWhiteSpaceAddr(t *testing.T) {
	values := map[string]string{
		"U+3000 before address":    "　127.0.0.1:9000",
		"U+00A0 after address":     "127.0.0.1:9000 ",
		"ASCII space then U+00A0":  "  ",
		"U+00A0 before port":       " :8100",
		"U+00A0 around ASCII pads": "  127.0.0.1:9000  ",
	}
	for name, value := range unicodeWhiteSpace {
		values[name+" only"] = value
	}
	for name, addr := range values {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), EnvAddr) {
				t.Errorf("error = %q, want it to name %s", err, EnvAddr)
			}
		})
	}
}

func TestLoadRejectsUnicodeWhiteSpaceLogLevel(t *testing.T) {
	values := map[string]string{
		"U+00A0 only":            " ",
		"U+3000 only":            "　",
		"U+00A0 before level":    " debug",
		"U+00A0 after level":     "debug ",
		"U+2028 before level":    " info",
		"ASCII and U+00A0 mixed": "   ",
	}
	for name, level := range values {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvLogLevel: level}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), EnvLogLevel) {
				t.Errorf("error = %q, want it to name %s", err, EnvLogLevel)
			}
		})
	}
}

func TestLoadDefaultsForEveryASCIIWhiteSpace(t *testing.T) {
	for name, value := range map[string]string{
		"space":           " ",
		"tab":             "\t",
		"carriage return": "\r",
		"line feed":       "\n",
		"vertical tab":    "\v",
		"form feed":       "\f",
		"all six":         " \t\r\n\v\f",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvAddr: value, EnvLogLevel: value}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Addr != DefaultAddr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
			}
			if cfg.LogLevel != LogLevelInfo {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelInfo)
			}
		})
	}
}

func TestLoadTrimsASCIIPadding(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvAddr:     " \t127.0.0.1:9000\r\n\v\f ",
		EnvLogLevel: " debug ",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "127.0.0.1:9000")
	}
	if cfg.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelDebug)
	}
}

func TestLoadRejectsNonASCIILogLevelThatFoldsToLevelName(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  string
	}{
		"U+0130 then NFO":             {value: "İNFO", want: "İNFO"},
		"U+0130 then nfo":             {value: "İnfo", want: "İnfo"},
		"U+0130 with ASCII padding":   {value: " İNFO ", want: "İNFO"},
		"U+212A (Kelvin sign) in nfo": {value: "KNFO", want: "KNFO"},
		"U+0131 (dotless i) nfo":      {value: "ınfo", want: "ınfo"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvLogLevel: tc.value}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			want := fmt.Sprintf("LOG_LEVEL must be one of debug, info, warn, error; got %q", tc.want)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}
}

func TestLoadAcceptsASCIILogLevelInAnyCase(t *testing.T) {
	for value, want := range map[string]LogLevel{
		"DEBUG": LogLevelDebug,
		"WARN":  LogLevelWarn,
		"Error": LogLevelError,
		"INFO":  LogLevelInfo,
		"iNfO":  LogLevelInfo,
	} {
		t.Run(value, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvLogLevel: value}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.LogLevel != want {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, want)
			}
		})
	}
}

func TestLoadBillingDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.RunMigrations {
		t.Error("RunMigrations = false, want true")
	}
	if cfg.LogtoAudience != DefaultAudience {
		t.Errorf("LogtoAudience = %q, want %q", cfg.LogtoAudience, DefaultAudience)
	}
	if len(cfg.CORSOrigins) != 1 || cfg.CORSOrigins[0] != DefaultWebOrigin {
		t.Errorf("CORSOrigins = %q, want [%q]", cfg.CORSOrigins, DefaultWebOrigin)
	}
	if cfg.WebOrigin != DefaultWebOrigin {
		t.Errorf("WebOrigin = %q, want %q", cfg.WebOrigin, DefaultWebOrigin)
	}
	if cfg.DatabaseURL != requiredValues[EnvDatabaseURL] || cfg.LogtoM2MClientSecret != "m2m-secret" || cfg.ResendAPIKey != "re_test" {
		t.Errorf("required values were not loaded: %+v", cfg)
	}
}

func TestLoadBillingOverrides(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvRunMigrations:           "false",
		EnvLogtoIssuer:             "https://tenant.logto.app/oidc/",
		EnvLogtoAudience:           "https://accounts.api.example",
		EnvLogtoManagementEndpoint: "https://tenant.logto.app/",
		EnvCORSOrigins:             " https://accounts.delino.io , http://127.0.0.1:13100,",
		EnvWebOrigin:               "http://127.0.0.1:13100",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RunMigrations {
		t.Error("RunMigrations = true, want false")
	}
	if cfg.LogtoIssuer != "https://tenant.logto.app/oidc" {
		t.Errorf("LogtoIssuer = %q", cfg.LogtoIssuer)
	}
	if cfg.LogtoManagementEndpoint != "https://tenant.logto.app" {
		t.Errorf("LogtoManagementEndpoint = %q", cfg.LogtoManagementEndpoint)
	}
	if cfg.LogtoAudience != "https://accounts.api.example" {
		t.Errorf("LogtoAudience = %q", cfg.LogtoAudience)
	}
	if strings.Join(cfg.CORSOrigins, " ") != "https://accounts.delino.io http://127.0.0.1:13100" {
		t.Errorf("CORSOrigins = %q", cfg.CORSOrigins)
	}
	if cfg.WebOrigin != "http://127.0.0.1:13100" {
		t.Errorf("WebOrigin = %q", cfg.WebOrigin)
	}
}

func TestLoadRequiresBillingValues(t *testing.T) {
	for name := range requiredValues {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for n, v := range requiredValues {
				if n != name {
					values[n] = v
				}
			}
			_, err := Load(rawLookup(values))
			if err == nil || err.Error() != name+" is required" {
				t.Fatalf("Load() error = %v, want %q", err, name+" is required")
			}
			values[name] = " \t "
			if _, err := Load(rawLookup(values)); err == nil {
				t.Fatal("Load() accepted a value of only white space")
			}
		})
	}
}

func TestLoadRejectsInvalidBillingValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"run migrations":          {EnvRunMigrations: "yes please"},
		"issuer scheme":           {EnvLogtoIssuer: "ftp://tenant.logto.app/oidc"},
		"issuer query":            {EnvLogtoIssuer: "https://tenant.logto.app/oidc?x=1"},
		"management relative":     {EnvLogtoManagementEndpoint: "tenant.logto.app"},
		"cors path":               {EnvCORSOrigins: "https://accounts.delino.io/app"},
		"cors only commas":        {EnvCORSOrigins: ",,"},
		"cors fragment":           {EnvCORSOrigins: "https://accounts.delino.io#"},
		"web origin path":         {EnvWebOrigin: "https://accounts.delino.io/billing"},
		"email from not an email": {EnvEmailFrom: "billing"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(lookupFrom(values)); err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
		})
	}
}

func TestLoadRejectsHostlessURLsAndOrigins(t *testing.T) {
	for name, tc := range map[string]struct{ env, value string }{
		"issuer https port only":       {EnvLogtoIssuer, "https://:443/oidc"},
		"issuer http port only":        {EnvLogtoIssuer, "http://:80"},
		"management port only":         {EnvLogtoManagementEndpoint, "https://:443"},
		"cors port only":               {EnvCORSOrigins, "https://:443"},
		"cors port only in a list":     {EnvCORSOrigins, "https://accounts.delino.io, https://:443"},
		"web origin port only":         {EnvWebOrigin, "https://:443"},
		"web origin http port only":    {EnvWebOrigin, "http://:80"},
		"cors empty bracketed address": {EnvCORSOrigins, "http://[]:80"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{tc.env: tc.value}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
			if strings.Contains(err.Error(), ":443") || strings.Contains(err.Error(), ":80") {
				t.Errorf("error %q contains the value", err)
			}
		})
	}
}

func TestLoadKeepsLoopbackAndIPv6Hosts(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvCORSOrigins: "http://127.0.0.1:13100,http://[::1]:80",
		EnvWebOrigin:   "http://[::1]:13100",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := []string{"http://127.0.0.1:13100", "http://[::1]:80"}; !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %q, want %q", cfg.CORSOrigins, want)
	}
	if cfg.WebOrigin != "http://[::1]:13100" {
		t.Errorf("WebOrigin = %q", cfg.WebOrigin)
	}
}

func TestLoadErrorsDoNotContainSecrets(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{EnvLogtoIssuer: "https://user:hunter2@tenant.logto.app/oidc"}))
	if err == nil {
		t.Fatal("Load() accepted an issuer with user info")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %q contains the secret", err)
	}
}

func TestMetricsAddr(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil || cfg.MetricsAddr != "" {
		t.Fatalf("default METRICS_ADDR = %q, %v; want empty", cfg.MetricsAddr, err)
	}
	cfg, err = Load(lookupFrom(map[string]string{EnvMetricsAddr: " :9100 "}))
	if err != nil || cfg.MetricsAddr != ":9100" {
		t.Fatalf("METRICS_ADDR = %q, %v", cfg.MetricsAddr, err)
	}
	for _, value := range []string{"9100", "host:port", "http://127.0.0.1:9100"} {
		if _, err := Load(lookupFrom(map[string]string{EnvMetricsAddr: value})); err == nil || !strings.Contains(err.Error(), EnvMetricsAddr) {
			t.Errorf("METRICS_ADDR=%q: err = %v, want an error that names %s", value, err, EnvMetricsAddr)
		}
	}
}

func TestPaddleConfig(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PaddleEnvironment != "sandbox" || cfg.PaddleAPIKey != "pdl_sdbx_apikey_test" || cfg.PaddleWebhookSecret != "pdl_ntfset_test" ||
		cfg.PaddleSubscriptionPriceID != "pri_01aaaaaaaaaaaaaaaaaaaaaaaa" || cfg.PaddleTopUpProductID != "pro_01bbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("Paddle configuration = %+v", cfg)
	}
	if cfg, err := Load(lookupFrom(map[string]string{EnvPaddleEnvironment: "production"})); err != nil || cfg.PaddleEnvironment != "production" {
		t.Fatalf("production: %v", err)
	}
	for name, value := range map[string]string{
		EnvPaddleEnvironment:    "live",
		EnvPaddlePriceID:        "pro_01aaaaaaaaaaaaaaaaaaaaaaaa",
		EnvPaddleTopUpProductID: "pri_01bbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		_, err := Load(lookupFrom(map[string]string{name: value}))
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), value) {
			t.Errorf("%s=%q: err = %v, want an error that names the variable and not the value", name, value, err)
		}
	}
	// The secrets never appear in an error.
	_, err = Load(lookupFrom(map[string]string{EnvPaddleAPIKey: " "}))
	if err == nil || err.Error() != EnvPaddleAPIKey+" is required" {
		t.Fatalf("missing key: %v", err)
	}
}

func TestUsageServiceClients(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.UsageServiceClients) != 0 {
		t.Errorf("UsageServiceClients = %v, want none by default", cfg.UsageServiceClients)
	}
	cfg, err = Load(lookupFrom(map[string]string{
		EnvUsageServiceClients: " app1=ownsand , app2 = ownsand,,app1=ownsand",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := fmt.Sprint(cfg.UsageServiceClients); got != "map[app1:[SERVICE_ID_OWNSAND] app2:[SERVICE_ID_OWNSAND]]" {
		t.Errorf("UsageServiceClients = %s", got)
	}
	for _, value := range []string{"app1", "=ownsand", "app1=", "app1=OWNSAND", "app1=unspecified", "app1=tide", "app 1=ownsand", "app1=ownsand=x"} {
		if _, err := Load(lookupFrom(map[string]string{EnvUsageServiceClients: value})); err == nil ||
			!strings.Contains(err.Error(), EnvUsageServiceClients) {
			t.Errorf("Load(%q) error = %v, want an error that names %s", value, err, EnvUsageServiceClients)
		}
	}
}
