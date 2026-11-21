// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Resources are the resources of a new sandbox. All three are required.
type Resources struct {
	VCPUCount int32
	MemoryMiB int64
	DiskGiB   int64
}

// CreateSandboxParams describes a new sandbox. Set exactly one of
// BaseImage, SnapshotID and TemplateID. IdleTTL, MaxTTL and Resources are
// required; the SDK has no default TTL.
type CreateSandboxParams struct {
	// OrganizationID overrides the client default.
	OrganizationID string
	BaseImage      ownsandv1.BaseImage
	SnapshotID     string
	TemplateID     string
	Resources      Resources
	IdleTTL        time.Duration
	MaxTTL         time.Duration
	// DeviceID pins the sandbox to one device.
	DeviceID string
	// HostOS and Arch filter the device. Nil means no filter.
	HostOS *ownsandv1.HostOs
	Arch   *ownsandv1.Arch
	// Network is the network mode. UNSPECIFIED sends no network message,
	// which the server treats as OUTBOUND.
	Network ownsandv1.NetworkMode
	// Labels must not contain secrets.
	Labels map[string]string
}

// request builds the CreateSandboxRequest with the given idempotency key.
func (p *CreateSandboxParams) request(org, key string) *ownsandv1.CreateSandboxRequest {
	req := &ownsandv1.CreateSandboxRequest{
		OrganizationId: org,
		IdempotencyKey: key,
		Source:         &ownsandv1.SandboxSource{},
		Resources: &ownsandv1.Resources{
			VcpuCount: p.Resources.VCPUCount,
			MemoryMib: p.Resources.MemoryMiB,
			DiskGib:   p.Resources.DiskGiB,
		},
		Labels: p.Labels,
	}
	// Each source counts once, so validation rejects zero or several.
	switch {
	case p.BaseImage != ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED && p.SnapshotID == "" && p.TemplateID == "":
		req.Source.Source = &ownsandv1.SandboxSource_BaseImage{BaseImage: p.BaseImage}
	case p.BaseImage == ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED && p.SnapshotID != "" && p.TemplateID == "":
		req.Source.Source = &ownsandv1.SandboxSource_SnapshotId{SnapshotId: p.SnapshotID}
	case p.BaseImage == ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED && p.SnapshotID == "" && p.TemplateID != "":
		req.Source.Source = &ownsandv1.SandboxSource_TemplateId{TemplateId: p.TemplateID}
	}
	if p.IdleTTL != 0 {
		req.IdleTtl = durationpb.New(p.IdleTTL)
	}
	if p.MaxTTL != 0 {
		req.MaxTtl = durationpb.New(p.MaxTTL)
	}
	if p.DeviceID != "" || p.HostOS != nil || p.Arch != nil {
		req.Placement = &ownsandv1.Placement{HostOs: p.HostOS, Arch: p.Arch}
		if p.DeviceID != "" {
			id := p.DeviceID
			req.Placement.DeviceId = &id
		}
	}
	if p.Network != ownsandv1.NetworkMode_NETWORK_MODE_UNSPECIFIED {
		req.Network = &ownsandv1.Network{Mode: p.Network}
	}
	return req
}

