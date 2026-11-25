// SPDX-License-Identifier: FSL-1.1-ALv2

// Package codex is the OPENAI_CODEX vendor of the AGENT_SESSION workload.
// OpenAI pushes the demand: each OPENAI_CODEX integration has the webhook
// endpoint POST /webhooks/openai/{integration_id} (webhook.go), which
// verifies the Standard Webhooks signature with the integration's webhook
// secret. An agent.session.action_required event with the required action
// type environment_connection makes tide read the session with the
// integration's OpenAI API key (api.agents.read), because the webhook
// carries neither the environment ID nor the remote URL, and queue a
// request whose key is the session ID and the environment ID. A runner
// gets only the environment key (CODEX_API_KEY) and runs
// `codex exec-server --remote <url> --environment-id <id>`. The owning
// contract is the OpenAI Codex section of
// docs/servers-tide-api-foundation.md, which records the endpoints, the
// command line, the version pins and the gaps of the OpenAI documentation:
//
//   - https://developers.openai.com/api/docs/guides/agents-api/environments/self-hosted
//   - https://developers.openai.com/api/docs/guides/agents-api/environments/lifecycle
//   - https://developers.openai.com/api/docs/guides/agents-api/sessions/webhooks
//   - https://developers.openai.com/api/docs/guides/webhooks
package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
	"github.com/delinoio/mono/servers/tide-api/internal/agent/vendorhttp"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// DefaultBaseURL is the OpenAI API origin.
const DefaultBaseURL = "https://api.openai.com"

// DefaultLookupTimeout bounds the session lookup of a webhook delivery.
// OpenAI counts a delivery that takes more than a few seconds as failed
// and retries it.
const DefaultLookupTimeout = 5 * time.Second

// Options configures the vendor.
type Options struct {
	// BaseURL is the OpenAI API origin; empty selects DefaultBaseURL.
	BaseURL string
	// HTTP sends the API calls; nil selects http.DefaultClient.
	HTTP *http.Client
	// LookupTimeout bounds the session lookup of a delivery; zero selects
	// DefaultLookupTimeout.
	LookupTimeout time.Duration
}

// Vendor is the OpenAI Codex vendor.
type Vendor struct {
	hub  *agent.Hub
	opts Options
	m    *metrics.Metrics
	log  *slog.Logger
}

var (
	_ agent.Vendor       = (*Vendor)(nil)
	_ agent.Pusher       = (*Vendor)(nil)
	_ agent.SandboxEnder = (*Vendor)(nil)
)

// Register adds the OpenAI Codex vendor to the Hub and mounts its webhook
// endpoint.
func Register(r *app.Registry, hub *agent.Hub, m *metrics.Metrics, opts Options) *Vendor {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.LookupTimeout == 0 {
		opts.LookupTimeout = DefaultLookupTimeout
	}
	v := &Vendor{hub: hub, opts: opts, m: m, log: hub.Service().Logger}
	hub.AddVendor(v)
	r.Handle("POST /webhooks/openai/{integration_id}", &WebhookHandler{v: v})
	return v
}

func (*Vendor) Vendor() tidev1.AgentVendor { return tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX }
func (*Vendor) Source() work.Source        { return work.SourceOpenAICodex }

// PushesDemand: OpenAI sends webhooks, so the Hub runs no watcher.
func (*Vendor) PushesDemand() {}

// Watch is never called for a Pusher; it waits for ctx.
func (*Vendor) Watch(ctx context.Context, _ *agent.Watch) {
	<-ctx.Done()
}

// api returns a client with the integration's OpenAI API key. The key
// stays in tide: it never enters a runner.
func (v *Vendor) api(ctx context.Context, in integration.Integration) (*vendorhttp.Client, error) {
	key, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_API_KEY)
	if err != nil {
		return nil, fmt.Errorf("read the OpenAI API key: %w", err)
	}
	return &vendorhttp.Client{Vendor: "OPENAI_CODEX", BaseURL: v.opts.BaseURL, HTTP: v.opts.HTTP, Logger: v.log, Auth: func(h http.Header) {
		h.Set("Authorization", "Bearer "+key)
		// The Agents API is a beta API; the OpenAI SDKs send this header on
		// every Agents API call.
		h.Set("OpenAI-Beta", "agents=v1")
	}}, nil
}

// Session is the part of an agent session that tide reads
// (GET /v1/agents/sessions/{id}).
type Session struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	RequiredActions []struct {
		Type          string `json:"type"`
		EnvironmentID string `json:"environment_id"`
	} `json:"required_actions"`
	Environment *struct {
		Type               string `json:"type"`
		ID                 string `json:"id"`
		RemoteURL          string `json:"remote_url"`
		WorkspaceDirectory string `json:"workspace_directory"`
	} `json:"environment"`
}

