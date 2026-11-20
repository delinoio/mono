package ownsand

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// MaxTTL is the largest allowed sandbox max TTL.
const MaxTTL = 7 * 24 * time.Hour

// CreateSandboxOptions describe a new sandbox. Exactly one source is
// required, and so are the resources and both TTLs. There is no default TTL.
type CreateSandboxOptions struct {
	// BaseImage, SnapshotID and TemplateID are the sources; set exactly one.
	BaseImage  ownsandv1.BaseImage
	SnapshotID string
	TemplateID string

	VCPUCount int64
	MemoryMiB int64
	DiskGiB   int64

	IdleTTL time.Duration
	MaxTTL  time.Duration

	// Placement filters; zero values mean no filter.
	DeviceID string
	HostOS   ownsandv1.HostOs
	Arch     ownsandv1.Arch

	// Network is NETWORK_MODE_OUTBOUND when unspecified.
	Network ownsandv1.NetworkMode
	// Labels must not contain secrets.
	Labels map[string]string
}

// ErrInvalidArgument wraps request validation errors that the SDK detects
// before it sends a request.
var ErrInvalidArgument = errors.New("invalid argument")

func invalid(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, a...)))
}

// Validate checks the options with the rules of the create request.
func (o *CreateSandboxOptions) Validate() error {
	sources := 0
	if o.BaseImage != ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED {
		sources++
	}
	if o.SnapshotID != "" {
		sources++
	}
	if o.TemplateID != "" {
		sources++
	}
	if sources != 1 {
		return invalid("exactly one of base image, snapshot and template is required")
	}
	if o.VCPUCount <= 0 || o.MemoryMiB <= 0 || o.DiskGiB <= 0 {
		return invalid("vCPU count, memory and disk are required and must be greater than zero")
	}
	if o.IdleTTL <= 0 || o.MaxTTL <= 0 {
		return invalid("idle TTL and max TTL are required and must be greater than zero")
	}
	if o.IdleTTL > o.MaxTTL {
		return invalid("idle TTL must not be greater than max TTL")
	}
	if o.MaxTTL > MaxTTL {
		return invalid("max TTL must not be greater than 7 days")
	}
	return nil
}

func (o *CreateSandboxOptions) request(org, key string) *ownsandv1.CreateSandboxRequest {
	req := &ownsandv1.CreateSandboxRequest{
		OrganizationId: org,
		IdempotencyKey: key,
		Source:         &ownsandv1.SandboxSource{},
		Resources:      &ownsandv1.Resources{VcpuCount: o.VCPUCount, MemoryMib: o.MemoryMiB, DiskGib: o.DiskGiB},
		IdleTtl:        durationpb.New(o.IdleTTL),
		MaxTtl:         durationpb.New(o.MaxTTL),
		Labels:         o.Labels,
	}
	switch {
	case o.BaseImage != ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED:
		req.Source.Source = &ownsandv1.SandboxSource_BaseImage{BaseImage: o.BaseImage}
	case o.SnapshotID != "":
		req.Source.Source = &ownsandv1.SandboxSource_SnapshotId{SnapshotId: o.SnapshotID}
	default:
		req.Source.Source = &ownsandv1.SandboxSource_TemplateId{TemplateId: o.TemplateID}
	}
	if o.DeviceID != "" || o.HostOS != ownsandv1.HostOs_HOST_OS_UNSPECIFIED || o.Arch != ownsandv1.Arch_ARCH_UNSPECIFIED {
		req.Placement = &ownsandv1.Placement{}
		if o.DeviceID != "" {
			id := o.DeviceID
			req.Placement.DeviceId = &id
		}
		if o.HostOS != ownsandv1.HostOs_HOST_OS_UNSPECIFIED {
			h := o.HostOS
			req.Placement.HostOs = &h
		}
		if o.Arch != ownsandv1.Arch_ARCH_UNSPECIFIED {
			a := o.Arch
			req.Placement.Arch = &a
		}
	}
	if o.Network != ownsandv1.NetworkMode_NETWORK_MODE_UNSPECIFIED {
		req.Network = &ownsandv1.NetworkPolicy{Mode: o.Network}
	}
	return req
}

// createRetryWindow bounds the retries of one logical create.
const createRetryWindow = 60 * time.Second

// CreateSandbox creates a sandbox. It validates the options before it sends
// the request, generates one idempotency key for the logical create and
// reuses it when it retries after a lost response or an unavailable server.
// The returned sandbox is usually CREATING; use Sandbox.Wait to watch it.
func (c *Client) CreateSandbox(ctx context.Context, o CreateSandboxOptions, opts ...CallOption) (*Sandbox, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	key := datapath.NewUUIDv7At(c.clk.Now()).String()
	req := o.request(org, key)
	deadline := c.clk.Now().Add(createRetryWindow)
	b := newBackoff()
	for {
		resp, err := c.sandboxes.CreateSandbox(ctx, connect.NewRequest(req))
		if err == nil {
			c.log.Debug("sandbox created", slog.String("sandbox_id", resp.Msg.GetSandbox().GetSandboxId()))
			return c.handle(org, resp.Msg.GetSandbox()), nil
		}
		f := classify(ctx, err)
		if !(f.transport || f.code == connect.CodeUnavailable) || c.clk.Now().After(deadline) {
			return nil, c.redact(err)
		}
		if err := clock.Sleep(ctx, c.clk, b.next(0)); err != nil {
			return nil, c.redact(err)
		}
	}
}

// Sandbox returns a handle for an existing sandbox without an RPC.
func (c *Client) Sandbox(sandboxID string, opts ...CallOption) (*Sandbox, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, err
	}
	return c.handle(org, &ownsandv1.Sandbox{OrganizationId: org, SandboxId: sandboxID}), nil
}