// CreateSandbox validates the parameters, generates one idempotency key and
// sends CreateSandbox. It retries a lost response (a transport
// interruption, unavailable or deadline_exceeded) with the same key for up
// to 60 seconds. The returned handle holds the sandbox in CREATING; call
// WaitStable to wait for RUNNING.
func (c *Client) CreateSandbox(ctx context.Context, p CreateSandboxParams) (*Sandbox, error) {
	org, err := c.organization(p.OrganizationID)
	if err != nil {
		return nil, err
	}
	key, err := uuidv7.NewString()
	if err != nil {
		return nil, err
	}
	req := p.request(org, key)
	if err := validate.CreateSandboxRequest(req); err != nil {
		return nil, err
	}
	rec := newRecovery(c.timing.createRetryWindow)
	for {
		// The first attempt is not bounded: the window starts with the
		// first lost response. Each retry is bounded by the time left in
		// the window, because a response that stalls on an open connection
		// must not outlive it (the default HTTP client has no overall
		// timeout). A timer cancels the attempt instead of a context
		// deadline, so Connect sends no timeout header and the server
		// cannot answer it first with a terminal deadline_exceeded status.
		attemptCtx, cancel := ctx, context.CancelCauseFunc(func(error) {})
		var timer *time.Timer
		if rec.inRecovery() {
			left := rec.remaining()
			if left <= 0 {
				return nil, rec.err()
			}
			attemptCtx, cancel = context.WithCancelCause(ctx)
			timer = time.AfterFunc(left, func() { cancel(errCreateDeadline) })
		}
		res, err := c.sandboxes.CreateSandbox(attemptCtx, connect.NewRequest(req))
		if timer != nil {
			timer.Stop()
		}
		timedOut := err != nil && ctx.Err() == nil && errors.Is(context.Cause(attemptCtx), errCreateDeadline)
		cancel(context.Canceled)
		if timedOut {
			// The window ended during the attempt: end with the last
			// failure of the window, not with the cancellation.
			return nil, rec.err()
		}
		if err == nil {
			c.logger.DebugContext(ctx, "ownsand sandbox created", "sandbox_id", res.Msg.GetSandbox().GetSandboxId())
			return c.handle(org, res.Msg.GetSandbox()), nil
		}
		kind, delay := classify(ctx, err)
		if kind == failTerminal {
			return nil, err
		}
		rec.fail(err)
		if berr := rec.backoff(ctx, c.timing, delay); berr != nil {
			return nil, berr
		}
	}
}

// Sandbox returns a handle for an existing sandbox. org may be "" for the
// client default.
func (c *Client) Sandbox(org, sandboxID string) (*Sandbox, error) {
	o, err := c.organization(org)
	if err != nil {
		return nil, err
	}
	return c.handle(o, &ownsandv1.Sandbox{SandboxId: sandboxID, OrganizationId: o}), nil
}

func (c *Client) handle(org string, sb *ownsandv1.Sandbox) *Sandbox {
	s := &Sandbox{c: c, org: org, id: sb.GetSandboxId(), info: sb}
	s.dp = newDataPath(s)
	return s
}

// Sandbox is a handle for one sandbox. It owns at most one peer-to-peer
// session, which carries every concurrent exec and file operation of the
// handle. It is safe for concurrent use.
type Sandbox struct {
	c   *Client
	org string
	id  string
	dp  *dataPath

	mu   sync.Mutex
	info *ownsandv1.Sandbox
}

// ID returns the sandbox ID.
func (s *Sandbox) ID() string { return s.id }

// OrganizationID returns the organization of the sandbox.
func (s *Sandbox) OrganizationID() string { return s.org }

// Info returns the last sandbox record that the handle received.
func (s *Sandbox) Info() *ownsandv1.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *Sandbox) setInfo(sb *ownsandv1.Sandbox) *ownsandv1.Sandbox {
	if sb == nil {
		return nil
	}
	s.mu.Lock()
	s.info = sb
	s.mu.Unlock()
	return sb
}

// Get reads the sandbox.
func (s *Sandbox) Get(ctx context.Context) (*ownsandv1.Sandbox, error) {
	res, err := s.c.sandboxes.GetSandbox(ctx, connect.NewRequest(&ownsandv1.GetSandboxRequest{OrganizationId: s.org, SandboxId: s.id}))
	if err != nil {
		return nil, err
	}
	return s.setInfo(res.Msg.GetSandbox()), nil
}

// Delete deletes the sandbox and closes the handle's data path.
func (s *Sandbox) Delete(ctx context.Context) (*ownsandv1.Sandbox, error) {
	res, err := s.c.sandboxes.DeleteSandbox(ctx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: s.org, SandboxId: s.id}))
	if err != nil {
		return nil, err
	}
	s.dp.close()
	return s.setInfo(res.Msg.GetSandbox()), nil
}

// Pause pauses the sandbox.
func (s *Sandbox) Pause(ctx context.Context) (*ownsandv1.Sandbox, error) {
	res, err := s.c.sandboxes.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: s.org, SandboxId: s.id}))
	if err != nil {
		return nil, err
	}
	return s.setInfo(res.Msg.GetSandbox()), nil
}

