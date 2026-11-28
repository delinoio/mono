// SPDX-License-Identifier: Apache-2.0

// Package vendor holds the vendor table of the bridge: how each vendor's
// ACP adapter gets its credential, which modes approve automatically or ask,
// and which environment names the bridge reserves. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#vendors; the values were checked
// against the pinned adapter sources (see that contract).
package vendor

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// Environment variables of the bridge process.
const (
	EnvPrefix            = "SANDCREW_"
	EnvSessionID         = "SANDCREW_SESSION_ID"
	EnvVendorCredential  = "SANDCREW_VENDOR_CREDENTIAL"
	envAnthropicAPIKey   = "ANTHROPIC_API_KEY"
	envClaudeOAuthToken  = "CLAUDE_CODE_OAUTH_TOKEN"
	envOpenAIAPIKey      = "OPENAI_API_KEY"
	envCodexAPIKey       = "CODEX_API_KEY"
	envCodexHome         = "CODEX_HOME"
	envIsSandbox         = "IS_SANDBOX"
	codexAPIKeyAuthID    = "api-key"
	codexHomeDirName     = "codex-home"
	codexAuthFileName    = "auth.json"
	claudeAutoApproveID  = "bypassPermissions"
	claudeAskModeID      = "default"
	codexAutoApproveID   = "agent-full-access"
	codexAskModeID       = "workspace-write"
	isSandboxEnableValue = "1"
)

// ReservedEnv lists the environment names that the bridge sets or removes
// for the agent, besides every SANDCREW_* name. Session secrets must not
// use them.
var ReservedEnv = []string{
	envAnthropicAPIKey,
	envClaudeOAuthToken,
	envOpenAIAPIKey,
	envCodexAPIKey,
	envCodexHome,
	envIsSandbox,
}

// ErrCredentialMissing marks a missing or unusable vendor credential.
var ErrCredentialMissing = errors.New("vendor: credential missing")

// Spec is the integration of one vendor.
type Spec struct {
	Vendor sandcrewv1.AgentVendor
	// AutoApproveModeID is the agent mode that approves every action. The
	// bridge switches to it in AUTO_APPROVE when the agent offers it.
	AutoApproveModeID string
	// AskModeID is the agent mode that asks for permission. The bridge
	// switches to it in ASK when the agent offers it.
	AskModeID string
	// CredentialKinds are the credential kinds that the vendor accepts.
	CredentialKinds []sandcrewv1.VendorCredentialKind
}

var specs = map[sandcrewv1.AgentVendor]Spec{
	sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE: {
		Vendor:            sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE,
		AutoApproveModeID: claudeAutoApproveID,
		AskModeID:         claudeAskModeID,
		CredentialKinds: []sandcrewv1.VendorCredentialKind{
			sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY,
			sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN,
		},
	},
	sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX: {
		Vendor:            sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX,
		AutoApproveModeID: codexAutoApproveID,
		AskModeID:         codexAskModeID,
		CredentialKinds: []sandcrewv1.VendorCredentialKind{
			sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY,
			sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON,
		},
	},
}

// Lookup returns the spec of vendor.
func Lookup(vendor sandcrewv1.AgentVendor) (Spec, bool) {
	spec, ok := specs[vendor]
	return spec, ok
}

// Credential is the vendor credential of a session.
type Credential struct {
	Kind sandcrewv1.VendorCredentialKind
	// Value is SANDCREW_VENDOR_CREDENTIAL for API_KEY and
	// CLAUDE_OAUTH_TOKEN. It is empty for CODEX_AUTH_JSON.
	Value string
	// CodexAuthFileExists reports whether `<state>/codex-home/auth.json`
	// exists, for CODEX_AUTH_JSON.
	CodexAuthFileExists bool
}

// Launch is what the bridge gives the agent process.
type Launch struct {
	// Env is the complete environment of the agent process.
	Env []string
	// AuthMethodID is the ACP `authenticate` method that the bridge calls
	// after `initialize`, or empty.
	AuthMethodID string
	// CredentialFile is the guest file that the bridge watches for agent
	// refreshes, or empty.
	CredentialFile string
}

// CodexHome returns the CODEX_HOME directory in stateDir.
func CodexHome(stateDir string) string {
	return filepath.Join(stateDir, codexHomeDirName)
}

// CodexAuthFile returns the Codex auth.json path in stateDir.
func CodexAuthFile(stateDir string) string {
	return filepath.Join(stateDir, codexHomeDirName, codexAuthFileName)
}

// Prepare checks the credential and returns the agent environment: the
// bridge environment without SANDCREW_* and reserved names, plus the
// vendor's credential variables. Session secrets pass unchanged. A missing
// credential returns an error that wraps ErrCredentialMissing; the error
// never contains the credential.
func (s Spec) Prepare(environ []string, credential Credential, stateDir string) (Launch, error) {
	if !slices.Contains(s.CredentialKinds, credential.Kind) {
		return Launch{}, fmt.Errorf("%w: %s does not accept %s", ErrCredentialMissing, s.Vendor, credential.Kind)
	}
	env := FilterEnv(environ)
	launch := Launch{}
	switch credential.Kind {
	case sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY,
		sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN:
		if strings.TrimSpace(credential.Value) == "" {
			return Launch{}, fmt.Errorf("%w: %s is empty", ErrCredentialMissing, EnvVendorCredential)
		}
	case sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON:
		if !credential.CodexAuthFileExists {
			return Launch{}, fmt.Errorf("%w: %s/%s does not exist", ErrCredentialMissing, codexHomeDirName, codexAuthFileName)
		}
	}
	switch s.Vendor {
	case sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		// The adapter offers bypassPermissions only to a non-root process
		// or when IS_SANDBOX is set. Guests can run the bridge as root, and
		// the sandbox is the security boundary.
		env = append(env, envIsSandbox+"="+isSandboxEnableValue)
		if credential.Kind == sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN {
			env = append(env, envClaudeOAuthToken+"="+credential.Value)
		} else {
			env = append(env, envAnthropicAPIKey+"="+credential.Value)
		}
	case sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		env = append(env, envCodexHome+"="+CodexHome(stateDir))
		if credential.Kind == sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY {
			// The adapter uses an API key only after an `authenticate` call
			// with the api-key method.
			env = append(env, envCodexAPIKey+"="+credential.Value)
			launch.AuthMethodID = codexAPIKeyAuthID
		} else {
			launch.CredentialFile = CodexAuthFile(stateDir)
		}
	}
	launch.Env = env
	return launch, nil
}

// FilterEnv removes every SANDCREW_* variable and every reserved name.
func FilterEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), EnvPrefix) || isReserved(name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isReserved(name string) bool {
	for _, reserved := range ReservedEnv {
		// Windows environment names are case-insensitive.
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}
