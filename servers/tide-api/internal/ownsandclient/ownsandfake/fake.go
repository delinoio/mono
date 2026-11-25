// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ownsandfake is an in-memory fake of the OwnSand API for tide-api
// tests. It implements ownsandclient.Factory and Client: API keys that
// belong to fake organizations, devices, templates, and sandboxes with a
// capacity limit, the idempotency of CreateSandbox, guest files and execs.
// It is not a model of the OwnSand scheduler: a test sets the capacity
// (MaxSandboxes), scripts CreateSandbox errors and ends sandboxes itself.
// Its own errors are wire errors (connect.NewWireError), as OwnSand sends
// them; a scripted error made with connect.NewError models a transport
// failure or an intermediary's answer.
package ownsandfake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// ExecFunc runs a fake exec. It may send events through p.OnEvent and must
// return when ctx ends (the caller's stream ended; a real command would
// keep running). Returning (nil, ctx.Err()) is a valid stream end.
type ExecFunc func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error)

// AttachFunc continues a fake exec.
type AttachFunc func(ctx context.Context, sandboxID, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error)

// Fake is the fake OwnSand API.
type Fake struct {
	mu    sync.Mutex
	orgs  map[string]*Org
	calls map[string]int
	// Exec and Attach run execs; nil runs every command with exit code 0 and
	// no output.
	Exec   ExecFunc
	Attach AttachFunc
	// createHook runs before each CreateSandbox call without the fake lock
	// (for example to stall a call until its context ends); its error is
	// the call's error.
	createHook func(context.Context, *ownsandv1.CreateSandboxRequest) error
	deleteHook func(ctx context.Context) error
}

// Org is one fake OwnSand organization.
type Org struct {
	ID     string
	APIKey string
	// Devices are returned by ListDevices.
	Devices []*ownsandv1.Device
	// Templates maps a template ID to its guest OS.
	Templates map[string]ownsandv1.GuestOs
	// MaxSandboxes bounds the sandboxes that are not ended; CreateSandbox
	// fails with unavailable beyond it. Zero means no limit.
	MaxSandboxes int
	// CreateError, when set, is the error of the next CreateSandbox calls.
	CreateError error
	// CreateResponseError, when set, models a lost response: CreateSandbox
	// creates the sandbox (or finds the one of its key) and returns this
	// error instead of it.
	CreateResponseError error
	// DeleteError, when set, is the error of the next DeleteSandbox calls
	// (for example a transient OwnSand failure).
	DeleteError error
	// FileError, when set, is returned for reads, write chunks and finalization.
	FileError error
	// OnFileRead observes the context and parameters before any chunks are
	// written. It runs without the fake lock.
	OnFileRead func(context.Context, ownsand.FileReadParams)
	// StartRunning makes new sandboxes RUNNING at once (the default);
	// false keeps them CREATING until the test calls SetRunning.
	StartRunning bool

	sandboxes map[string]*fakeSandbox
	keys      map[string]string // idempotency key -> sandbox ID
	// requests holds the encoded request of each key: like OwnSand, a key
	// that comes again with other parameters fails with already_exists.
	requests map[string][]byte
}

type fakeSandbox struct {
	sb    *ownsandv1.Sandbox
	files map[string][]byte
	// writes of FileWrite operations, by write ID.
	writes map[string]*fakeWrite
	// reads keeps the snapshot of each read ID.
	reads map[string][]byte
}

type fakeWrite struct {
	path   string
	ranges map[int64][]byte
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{orgs: map[string]*Org{}, calls: map[string]int{}}
}

// AddOrg adds an organization with one API key.
func (f *Fake) AddOrg(id, apiKey string) *Org {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.orgs[id]; ok {
		o.APIKey = apiKey
		return o
	}
	o := &Org{ID: id, APIKey: apiKey, Templates: map[string]ownsandv1.GuestOs{}, StartRunning: true,
		sandboxes: map[string]*fakeSandbox{}, keys: map[string]string{}, requests: map[string][]byte{}}
	f.orgs[id] = o
	return o
}

// Update runs fn with the fake lock held, so a test can change an
// organization's fields safely.
func (f *Fake) Update(orgID string, fn func(o *Org)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.orgs[orgID])
}

// RevokeKey makes the organization's key invalid (unauthenticated).
func (f *Fake) RevokeKey(id string) { f.Update(id, func(o *Org) { o.APIKey = "" }) }

// SetTemplate records a template of an organization.
func (f *Fake) SetTemplate(orgID, templateID string, guest ownsandv1.GuestOs) {
	f.Update(orgID, func(o *Org) { o.Templates[templateID] = guest })
}

// SetDevices replaces the devices of an organization.
func (f *Fake) SetDevices(orgID string, devices ...*ownsandv1.Device) {
	f.Update(orgID, func(o *Org) { o.Devices = devices })
}

