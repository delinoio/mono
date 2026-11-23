// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

func testPEM(t *testing.T, pkcs8 bool) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), key
}

func gitHubEnv(t *testing.T, keyPEM string) map[string]string {
	env := baseEnv(t)
	env[EnvGitHubAppID] = "12345"
	env[EnvGitHubAppClientID] = "Iv23liTestClient"
	env[EnvGitHubAppClientSecret] = "client-secret-value"
	env[EnvGitHubAppPrivateKey] = keyPEM
	env[EnvGitHubAppWebhookSecret] = "webhook-secret-value"
	env[EnvGitHubAppSlug] = "tide-test"
	return env
}

func TestGitHubAppOffByDefault(t *testing.T) {
	cfg, err := Load(lookup(baseEnv(t)))
	if err != nil || cfg.GitHub != nil {
		t.Fatalf("github = %+v, %v", cfg.GitHub, err)
	}
}

func TestGitHubApp(t *testing.T) {
	keyPEM, key := testPEM(t, false)
	cfg, err := Load(lookup(gitHubEnv(t, keyPEM)))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.GitHub
	if g == nil || g.AppID != 12345 || g.ClientID != "Iv23liTestClient" || g.Slug != "tide-test" || !g.PrivateKey.Equal(key) ||
		g.APIOrigin != "https://api.github.com" || g.WebOrigin != "https://github.com" {
		t.Fatalf("github = %+v", g)
	}
	// PKCS #8 and a one-line value with literal \n sequences.
	keyPEM, key = testPEM(t, true)
	env := gitHubEnv(t, strings.ReplaceAll(keyPEM, "\n", `\n`))
	env[EnvGitHubAPIOrigin] = "http://127.0.0.1:9999/"
	env[EnvGitHubWebOrigin] = "http://127.0.0.1:9998"
	if cfg, err = Load(lookup(env)); err != nil || !cfg.GitHub.PrivateKey.Equal(key) || cfg.GitHub.APIOrigin != "http://127.0.0.1:9999" ||
		cfg.GitHub.WebOrigin != "http://127.0.0.1:9998" {
		t.Fatalf("pkcs8 = %+v, %v", cfg.GitHub, err)
	}
}

func TestGitHubAppInvalid(t *testing.T) {
	keyPEM, _ := testPEM(t, false)
	for _, tc := range []struct{ name, value, want string }{
		{EnvGitHubAppSlug, "", EnvGitHubAppSlug},
		{EnvGitHubAppID, "-1", EnvGitHubAppID},
		{EnvGitHubAppID, "abc", EnvGitHubAppID},
		{EnvGitHubAppClientID, "with space", EnvGitHubAppClientID},
		{EnvGitHubAppWebhookSecret, "short", EnvGitHubAppWebhookSecret},
		{EnvGitHubAppSlug, "Tide_App", EnvGitHubAppSlug},
		{EnvGitHubAppPrivateKey, "not a key", EnvGitHubAppPrivateKey},
		{EnvGitHubAppPrivateKey, "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n", EnvGitHubAppPrivateKey},
		{EnvGitHubAPIOrigin, "https://api.github.com/v3", EnvGitHubAPIOrigin},
		{EnvGitHubWebOrigin, "ftp://github.com", EnvGitHubWebOrigin},
	} {
		env := gitHubEnv(t, keyPEM)
		env[tc.name] = tc.value
		_, err := Load(lookup(env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v, want an error that names %s", tc.name, tc.value, err, tc.want)
			continue
		}
		for _, secret := range []string{"client-secret-value", "webhook-secret-value", "MII"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error contains a secret", tc.name)
			}
		}
	}
}
