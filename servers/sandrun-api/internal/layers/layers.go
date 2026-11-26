// SPDX-License-Identifier: FSL-1.1-ALv2

// Package layers lists the extensions of sandrun-api in their registration
// order, so cmd/sandrun-api and the integration tests wire the same layers. The
// order matters: hooks run in registration order, and the contract's
// clean-up order of organization deletion (requests, runners, vendor
// watchers, GitHub links, credentials) follows it.
package layers

import (
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/claude"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/codex"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/cursor"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/devin"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/github"
	"github.com/delinoio/mono/servers/sandrun-api/internal/integration"
	"github.com/delinoio/mono/servers/sandrun-api/internal/link"
	"github.com/delinoio/mono/servers/sandrun-api/internal/metrics"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

// Options configures the layers.
type Options struct {
	// OwnSand builds the OwnSand clients of organizations.
	OwnSand        ownsandclient.Factory
	OwnSandRemoval link.RemovalOptions
	// PublicOrigin is the public origin of sandrun-api (webhook URLs).
	PublicOrigin string
	// WorkTiming and DevSessionTiming override the engine and stream
	// durations (tests); zero values select the production values.
	WorkTiming       *work.Timing
	DevSessionTiming devsession.Timing
	// GitHub configures the GitHub App layer; a nil Client leaves it off.
	GitHub github.Options
	// AgentTiming overrides the agent session watcher and claim poller
	// intervals (tests); zero selects the production values.
	AgentTiming agent.Timing
	// Cursor configures the Cursor vendor (API origin, intervals).
	Cursor cursor.Options
	// Devin configures the Devin vendor (API origin, release grace).
	Devin devin.Options
	// Claude configures the Claude Code vendor (binaries, orchestrator
	// process starter, expected spawn time).
	Claude claude.Options
	// Codex configures the OpenAI Codex vendor (API origin, lookup
	// timeout).
	Codex codex.Options
	// CodexGuestOSes replaces the verified guest OSes of OPENAI_CODEX
	// runner profiles (profile.VerifiedCodexGuestOSes). Only the
	// in-process tests and an acceptance run set it; see the OpenAI Codex
	// section of the contract.
	CodexGuestOSes []sandrunv1.GuestOs
}

// Layers holds the shared parts of the wired layers, for tests.
type Layers struct {
	Metrics      *metrics.Metrics
	Links        *link.Links
	Integrations *integration.Store
	Profiles     *profile.Profiles
	Engine       *work.Engine
	DevSessions  *devsession.Handler
	GitHub       *github.Layer
	Agents       *agent.Hub
	Cursor       *cursor.Vendor
	Devin        *devin.Vendor
	Claude       *claude.Vendor
	Codex        *codex.Vendor
}

// Extensions returns the extensions; New fills out while it wires the app.
func Extensions(opts Options, out *Layers) []app.Extension {
	return []app.Extension{
		func(r *app.Registry) error {
			m, err := metrics.New(r.Service.Metrics)
			if err != nil {
				return err
			}
			out.Metrics = m
			// Delivery IDs of every webhook sender are kept for 7 days.
			r.AddDailyJob(webhook.RetentionJob(r.Service.DB))
			out.Links = link.Register(r, opts.OwnSand, m, opts.OwnSandRemoval)
			out.Integrations = integration.Register(r, opts.PublicOrigin)
			rules := profile.DefaultRules()
			if opts.CodexGuestOSes != nil {
				rules.CodexGuestOSes = opts.CodexGuestOSes
			}
			out.Profiles = profile.Register(r, out.Links, out.Integrations, rules)
			timing := work.DefaultTiming()
			if opts.WorkTiming != nil {
				timing = *opts.WorkTiming
			}
			out.Engine = work.Register(r, out.Links, out.Profiles, m, timing)
			out.DevSessions = devsession.Register(r, out.Engine, opts.DevSessionTiming)
			out.GitHub = github.Register(r, out.Engine, m, opts.GitHub)
			out.Agents = agent.Register(r, out.Engine, out.Integrations, opts.AgentTiming)
			out.Cursor = cursor.Register(out.Agents, opts.Cursor)
			out.Devin = devin.Register(out.Agents, opts.Devin)
			out.Claude = claude.Register(out.Agents, opts.Claude)
			out.Codex = codex.Register(r, out.Agents, m, opts.Codex)
			return nil
		},
	}
}
