// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import "testing"

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoad(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvDatabaseURL: "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.APIResource != DefaultAPIResource || len(cfg.CORSAllowedOrigins) != 1 || cfg.CORSAllowedOrigins[0] != DefaultCORSAllowedOrigins || cfg.LogtoEndpoint != "" {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, err = Load(env(map[string]string{
		EnvDatabaseURL: "postgres://x", EnvLogtoEndpoint: "https://tenant.example/", EnvLogLevel: "DEBUG",
		EnvCORSAllowedOrigins: "https://ownsand.delino.io, http://127.0.0.1:3300",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogtoIssuer() != "https://tenant.example/oidc" || cfg.LogtoJWKSURI() != "https://tenant.example/oidc/jwks" || cfg.LogLevel != LogLevelDebug || len(cfg.CORSAllowedOrigins) != 2 {
		t.Fatalf("parsed: %+v", cfg)
	}
	for name, m := range map[string]map[string]string{
		"missing database": {},
		"bad addr":         {EnvDatabaseURL: "x", EnvAddr: "nope"},
		"http logto":       {EnvDatabaseURL: "x", EnvLogtoEndpoint: "http://tenant.example"},
		"http origin":      {EnvDatabaseURL: "x", EnvCORSAllowedOrigins: "http://evil.example"},
		"origin with path": {EnvDatabaseURL: "x", EnvCORSAllowedOrigins: "https://a.example/x"},
		"bad level":        {EnvDatabaseURL: "x", EnvLogLevel: "loud"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