// Calls returns the number of calls of an RPC.
func (f *Fake) Calls(rpc string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[rpc]
}

// Sandboxes returns copies of the sandboxes of an organization, oldest
// first.
func (f *Fake) Sandboxes(orgID string) []*ownsandv1.Sandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ownsandv1.Sandbox
	for _, s := range f.orgs[orgID].sandboxes {
		out = append(out, proto.Clone(s.sb).(*ownsandv1.Sandbox))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetSandboxId() < out[j].GetSandboxId() })
	return out
}

// Active returns the number of sandboxes that are not ended.
func (f *Fake) Active(orgID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orgs[orgID].active()
}

func (o *Org) active() int {
	n := 0
	for _, s := range o.sandboxes {
		if !ownsand.IsTerminal(s.sb.GetState()) {
			n++
		}
	}
	return n
}

// SetRunning moves a CREATING sandbox to RUNNING.
func (f *Fake) SetRunning(orgID, sandboxID string) {
	f.Update(orgID, func(o *Org) { o.sandboxes[sandboxID].sb.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING })
}

// EndSandbox ends a sandbox with an OwnSand end reason, as a TTL or a
// device removal does.
func (f *Fake) EndSandbox(orgID, sandboxID string, reason ownsandv1.SandboxEndReason, state ownsandv1.SandboxState) {
	f.Update(orgID, func(o *Org) {
		s := o.sandboxes[sandboxID]
		s.sb.State = state
		s.sb.EndReason = &reason
	})
}

// File returns a guest file of a sandbox.
func (f *Fake) File(orgID, sandboxID, path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.orgs[orgID].sandboxes[sandboxID].files[path]
	return append([]byte(nil), b...), ok
}

// Files returns copies of every guest file of a sandbox by path.
func (f *Fake) Files(orgID, sandboxID string) map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for p, b := range f.orgs[orgID].sandboxes[sandboxID].files {
		out[p] = append([]byte(nil), b...)
	}
	return out
}

// SetFile writes a guest file of a sandbox.
func (f *Fake) SetFile(orgID, sandboxID, path string, data []byte) {
	f.Update(orgID, func(o *Org) { o.sandboxes[sandboxID].files[path] = append([]byte(nil), data...) })
}

// New implements ownsandclient.Factory.
func (f *Fake) New(ownsandOrganizationID, apiKey string) (ownsandclient.Client, error) {
	return &client{f: f, org: ownsandOrganizationID, key: apiKey}, nil
}

type client struct {
	f   *Fake
	org string
	key string
}

// begin checks the key like ownsand-api: an unknown key is unauthenticated,
// and a key of another organization gets not_found for this organization.
// It returns the organization with the fake lock held.
func (c *client) begin(rpc string) (*Org, error) {
	c.f.mu.Lock()
	c.f.calls[rpc]++
	for _, o := range c.f.orgs {
		if o.APIKey != "" && o.APIKey == c.key {
			if o.ID != c.org {
				c.f.mu.Unlock()
				return nil, connect.NewWireError(connect.CodeNotFound, errors.New("organization not found"))
			}
			return o, nil
		}
	}
	c.f.mu.Unlock()
	return nil, connect.NewWireError(connect.CodeUnauthenticated, errors.New("invalid API key"))
}

func (c *client) ListDevices(ctx context.Context) ([]*ownsandv1.Device, error) {
	o, err := c.begin("ListDevices")
	if err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	out := make([]*ownsandv1.Device, 0, len(o.Devices))
	for _, d := range o.Devices {
		out = append(out, proto.Clone(d).(*ownsandv1.Device))
	}
	return out, nil
}

func (c *client) TemplateGuestOS(ctx context.Context, templateID string) (ownsandv1.GuestOs, error) {
	o, err := c.begin("ListTemplates")
	if err != nil {
		return 0, err
	}
	defer c.f.mu.Unlock()
	g, ok := o.Templates[templateID]
	if !ok {
		return 0, connect.NewWireError(connect.CodeNotFound, errors.New("template not found"))
	}
	return g, nil
}

// SetCreateHook sets the hook that runs before each CreateSandbox call; nil
// removes it.
func (f *Fake) SetCreateHook(fn func(ctx context.Context) error) {
	if fn == nil {
		f.SetCreateRequestHook(nil)
		return
	}
	f.SetCreateRequestHook(func(ctx context.Context, _ *ownsandv1.CreateSandboxRequest) error { return fn(ctx) })
}

// SetCreateRequestHook sets a hook that can inspect each CreateSandbox request.
// A nil hook removes it.
func (f *Fake) SetCreateRequestHook(fn func(context.Context, *ownsandv1.CreateSandboxRequest) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createHook = fn
}

