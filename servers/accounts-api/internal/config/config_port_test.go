// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsOutOfRangeLogtoPorts(t *testing.T) {
	for _, env := range []string{EnvLogtoIssuer, EnvLogtoManagementEndpoint} {
		for name, value := range map[string]string{
			"just above the range":    "https://127.0.0.1:65536/oidc",
			"far above the range":     "http://127.0.0.1:99999",
			"overflowing digit run":   "https://127.0.0.1:99999999999999999999/oidc",
			"overflowing with slash":  "https://tenant.logto.app:99999999999999999999/",
			"bracketed IPv6 overflow": "http://[::1]:65536",
		} {
			t.Run(env+" "+name, func(t *testing.T) {
				_, err := Load(lookupFrom(map[string]string{env: value}))
				if err == nil {
					t.Fatal("Load() error = nil, want an error")
				}
				message := err.Error()
				if !strings.Contains(message, env) {
					t.Errorf("error %q does not name %s", message, env)
				}
				if strings.Contains(message, "65536") || strings.Contains(message, "99999") ||
					strings.Contains(message, "127.0.0.1") || strings.Contains(message, "tenant.logto.app") {
					t.Errorf("error %q contains the value", message)
				}
			})
		}
	}
}

func TestLoadAcceptsLogtoPortsInRange(t *testing.T) {
	for _, env := range []string{EnvLogtoIssuer, EnvLogtoManagementEndpoint} {
		for name, tc := range map[string]struct{ value, want string }{
			"no port":                {"https://tenant.logto.app/oidc", "https://tenant.logto.app/oidc"},
			"highest port":           {"https://127.0.0.1:65535/oidc", "https://127.0.0.1:65535/oidc"},
			"highest port and slash": {"https://127.0.0.1:65535/oidc/", "https://127.0.0.1:65535/oidc"},
			"default https port":     {"https://tenant.logto.app:443", "https://tenant.logto.app:443"},
			"loopback http port":     {"http://127.0.0.1:3001/", "http://127.0.0.1:3001"},
			"bracketed IPv6 port":    {"http://[::1]:65535", "http://[::1]:65535"},
		} {
			t.Run(env+" "+name, func(t *testing.T) {
				cfg, err := Load(lookupFrom(map[string]string{env: tc.value}))
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				got := cfg.LogtoIssuer
				if env == EnvLogtoManagementEndpoint {
					got = cfg.LogtoManagementEndpoint
				}
				if got != tc.want {
					t.Errorf("%s = %q, want %q", env, got, tc.want)
				}
			})
		}
	}
}