// Resume resumes the sandbox.
func (s *Sandbox) Resume(ctx context.Context) (*ownsandv1.Sandbox, error) {
	res, err := s.c.sandboxes.ResumeSandbox(ctx, connect.NewRequest(&ownsandv1.ResumeSandboxRequest{OrganizationId: s.org, SandboxId: s.id}))
	if err != nil {
		return nil, err
	}
	return s.setInfo(res.Msg.GetSandbox()), nil
}

// Snapshot takes a snapshot of the sandbox. The snapshot starts in
// CREATING; watch it with SnapshotService.Get.
func (s *Sandbox) Snapshot(ctx context.Context) (*ownsandv1.Snapshot, error) {
	res, err := s.c.snapshots.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: s.org, SandboxId: s.id}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetSnapshot(), nil
}

// Close closes the handle's peer-to-peer session and cancels a session setup
// that is in flight: that operation returns a "sandbox handle closed" error,
// and a session that the setup still opens is closed and never installed.
// A closed handle rejects later operations that use the peer-to-peer path
// (every transport except TransportRelay) with the same error. Running execs
// continue on the agent and can be attached again with a new handle. Close is
// idempotent.
func (s *Sandbox) Close() error {
	s.dp.close()
	return nil
}

// Transport returns the data path transport that the handle uses now, or
// UNSPECIFIED before the first data path operation.
func (s *Sandbox) Transport() ownsandv1.DataPathTransport {
	return s.dp.current()
}

// IsStable reports whether state is a stable sandbox state: RUNNING,
// PAUSED, or the terminal FAILED or DELETED.
func IsStable(state ownsandv1.SandboxState) bool {
	switch state {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
		ownsandv1.SandboxState_SANDBOX_STATE_FAILED,
		ownsandv1.SandboxState_SANDBOX_STATE_DELETED:
		return true
	}
	return false
}

// IsTerminal reports whether state is DELETED or FAILED.
func IsTerminal(state ownsandv1.SandboxState) bool {
	return state == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || state == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// ErrSandboxEnded reports a sandbox that reached DELETED or FAILED while the
// SDK waited for another state.
type ErrSandboxEnded struct {
	State  ownsandv1.SandboxState
	Reason ownsandv1.SandboxEndReason
}

func (e *ErrSandboxEnded) Error() string {
	return fmt.Sprintf("ownsand: sandbox ended in %s (%s)", e.State, e.Reason)
}

// WaitStable polls GetSandbox until the sandbox is RUNNING, PAUSED, FAILED
// or DELETED and returns that record. Transient errors are retried.
func (s *Sandbox) WaitStable(ctx context.Context) (*ownsandv1.Sandbox, error) {
	return s.waitFor(ctx, IsStable)
}

// waitFor polls until cond holds for the sandbox state.
func (s *Sandbox) waitFor(ctx context.Context, cond func(ownsandv1.SandboxState) bool) (*ownsandv1.Sandbox, error) {
	for {
		sb, err := s.Get(ctx)
		if err != nil {
			if kind, _ := classify(ctx, err); kind == failTerminal {
				return nil, err
			}
		} else if cond(sb.GetState()) {
			return sb, nil
		}
		if err := sleep(ctx, s.c.timing.pollInterval); err != nil {
			return nil, err
		}
	}
}

// waitForResume waits until the sandbox is RUNNING. It returns
// ErrSandboxEnded when the sandbox ends first.
func (s *Sandbox) waitForResume(ctx context.Context) error {
	s.c.logger.DebugContext(ctx, "ownsand waiting for sandbox resume", "sandbox_id", s.id)
	sb, err := s.waitFor(ctx, func(st ownsandv1.SandboxState) bool {
		return st == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || IsTerminal(st)
	})
	if err != nil {
		return err
	}
	if IsTerminal(sb.GetState()) {
		return &ErrSandboxEnded{State: sb.GetState(), Reason: sb.GetEndReason()}
	}
	return nil
}

// endedState returns ErrSandboxEnded when the sandbox is terminal now.
func (s *Sandbox) endedState(ctx context.Context) *ErrSandboxEnded {
	sb, err := s.Get(ctx)
	if err != nil {
		return nil
	}
	if IsTerminal(sb.GetState()) {
		return &ErrSandboxEnded{State: sb.GetState(), Reason: sb.GetEndReason()}
	}
	return nil
}

func isPaused(sb *ownsandv1.Sandbox) bool {
	return sb != nil && sb.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
}