func (c *client) CreateSandbox(ctx context.Context, req *ownsandv1.CreateSandboxRequest) (*ownsandv1.Sandbox, error) {
	c.f.mu.Lock()
	hook := c.f.createHook
	c.f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, req); err != nil {
			return nil, err
		}
	}
	o, err := c.begin("CreateSandbox")
	if err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	if req.GetIdempotencyKey() == "" || req.GetPlacement().GetDeviceId() != "" {
		return nil, connect.NewWireError(connect.CodeInvalidArgument, errors.New("tide sends an idempotency key and no device ID"))
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	if id, ok := o.keys[req.GetIdempotencyKey()]; ok {
		if !bytes.Equal(o.requests[req.GetIdempotencyKey()], encoded) {
			return nil, connect.NewWireError(connect.CodeAlreadyExists, errors.New("the idempotency_key was used with other parameters"))
		}
		if o.CreateResponseError != nil {
			return nil, o.CreateResponseError
		}
		return proto.Clone(o.sandboxes[id].sb).(*ownsandv1.Sandbox), nil
	}
	if o.CreateError != nil {
		return nil, o.CreateError
	}
	if o.MaxSandboxes > 0 && o.active() >= o.MaxSandboxes {
		return nil, connect.NewWireError(connect.CodeUnavailable, errors.New("no device has free capacity"))
	}
	id, _ := uuidv7.NewString()
	state := ownsandv1.SandboxState_SANDBOX_STATE_CREATING
	if o.StartRunning {
		state = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	}
	sb := &ownsandv1.Sandbox{
		SandboxId: id, OrganizationId: o.ID, DeviceId: "device-" + strconv.Itoa(len(o.sandboxes)%2), State: state,
		Source: req.GetSource(), Resources: req.GetResources(), Labels: req.GetLabels(), IdleTtl: req.GetIdleTtl(), MaxTtl: req.GetMaxTtl(),
	}
	o.sandboxes[id] = &fakeSandbox{sb: sb, files: map[string][]byte{}, writes: map[string]*fakeWrite{}, reads: map[string][]byte{}}
	o.keys[req.GetIdempotencyKey()] = id
	o.requests[req.GetIdempotencyKey()] = encoded
	if o.CreateResponseError != nil {
		return nil, o.CreateResponseError
	}
	return proto.Clone(sb).(*ownsandv1.Sandbox), nil
}

func (c *client) sandbox(rpc, id string) (*Org, *fakeSandbox, error) {
	o, err := c.begin(rpc)
	if err != nil {
		return nil, nil, err
	}
	s, ok := o.sandboxes[id]
	if !ok {
		c.f.mu.Unlock()
		return nil, nil, connect.NewWireError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	return o, s, nil
}

func (c *client) GetSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	_, s, err := c.sandbox("GetSandbox", sandboxID)
	if err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	return proto.Clone(s.sb).(*ownsandv1.Sandbox), nil
}

// SetDeleteHook sets the hook that runs before each DeleteSandbox call. The
// hook runs without the fake lock. Passing nil removes it.
func (f *Fake) SetDeleteHook(fn func(ctx context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteHook = fn
}

func (c *client) DeleteSandbox(ctx context.Context, sandboxID string) (*ownsandv1.Sandbox, error) {
	c.f.mu.Lock()
	hook := c.f.deleteHook
	c.f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	o, s, err := c.sandbox("DeleteSandbox", sandboxID)
	if err != nil {
		return nil, err
	}
	defer c.f.mu.Unlock()
	if o.DeleteError != nil {
		return nil, o.DeleteError
	}
	if !ownsand.IsTerminal(s.sb.GetState()) {
		reason := ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
		s.sb.State, s.sb.EndReason = ownsandv1.SandboxState_SANDBOX_STATE_DELETED, &reason
	}
	return proto.Clone(s.sb).(*ownsandv1.Sandbox), nil
}

// running checks that a sandbox can serve a data path operation.
func (c *client) running(rpc, id string) error {
	_, s, err := c.sandbox(rpc, id)
	if err != nil {
		return err
	}
	defer c.f.mu.Unlock()
	if s.sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return connect.NewWireError(connect.CodeFailedPrecondition, errors.New("sandbox is not running"))
	}
	return nil
}

func (c *client) Exec(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
	if err := c.running("Exec", sandboxID); err != nil {
		return nil, err
	}
	if fn := c.f.execFunc(); fn != nil {
		return fn(ctx, sandboxID, p)
	}
	code := int64(0)
	return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
}

func (c *client) AttachExec(ctx context.Context, sandboxID, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
	if err := c.running("AttachExec", sandboxID); err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	fn := c.f.Attach
	c.f.mu.Unlock()
	if fn != nil {
		return fn(ctx, sandboxID, execID, p)
	}
	return nil, connect.NewWireError(connect.CodeNotFound, errors.New("exec not found"))
}

func (f *Fake) execFunc() ExecFunc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Exec
}

