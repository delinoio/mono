// SPDX-License-Identifier: FSL-1.1-ALv2

// Package layers lists the extensions of tide-api in their registration
// order, so cmd/tide-api and the integration tests wire the same layers. The
// order matters: hooks run in registration order, and the contract's
// clean-up order of organization deletion (requests, runners, vendor
// watchers, GitHub links, credentials) follows it.
package layers

import (
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/integration"
	"github.com/delinoio/mono/servers/tide-api/internal/link"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// Options configures the layers.
type Options struct {
	// OwnSand builds the OwnSand clients of organizations.
	OwnSand ownsandclient.Factory
	// PublicOrigin is the public origin of tide-api (webhook URLs).
	PublicOrigin string
}

// Layers holds the shared parts of the wired layers, for tests.
type Layers struct {
	Metrics      *metrics.Metrics
	Links        *link.Links
	Integrations *integration.Store
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
			out.Links = link.Register(r, opts.OwnSand, m)
			out.Integrations = integration.Register(r, opts.PublicOrigin)
			return nil
		},
	}
}