func (c *Client) handle(org string, info *ownsandv1.Sandbox) *Sandbox {
	s := &Sandbox{c: c, org: org, id: info.GetSandboxId(), info: info}
	s.dp = newDataPath(c, org, s.id)
	return s
}

// GetSandbox reads one sandbox and returns its handle.
func (c *Client) GetSandbox(ctx context.Context, sandboxID string, opts ...CallOption) (*Sandbox, error) {
	s, err := c.Sandbox(sandboxID, opts...)
	if err != nil {
		return nil, err
	}
	if _, err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ListSandboxesOptions filter ListSandboxes.
type ListSandboxesOptions struct {
	ListOptions
	DeviceID string
	// States filters by state; asking for DELETED or FAILED includes ended sandboxes.
	States []ownsandv1.SandboxState
}

// ListSandboxes lists sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, lo ListSandboxesOptions, opts ...CallOption) ([]*ownsandv1.Sandbox, string, error) {
	org, err := c.orgFor(opts)
	if err != nil {
		return nil, "", err
	}
	req := &ownsandv1.ListSandboxesRequest{OrganizationId: org, PageSize: lo.PageSize, PageToken: lo.PageToken, States: lo.States}
	if lo.DeviceID != "" {
		req.DeviceId = &lo.DeviceID
	}
	r, err := call(c, ctx, c.sandboxes.ListSandboxes, req)
	return r.GetSandboxes(), r.GetNextPageToken(), err
}

// Sandbox is a handle of one sandbox. It holds at most one peer-to-peer
// session, which it opens only while an operation is open. Close the handle
// to close that session.
type Sandbox struct {
	c    *Client
	org  string
	id   string
	info *ownsandv1.Sandbox
	dp   *dataPath
}

// ID returns the sandbox ID.
func (s *Sandbox) ID() string { return s.id }

// OrganizationID returns the organization of the sandbox.
func (s *Sandbox) OrganizationID() string { return s.org }

// Info returns the last sandbox record that the handle received.
func (s *Sandbox) Info() *ownsandv1.Sandbox { return s.info }

// Transport reports the data path transport that the handle uses now
// (UNSPECIFIED before the first data path operation).
func (s *Sandbox) Transport() ownsandv1.DataPathTransport { return s.dp.current() }

// Close closes the peer-to-peer session of the handle, if any.
func (s *Sandbox) Close() error {
	s.dp.close()
	return nil
}

// Refresh reads the sandbox.
func (s *Sandbox) Refresh(ctx context.Context) (*ownsandv1.Sandbox, error) {
	r, err := call(s.c, ctx, s.c.sandboxes.GetSandbox, &ownsandv1.GetSandboxRequest{OrganizationId: s.org, SandboxId: s.id})
	if err != nil {
		return nil, err
	}
	s.info = r.GetSandbox()
	return s.info, nil
}

// Delete deletes the sandbox.
func (s *Sandbox) Delete(ctx context.Context) (*ownsandv1.Sandbox, error) {
	s.dp.close()
	r, err := call(s.c, ctx, s.c.sandboxes.DeleteSandbox, &ownsandv1.DeleteSandboxRequest{OrganizationId: s.org, SandboxId: s.id})
	if err == nil {
		s.info = r.GetSandbox()
	}
	return r.GetSandbox(), err
}

// Pause pauses the sandbox.
func (s *Sandbox) Pause(ctx context.Context) (*ownsandv1.Sandbox, error) {
	r, err := call(s.c, ctx, s.c.sandboxes.PauseSandbox, &ownsandv1.PauseSandboxRequest{OrganizationId: s.org, SandboxId: s.id})
	if err == nil {
		s.info = r.GetSandbox()
	}
	return r.GetSandbox(), err
}

// Resume resumes the sandbox.
func (s *Sandbox) Resume(ctx context.Context) (*ownsandv1.Sandbox, error) {
	r, err := call(s.c, ctx, s.c.sandboxes.ResumeSandbox, &ownsandv1.ResumeSandboxRequest{OrganizationId: s.org, SandboxId: s.id})
	if err == nil {
		s.info = r.GetSandbox()
	}
	return r.GetSandbox(), err
}

// Wait polls the sandbox until it reaches a stable state (RUNNING, PAUSED,
// DELETED or FAILED) and returns it.
func (s *Sandbox) Wait(ctx context.Context, interval time.Duration) (*ownsandv1.Sandbox, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		info, err := s.Refresh(ctx)
		if err != nil {
			return nil, err
		}
		switch info.GetState() {
		case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
			ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxState_SANDBOX_STATE_FAILED:
			return info, nil
		}
		if err := clock.Sleep(ctx, s.c.clk, interval); err != nil {
			return nil, err
		}
	}
}

// Snapshot takes a snapshot of the sandbox on its device.
func (s *Sandbox) Snapshot(ctx context.Context) (*ownsandv1.Snapshot, error) {
	r, err := call(s.c, ctx, s.c.snapshots.CreateSnapshot, &ownsandv1.CreateSnapshotRequest{OrganizationId: s.org, SandboxId: s.id})
	return r.GetSnapshot(), err
}

// CreateSnapshot takes a snapshot of a sandbox by ID.
func (c *Client) CreateSnapshot(ctx context.Context, sandboxID string, opts ...CallOption) (*ownsandv1.Snapshot, error) {
	s, err := c.Sandbox(sandboxID, opts...)
	if err != nil {
		return nil, err
	}
	return s.Snapshot(ctx)
}