// NeedsConnection reports whether the session still asks for an executor
// of its self-hosted environment.
func (s *Session) NeedsConnection() bool {
	if s.Environment == nil || s.Environment.Type != "self_hosted" || s.Environment.ID == "" || s.Environment.RemoteURL == "" {
		return false
	}
	for _, a := range s.RequiredActions {
		if a.Type == "environment_connection" {
			return true
		}
	}
	return false
}

// errSessionGone reports a session that OpenAI no longer has.
var errSessionGone = errors.New("the agent session does not exist")

// session reads a session. A 404 is errSessionGone.
func (v *Vendor) session(ctx context.Context, in integration.Integration, id string) (*Session, error) {
	c, err := v.api(ctx, in)
	if err != nil {
		return nil, err
	}
	var s Session
	err = c.Do(ctx, "get_session", http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(id), nil, nil, &s)
	if vendorhttp.Status(err) == http.StatusNotFound {
		return nil, errSessionGone
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CheckStarted reads the sessions of STARTING requests: the API clears the
// environment_connection action when the executor connects, so a session
// without it (and not failed) means that the executor took the session.
func (v *Vendor) CheckStarted(ctx context.Context, in integration.Integration, claims []*agent.Claim) ([]string, error) {
	var out []string
	var errs []error
	for _, c := range claims {
		s, err := v.session(ctx, in, c.VendorRequestID)
		if errors.Is(err, errSessionGone) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if s.Status != "failed" && !s.NeedsConnection() {
			out = append(out, c.RequestID)
		}
	}
	return out, errors.Join(errs...)
}

// Release: OpenAI has no claim to give back. A session that tide does not
// serve fails its submission after the 5-minute connection wait.
func (v *Vendor) Release(context.Context, integration.Integration, *agent.Claim, *work.Request) (bool, error) {
	return true, nil
}

// SandboxEnded: the executor's exit behavior is NOT DOCUMENTED, so the
// profile's max_ttl is the planned end of a Codex runner. A TTL end of the
// sandbox ends the runner with DELETED and the request with ENDED and
// SESSION_TTL_EXPIRED; other ends keep the default.
func (v *Vendor) SandboxEnded(req *work.Request, reason ownsandv1.SandboxEndReason) (work.SandboxOutcome, bool) {
	switch reason {
	case ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED:
		return work.SandboxOutcome{
			RunnerState: tidev1.RunnerState_RUNNER_STATE_DELETED, RunnerReason: tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED,
			Request: work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_TTL_EXPIRED},
		}, true
	}
	return work.SandboxOutcome{}, false
}

// Prepare installs Node.js (when missing) and codex.
func (v *Vendor) Prepare(ctx context.Context, g *work.Guest, _ integration.Integration) error {
	return agent.RunStep(ctx, g, "install the Codex CLI", InstallScript(g.Runner.GuestOS), agent.InstallTimeout, installCodes)
}

// validWorkspace reports whether a workspace directory from OpenAI is an
// absolute POSIX path that tide may create.
func validWorkspace(dir string) bool {
	if !strings.HasPrefix(dir, "/") || len(dir) > 1024 {
		return false
	}
	for _, seg := range strings.Split(dir[1:], "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	for _, r := range dir {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Start creates the session's workspace directory when OpenAI names one,
// writes the environment key to the runner's credential file and starts
// the executor as the self-hosted guide documents:
//
//	CODEX_API_KEY=<environment key> \
//	  codex exec-server --remote <environment.remote_url> --environment-id <environment.id>
func (v *Vendor) Start(ctx context.Context, g *work.Guest, in integration.Integration, c *agent.Claim) (work.StartResult, error) {
	if dir := c.WorkspaceDirectory; dir != "" {
		if !validWorkspace(dir) {
			return work.StartResult{}, fmt.Errorf("the session's workspace directory is not an absolute path")
		}
		if err := agent.RunStep(ctx, g, "create the session workspace", "mkdir -p "+agent.Quote(g.Runner.GuestOS, dir), 2*time.Minute,
			map[int64]string{1: "the guest user cannot create the session's workspace directory " + dir}); err != nil {
			return work.StartResult{}, err
		}
	}
	key, err := v.hub.Credential(ctx, in, tidev1.AgentCredential_AGENT_CREDENTIAL_ENVIRONMENT_KEY)
	if err != nil {
		return work.StartResult{}, err
	}
	dir, err := agent.WriteCredential(ctx, g, key)
	if err != nil {
		return work.StartResult{}, err
	}
	return agent.StartProgram(ctx, g, Program(dir, c))
}

// Program returns the executor of a claim.
func Program(dir string, c *agent.Claim) agent.Program {
	return agent.Program{Dir: dir, CredentialEnv: "CODEX_API_KEY", FirstBins: []string{nodeBin},
		Args: []string{"codex", "exec-server", "--remote", c.Endpoint, "--environment-id", c.Scope}}
}
