// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ownsandclient is the tide-api side of the OwnSand API: a small
// Client interface over the OwnSand Go SDK (go/ownsand), so tests can use a
// fake (package ownsandfake), and an instrumented wrapper that counts every
// call in tide_ownsand_calls_total{rpc,code} and logs every call result.
// tide uses OwnSand only through the public API with the OwnSand API key of
// each organization; it never imports code from servers/ownsand-api. The
// owning contract is docs/servers-tide-api-foundation.md (OwnSand link,
// Queue and dispatch, Logging).
package ownsandclient

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
)

// Client is one OwnSand organization, authenticated with its API key.
// Errors are Connect errors of the OwnSand API (or of the SDK's data path)
// and never contain the key.
type Client interface {
	// ListDevices returns every device that is not removed.
	ListDevices(ctx context.Context) ([]*ownsandv1.Device, error)
	// TemplateGuestOS returns the guest OS of a template (the guest OS of
	// its snapshot). An unknown template fails with not_found.
	TemplateGuestOS(ctx context.Context, templateID string) (ownsandv1.GuestOs, error)
	// CreateSandbox sends the request as given, including the caller's
	// idempotency_key, without retries: the caller stores the key first and
	// retries with the same key.
	CreateSandbox(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.Sandbox, error)
	GetSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error)
	DeleteSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error)
}

// Factory returns a client for an OwnSand organization and its API key.
type Factory interface {
	New(ownsandOrganizationID, apiKey string) (Client, error)
}

// SDKFactory builds clients on the OwnSand Go SDK.
type SDKFactory struct {
	// BaseURL is the OwnSand API origin.
	BaseURL string
	// Logger receives the SDK's own debug records (no secrets).
	Logger *slog.Logger
}

// New returns an SDK client. The data path uses the OwnSand relay
// (recorded decision in the contract): tide-api is a server that relays one
// operation per RPC, so a peer-to-peer session per call would only add setup
// time.
func (f SDKFactory) New(ownsandOrganizationID, apiKey string) (Client, error) {
	c, err := ownsand.New(ownsand.Options{
		BaseURL:        f.BaseURL,
		APIKey:         apiKey,
		OrganizationID: ownsandOrganizationID,
		Logger:         f.Logger,
		Transport:      ownsand.TransportRelay,
		// Never read OWNSAND_API_KEY or OWNSAND_ORG from the server
		// environment.
		LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		return nil, err
	}
	return &sdkClient{c: c, org: ownsandOrganizationID}, nil
}

type sdkClient struct {
	c   *ownsand.Client
	org string
}

func (s *sdkClient) ListDevices(ctx context.Context) ([]*ownsandv1.Device, error) {
	var out []*ownsandv1.Device
	token := ""
	for {
		res, err := s.c.Devices().List(ctx, &ownsandv1.ListDevicesRequest{OrganizationId: s.org, PageSize: 200, PageToken: token})
		if err != nil {
			return nil, err
		}
		out = append(out, res.GetDevices()...)
		if token = res.GetNextPageToken(); token == "" {
			return out, nil
		}
	}
}

func (s *sdkClient) TemplateGuestOS(ctx context.Context, templateID string) (ownsandv1.GuestOs, error) {
	token := ""
	for {
		res, err := s.c.Snapshots().ListTemplates(ctx, &ownsandv1.ListTemplatesRequest{OrganizationId: s.org, PageSize: 200, PageToken: token})
		if err != nil {
			return 0, err
		}
		for _, t := range res.GetTemplates() {
			if t.GetTemplateId() == templateID {
				snap, err := s.c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{OrganizationId: s.org, SnapshotId: t.GetSnapshotId()})
				if err != nil {
					return 0, err
				}
				return snap.GetSnapshot().GetGuestOs(), nil
			}
		}
		if token = res.GetNextPageToken(); token == "" {
			return 0, connect.NewError(connect.CodeNotFound, errors.New("template not found"))
		}
	}
}

func (s *sdkClient) CreateSandbox(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.Sandbox, error) {
	res, err := s.c.Sandboxes().Create(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.GetSandbox(), nil
}

func (s *sdkClient) GetSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	res, err := s.c.Sandboxes().Get(ctx, &ownsandv1.GetSandboxRequest{OrganizationId: s.org, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	return res.GetSandbox(), nil
}

func (s *sdkClient) DeleteSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	res, err := s.c.Sandboxes().Delete(ctx, &ownsandv1.DeleteSandboxRequest{OrganizationId: s.org, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	return res.GetSandbox(), nil
}

// Code returns the Connect code of an OwnSand call result as the metric and
// log value ("ok" for nil).
func Code(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled.String()
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded.String()
	}
	return connect.CodeOf(err).String()
}

// Instrumented wraps a client: it counts and logs every call, and calls
// OnUnauthenticated when OwnSand rejects the key, so the link layer marks the
// link INVALID.
type Instrumented struct {
	Inner   Client
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// OrganizationID is the tide organization, for the log records.
	OrganizationID    string
	OnUnauthenticated func()
}

// Observe records one call; the data path operations of later layers use it
// too.
func (c *Instrumented) Observe(ctx context.Context, rpc string, start time.Time, err error, attrs ...slog.Attr) {
	code := Code(err)
	c.Metrics.OwnSandCall(rpc, code)
	all := append([]slog.Attr{
		slog.String("rpc", rpc), slog.String("code", code), slog.String("organization_id", c.OrganizationID),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}, attrs...)
	level := slog.LevelInfo
	if err != nil && code != connect.CodeCanceled.String() {
		level = slog.LevelWarn
	}
	c.Logger.LogAttrs(ctx, level, "ownsand_call", all...)
	if connect.CodeOf(err) == connect.CodeUnauthenticated && c.OnUnauthenticated != nil {
		c.OnUnauthenticated()
	}
}

func (c *Instrumented) ListDevices(ctx context.Context) ([]*ownsandv1.Device, error) {
	start := time.Now()
	d, err := c.Inner.ListDevices(ctx)
	c.Observe(ctx, "ListDevices", start, err)
	return d, err
}

func (c *Instrumented) TemplateGuestOS(ctx context.Context, templateID string) (ownsandv1.GuestOs, error) {
	start := time.Now()
	g, err := c.Inner.TemplateGuestOS(ctx, templateID)
	c.Observe(ctx, "ListTemplates", start, err, slog.String("ownsand_template_id", templateID))
	return g, err
}

func (c *Instrumented) CreateSandbox(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.Sandbox, error) {
	start := time.Now()
	sb, err := c.Inner.CreateSandbox(ctx, req)
	c.Observe(ctx, "CreateSandbox", start, err, slog.String("sandbox_id", sb.GetSandboxId()), slog.String("runner_id", req.GetLabels()[LabelRunnerID]))
	return sb, err
}

func (c *Instrumented) GetSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	start := time.Now()
	sb, err := c.Inner.GetSandbox(ctx, sandboxID)
	c.Observe(ctx, "GetSandbox", start, err, slog.String("sandbox_id", sandboxID))
	return sb, err
}

func (c *Instrumented) DeleteSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	start := time.Now()
	sb, err := c.Inner.DeleteSandbox(ctx, sandboxID)
	c.Observe(ctx, "DeleteSandbox", start, err, slog.String("sandbox_id", sandboxID))
	return sb, err
}

// OwnSand labels of tide sandboxes. Labels hold no secrets.
const (
	LabelOrganizationID  = "tide.organization-id"
	LabelRunnerID        = "tide.runner-id"
	LabelRunnerProfileID = "tide.runner-profile-id"
)