// SetExec replaces the exec functions.
func (f *Fake) SetExec(exec ExecFunc, attach AttachFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Exec, f.Attach = exec, attach
}

// readChunk is the chunk size of fake reads.
const readChunk = 1000

func (c *client) ReadFile(ctx context.Context, sandboxID string, p ownsand.FileReadParams, w io.Writer) (int64, error) {
	if err := c.running("ReadFile", sandboxID); err != nil {
		return 0, err
	}
	c.f.mu.Lock()
	org := c.f.orgs[c.org]
	if err := org.FileError; err != nil {
		c.f.mu.Unlock()
		return 0, err
	}
	s := org.sandboxes[sandboxID]
	snap, ok := s.reads[p.ReadID]
	if !ok {
		src, exists := s.files[p.Path]
		if !exists {
			c.f.mu.Unlock()
			return 0, connect.NewWireError(connect.CodeNotFound, errors.New("no such file"))
		}
		snap = append([]byte(nil), src...)
		s.reads[p.ReadID] = snap
	}
	onRead := org.OnFileRead
	c.f.mu.Unlock()
	if onRead != nil {
		onRead(ctx, p)
	}
	for off := p.Offset; off < int64(len(snap)); {
		if err := ctx.Err(); err != nil {
			return off, err
		}
		end := min(off+readChunk, int64(len(snap)))
		if _, err := w.Write(snap[off:end]); err != nil {
			return off, err
		}
		off = end
	}
	return int64(len(snap)), nil
}

func (c *client) OpenFileWrite(sandboxID string, p ownsand.FileWriteParams) (ownsandclient.FileWrite, error) {
	return &fileWrite{c: c, sandboxID: sandboxID, p: p}, nil
}

type fileWrite struct {
	c         *client
	sandboxID string
	p         ownsand.FileWriteParams
}

func (w *fileWrite) WriteChunk(ctx context.Context, offset int64, data []byte) (*ownsandv1.WriteFileResponse, error) {
	return w.send(offset, data, nil)
}

func (w *fileWrite) Finish(ctx context.Context, offset int64, data []byte, sizeBytes int64, mode uint32) (*ownsandv1.WriteFileResponse, error) {
	return w.send(offset, data, &sizeBytes)
}

func (w *fileWrite) send(offset int64, data []byte, size *int64) (*ownsandv1.WriteFileResponse, error) {
	if err := w.c.running("WriteFile", w.sandboxID); err != nil {
		return nil, err
	}
	w.c.f.mu.Lock()
	defer w.c.f.mu.Unlock()
	org := w.c.f.orgs[w.c.org]
	if err := org.FileError; err != nil {
		return nil, err
	}
	s := org.sandboxes[w.sandboxID]
	fw := s.writes[w.p.WriteID]
	if fw == nil {
		fw = &fakeWrite{path: w.p.Path, ranges: map[int64][]byte{}}
		s.writes[w.p.WriteID] = fw
	}
	if old, ok := fw.ranges[offset]; ok && !bytes.Equal(old, data) {
		return nil, connect.NewWireError(connect.CodeFailedPrecondition, errors.New("chunk overlaps written bytes with different content"))
	}
	fw.ranges[offset] = append([]byte(nil), data...)
	res := &ownsandv1.WriteFileResponse{WriteId: w.p.WriteID, Offset: offset, Length: int64(len(data))}
	if size != nil {
		var buf bytes.Buffer
		offsets := make([]int64, 0, len(fw.ranges))
		for off := range fw.ranges {
			offsets = append(offsets, off)
		}
		sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
		for _, off := range offsets {
			if off != int64(buf.Len()) {
				return nil, connect.NewWireError(connect.CodeFailedPrecondition, errors.New("the written ranges do not cover the file"))
			}
			buf.Write(fw.ranges[off])
		}
		if int64(buf.Len()) != *size {
			return nil, connect.NewWireError(connect.CodeFailedPrecondition, errors.New("the written ranges do not cover the size"))
		}
		s.files[fw.path] = buf.Bytes()
		res.Finished, res.SizeBytes = true, *size
		delete(s.writes, w.p.WriteID)
	}
	return res, nil
}

// SlowExec returns an ExecFunc that sends one stdout chunk every interval
// until ctx ends, for stream rotation tests. Each call starts at the given
// offset of a counter per exec ID.
func SlowExec(interval time.Duration) ExecFunc {
	return func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		t := time.NewTicker(interval)
		defer t.Stop()
		var off int64
		if p.OnEvent != nil {
			if err := p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: p.ExecID}); err != nil {
				return nil, err
			}
		}
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-t.C:
				if p.OnEvent != nil {
					if err := p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStdout, ExecID: p.ExecID, Offset: off, Data: []byte("x")}); err != nil {
						return nil, err
					}
				}
				off++
			}
		}
	}
}
