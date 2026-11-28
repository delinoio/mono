// SPDX-License-Identifier: Apache-2.0

package vendor

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

var environ = []string{
	"PATH=/usr/bin",
	"SANDCREW_SESSION_ID=s",
	"SANDCREW_VENDOR_CREDENTIAL=secret",
	"sandcrew_lower=x",
	"ANTHROPIC_API_KEY=from-image",
	"codex_home=/tmp/x",
	"GITHUB_TOKEN=session-secret",
}

func TestClaudeEnvironment(t *testing.T) {
	spec, _ := Lookup(sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE)
	for kind, want := range map[sandcrewv1.VendorCredentialKind]string{
		sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN: "CLAUDE_CODE_OAUTH_TOKEN=secret",
		sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY:            "ANTHROPIC_API_KEY=secret",
	} {
		launch, err := spec.Prepare(environ, Credential{Kind: kind, Value: "secret"}, "/w/.sandcrew")
		if err != nil {
			t.Fatal(err)
		}
		wantEnv := []string{"PATH=/usr/bin", "GITHUB_TOKEN=session-secret", "IS_SANDBOX=1", want}
		if !slices.Equal(launch.Env, wantEnv) || launch.AuthMethodID != "" || launch.CredentialFile != "" {
			t.Fatalf("%s: launch %+v, want env %v", kind, launch, wantEnv)
		}
	}
}

func TestCodexEnvironment(t *testing.T) {
	spec, _ := Lookup(sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX)
	state := filepath.Join("w", ".sandcrew")
	launch, err := spec.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY, Value: "secret"}, state)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(launch.Env, "CODEX_API_KEY=secret") || !slices.Contains(launch.Env, "CODEX_HOME="+filepath.Join(state, "codex-home")) || launch.AuthMethodID != "api-key" {
		t.Fatalf("API key launch %+v", launch)
	}
	launch, err = spec.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON, CodexAuthFileExists: true}, state)
	if err != nil {
		t.Fatal(err)
	}
	if launch.AuthMethodID != "" || launch.CredentialFile != filepath.Join(state, "codex-home", "auth.json") {
		t.Fatalf("auth.json launch %+v", launch)
	}
	for _, entry := range launch.Env {
		if strings.Contains(entry, "secret") && !strings.HasPrefix(entry, "GITHUB_TOKEN") {
			t.Fatalf("auth.json launch passes the bridge credential: %s", entry)
		}
	}
}

func TestMissingCredentials(t *testing.T) {
	claude, _ := Lookup(sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE)
	codex, _ := Lookup(sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX)
	for name, check := range map[string]func() error{
		"empty token": func() error {
			_, err := claude.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN, Value: " "}, "/s")
			return err
		},
		"auth.json for claude": func() error {
			_, err := claude.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON, CodexAuthFileExists: true}, "/s")
			return err
		},
		"oauth token for codex": func() error {
			_, err := codex.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN, Value: "secret"}, "/s")
			return err
		},
		"missing auth.json": func() error {
			_, err := codex.Prepare(environ, Credential{Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON}, "/s")
			return err
		},
	} {
		err := check()
		if !errors.Is(err, ErrCredentialMissing) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s: error %v", name, err)
		}
	}
}
