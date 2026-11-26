// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// SetSessionState changes the state of a dev session.
func (s *Server) SetSessionState(id string, state sandrunv1.RequestState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.sessions {
		if d.GetDevSessionId() == id {
			d.State = state
		}
	}
}

// Sessions returns copies of the dev sessions.
func (s *Server) Sessions() []*sandrunv1.DevSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sandrunv1.DevSession, 0, len(s.sessions))
	for _, d := range s.sessions {
		out = append(out, proto.CloneOf(d))
	}
	return out
}

// ExecRequests returns the exec requests that the server received, in
// order. Tests use it to check that values (for example environment values
// of the MCP server) reached the guest.
func (s *Server) ExecRequests() []*sandrunv1.ExecDevSessionRequest {
	s.guest.mu.Lock()
	defer s.guest.mu.Unlock()
	return slices.Clone(s.guest.requests)
}

// ReadRequests returns the ReadDevSessionFile requests that the server
// received, in order.
func (s *Server) ReadRequests() []*sandrunv1.ReadDevSessionFileRequest {
	s.guest.mu.Lock()
	defer s.guest.mu.Unlock()
	return slices.Clone(s.guest.reads)
}

type devSessionService struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	s *Server
}

func (d *devSessionService) ListDevSessionProfiles(_ context.Context, req *connect.Request[sandrunv1.ListDevSessionProfilesRequest]) (*connect.Response[sandrunv1.ListDevSessionProfilesResponse], error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireOrg(req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	var mine []*sandrunv1.DevSessionProfile
	for _, p := range s.profiles {
		if p.GetOrganizationId() == req.Msg.GetOrganizationId() && p.GetWorkloadKind() == sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION {
			mine = append(mine, &sandrunv1.DevSessionProfile{
				RunnerProfileId: p.GetRunnerProfileId(), Name: p.GetName(), GuestOs: p.GetGuestOs(),
				Resources: p.GetResources(), WorkspacePath: p.GetWorkspacePath(),
				IdleTtl: p.GetIdleTtl(), MaxTtl: p.GetMaxTtl(), MaxQueueWait: p.GetMaxQueueWait(),
			})
		}
	}
	items, next, err := page(mine, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListDevSessionProfilesResponse{NextPageToken: next}
	for _, it := range items {
		res.Profiles = append(res.Profiles, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (d *devSessionService) CreateDevSession(_ context.Context, req *connect.Request[sandrunv1.CreateDevSessionRequest]) (*connect.Response[sandrunv1.CreateDevSessionResponse], error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	org, err := s.requireOrg(req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if len(req.Msg.GetLabel()) > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("label has more than 100 characters"))
	}
	var prof *sandrunv1.RunnerProfile
	for _, p := range s.profiles {
		if p.GetOrganizationId() == org.GetOrganizationId() && p.GetRunnerProfileId() == req.Msg.GetRunnerProfileId() && p.GetWorkloadKind() == sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION {
			prof = p
		}
	}
	if prof == nil {
		return nil, notFound("runner profile")
	}
	if org.GetSettings().MaxDevSessionsPerUser != nil {
		active := 0
		for _, x := range s.sessions {
			if x.GetOrganizationId() == org.GetOrganizationId() && x.GetOwnerUserId() == s.UserID && !terminalRequest(x.GetState()) {
				active++
			}
		}
		if active >= int(org.GetSettings().GetMaxDevSessionsPerUser()) {
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("the per-user dev session limit is reached"))
		}
	}
	state := s.NewSessionState
	if state == sandrunv1.RequestState_REQUEST_STATE_UNSPECIFIED {
		state = sandrunv1.RequestState_REQUEST_STATE_RUNNING
	}
	ds := &sandrunv1.DevSession{
		DevSessionId: s.newID(), OrganizationId: org.GetOrganizationId(), RunnerProfileId: prof.GetRunnerProfileId(),
		OwnerUserId: s.UserID, Label: req.Msg.GetLabel(), GuestOs: prof.GetGuestOs(), WorkspacePath: prof.GetWorkspacePath(),
		State: state, QueuedAt: timestamppb.Now(), RunnerProfileName: prof.GetName(),
	}
	if state == sandrunv1.RequestState_REQUEST_STATE_RUNNING {
		ds.StartedAt = timestamppb.Now()
		ds.RunnerId = s.newID()
	}
	s.sessions = append(s.sessions, ds)
	return connect.NewResponse(&sandrunv1.CreateDevSessionResponse{DevSession: proto.CloneOf(ds)}), nil
}

// session returns the dev session of the request. The caller holds s.mu.
func (s *Server) session(org, id string) (*sandrunv1.DevSession, error) {
	for _, x := range s.sessions {
		if x.GetOrganizationId() == org && x.GetDevSessionId() == id {
			return x, nil
		}
	}
	return nil, notFound("dev session")
}

// runningSession returns a copy of a RUNNING dev session.
func (s *Server) runningSession(org, id string) (*sandrunv1.DevSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.session(org, id)
	if err != nil {
		return nil, err
	}
	if x.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the dev session is not running"))
	}
	return proto.CloneOf(x), nil
}

func (d *devSessionService) GetDevSession(_ context.Context, req *connect.Request[sandrunv1.GetDevSessionRequest]) (*connect.Response[sandrunv1.GetDevSessionResponse], error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.session(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId())
	if err != nil {
		return nil, err
	}
	s.sessionGets[x.GetDevSessionId()]++
	if s.PromoteAfterGets > 0 && s.sessionGets[x.GetDevSessionId()] >= s.PromoteAfterGets &&
		(x.GetState() == sandrunv1.RequestState_REQUEST_STATE_QUEUED || x.GetState() == sandrunv1.RequestState_REQUEST_STATE_STARTING) {
		x.State = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		x.StartedAt = timestamppb.Now()
	}
	return connect.NewResponse(&sandrunv1.GetDevSessionResponse{DevSession: proto.CloneOf(x)}), nil
}

func (d *devSessionService) ListDevSessions(_ context.Context, req *connect.Request[sandrunv1.ListDevSessionsRequest]) (*connect.Response[sandrunv1.ListDevSessionsResponse], error) {
	s := d.s
	m := req.Msg
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.requireOrg(m.GetOrganizationId()); err != nil {
		return nil, err
	}
	profileID, err := normalizeListProfileID(m.GetRunnerProfileId())
	if err != nil {
		return nil, err
	}
	states := listStates(m.GetStates(), openRequestStates, m.GetIncludeEnded())
	var mine []*sandrunv1.DevSession
	for _, x := range s.sessions {
		if x.GetOrganizationId() != m.GetOrganizationId() || (profileID != "" && x.GetRunnerProfileId() != profileID) {
			continue
		}
		if m.GetOwnedByCaller() && x.GetOwnerUserId() != s.UserID {
			continue
		}
		if o := m.GetOwnerUserId(); o != "" && x.GetOwnerUserId() != o {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, x.GetState()) {
			continue
		}
		mine = append(mine, x)
	}
	items, next, err := page(mine, m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &sandrunv1.ListDevSessionsResponse{NextPageToken: next}
	for _, it := range items {
		res.DevSessions = append(res.DevSessions, proto.CloneOf(it))
	}
	return connect.NewResponse(res), nil
}

func (d *devSessionService) DeleteDevSession(_ context.Context, req *connect.Request[sandrunv1.DeleteDevSessionRequest]) (*connect.Response[sandrunv1.DeleteDevSessionResponse], error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	x, err := s.session(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId())
	if err != nil {
		return nil, err
	}
	switch {
	case terminalRequest(x.GetState()):
	case x.GetState() == sandrunv1.RequestState_REQUEST_STATE_QUEUED:
		x.State = sandrunv1.RequestState_REQUEST_STATE_CANCELED
		x.EndReason = sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER
		x.EndedAt = timestamppb.Now()
	default:
		x.State = sandrunv1.RequestState_REQUEST_STATE_ENDED
		x.EndReason = sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED
		x.EndedAt = timestamppb.Now()
	}
	return connect.NewResponse(&sandrunv1.DeleteDevSessionResponse{DevSession: proto.CloneOf(x)}), nil
}

func (d *devSessionService) ExecDevSession(ctx context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	ds, err := d.s.runningSession(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId())
	if err != nil {
		return err
	}
	if len(req.Msg.GetCommand()) == 0 || req.Msg.GetExecId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("exec_id and command are required"))
	}
	if d.s.BeforeExec != nil {
		d.s.BeforeExec(proto.CloneOf(req.Msg))
	}
	p, err := d.s.guest.start(ds, req.Msg)
	if err != nil {
		return err
	}
	return d.s.guest.serve(ctx, p, 0, 0, func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.ExecDevSessionResponse{Event: ev})
	})
}

func (d *devSessionService) AttachDevSessionExec(ctx context.Context, req *connect.Request[sandrunv1.AttachDevSessionExecRequest], stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
	if _, err := d.s.runningSession(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId()); err != nil {
		return err
	}
	d.s.guest.mu.Lock()
	p := d.s.guest.execs[req.Msg.GetExecId()]
	d.s.guest.mu.Unlock()
	if p == nil || p.session != req.Msg.GetDevSessionId() {
		return notFound("exec")
	}
	return d.s.guest.serve(ctx, p, req.Msg.GetStdoutOffset(), req.Msg.GetStderrOffset(), func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.AttachDevSessionExecResponse{Event: ev})
	})
}

// guestPath resolves a request path against the workspace path.
func guestPath(ds *sandrunv1.DevSession, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(ds.GetWorkspacePath(), p)
}

func (d *devSessionService) ReadDevSessionFile(ctx context.Context, req *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
	ds, err := d.s.runningSession(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId())
	if err != nil {
		return err
	}
	if req.Msg.GetReadId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("read_id is required"))
	}
	data, err := d.s.guest.readSnapshot(ds, req.Msg)
	if err != nil {
		return err
	}
	off := req.Msg.GetOffset()
	if off < 0 || off > int64(len(data)) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the offset is outside the file"))
	}
	sent := 0
	for off < int64(len(data)) {
		if d.s.ReadStreamChunkLimit > 0 && sent >= d.s.ReadStreamChunkLimit {
			// Like the stream limit of sandrun-api: an OK status without the
			// end message.
			return nil
		}
		n := min(int64(len(data))-off, 1<<20)
		if err := stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_Chunk{Chunk: &sandrunv1.DevSessionFileChunk{Offset: off, Data: slices.Clone(data[off : off+n])}}}); err != nil {
			return err
		}
		off += n
		sent++
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_End{End: &sandrunv1.DevSessionFileEnd{SizeBytes: off}}})
}

func (g *guest) readSnapshot(ds *sandrunv1.DevSession, req *sandrunv1.ReadDevSessionFileRequest) ([]byte, error) {
	id, path := req.GetReadId(), guestPath(ds, req.GetPath())
	orgID, sessionID := ds.GetOrganizationId(), ds.GetDevSessionId()
	g.mu.Lock()
	g.reads = append(g.reads, proto.CloneOf(req))
	snapshot, ok := g.snapshots[id]
	g.mu.Unlock()
	if !ok {
		data, err := readSnapshotFile(path)

		// Another request may have bound this ID during the file read.
		g.mu.Lock()
		defer g.mu.Unlock()
		snapshot, ok = g.snapshots[id]
		if !ok {
			if err == nil {
				g.snapshots[id] = readSnapshot{organizationID: orgID, devSessionID: sessionID, path: path, data: data}
			}
			return data, err
		}
	}
	if snapshot.organizationID != orgID || snapshot.devSessionID != sessionID {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("read_id is already bound to another dev session"))
	}
	if snapshot.path != path {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("read_id is already bound to a different path"))
	}
	return snapshot.data, nil
}

func readSnapshotFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound("file")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if info.IsDir() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("not a regular file"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return data, nil
}

func (d *devSessionService) WriteDevSessionFile(_ context.Context, req *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
	ds, err := d.s.runningSession(req.Msg.GetOrganizationId(), req.Msg.GetDevSessionId())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	chunkSize := int64(len(m.GetData()))
	if m.GetWriteId() == "" || chunkSize > 1<<20 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("write_id is required and a chunk holds at most 1 MiB"))
	}
	if m.GetOffset() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid offset: must not be negative"))
	}
	if m.GetOffset() > math.MaxInt64-chunkSize {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid offset: the chunk end offset exceeds the maximum int64 value"))
	}
	key := writeKey{
		organizationID: ds.GetOrganizationId(),
		devSessionID:   ds.GetDevSessionId(),
		writeID:        m.GetWriteId(),
	}
	path := guestPath(ds, m.GetPath())
	g := d.s.guest
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.writes[key]
	if w == nil {
		w = &pendingWrite{path: path}
		g.writes[key] = w
	} else if w.path != path {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("every chunk of a write_id must name the same path"))
	}
	if w.done {
		return connect.NewResponse(&sandrunv1.WriteDevSessionFileResponse{WriteId: m.GetWriteId(), Offset: m.GetOffset(), Length: chunkSize, Finished: true, SizeBytes: int64(len(w.data))}), nil
	}
	end := m.GetOffset() + chunkSize
	switch {
	case m.GetOffset() == int64(len(w.data)):
		w.data = append(w.data, m.GetData()...)
	case end <= int64(len(w.data)) && bytes.Equal(w.data[m.GetOffset():end], m.GetData()):
		// A retried chunk.
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("chunk at offset %d does not continue the write at %d", m.GetOffset(), len(w.data)))
	}
	res := &sandrunv1.WriteDevSessionFileResponse{WriteId: m.GetWriteId(), Offset: m.GetOffset(), Length: chunkSize}
	if m.GetFinal() {
		if end != int64(len(w.data)) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the final chunk does not end the file"))
		}
		mode := fs.FileMode(m.GetMode()).Perm()
		if mode == 0 {
			mode = 0o644
		}
		tmp := w.path + ".sandruntest-write"
		if err := os.WriteFile(tmp, w.data, mode); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		_ = os.Chmod(tmp, mode)
		if err := os.Rename(tmp, w.path); err != nil {
			_ = os.Remove(tmp)
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		w.done = true
		res.Finished = true
		res.SizeBytes = int64(len(w.data))
	}
	return connect.NewResponse(res), nil
}

// The fake guest.

type writeKey struct {
	organizationID string
	devSessionID   string
	writeID        string
}

type pendingWrite struct {
	path string
	data []byte
	done bool
}

type readSnapshot struct {
	organizationID string
	devSessionID   string
	path           string
	data           []byte
}

type guest struct {
	s        *Server
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	execs    map[string]*proc
	writes   map[writeKey]*pendingWrite
	requests []*sandrunv1.ExecDevSessionRequest
	// snapshots holds the first snapshot bound to each read_id.
	snapshots map[string]readSnapshot
	reads     []*sandrunv1.ReadDevSessionFileRequest
	wg        sync.WaitGroup
}

func newGuest(s *Server) *guest {
	ctx, cancel := context.WithCancel(context.Background())
	return &guest{s: s, ctx: ctx, cancel: cancel, execs: map[string]*proc{}, writes: map[writeKey]*pendingWrite{}, snapshots: map[string]readSnapshot{}}
}

func (g *guest) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.cancel()
	g.wg.Wait()
}

// proc is one exec. Its output and result outlive every stream.
type proc struct {
	session string
	execID  string
	mu      sync.Mutex
	changed chan struct{}
	start   time.Time
	out     [2]outBuf // stdout, stderr
	final   *sandrunv1.DevSessionExecEvent
	keep    int
}

type outBuf struct {
	base int64 // stream offset of data[0]
	data []byte
}

func (p *proc) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *proc) append(i int, b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o := &p.out[i]
	o.data = append(o.data, b...)
	if p.keep > 0 && len(o.data) > p.keep {
		drop := len(o.data) - p.keep
		o.data = slices.Clone(o.data[drop:])
		o.base += int64(drop)
	}
	p.notifyLocked()
}

func (p *proc) finish(ev *sandrunv1.DevSessionExecEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.final = ev
	p.notifyLocked()
}

// start starts a local process for the exec request.
func (g *guest) start(ds *sandrunv1.DevSession, req *sandrunv1.ExecDevSessionRequest) (*proc, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the fake guest is closed"))
	}
	if g.execs[req.GetExecId()] != nil {
		g.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("the exec_id is in use"))
	}
	p := &proc{session: ds.GetDevSessionId(), execID: req.GetExecId(), changed: make(chan struct{}), start: time.Now(), keep: g.s.OutputKeepBytes}
	g.execs[req.GetExecId()] = p
	g.requests = append(g.requests, proto.CloneOf(req))
	// Register the exec before close can stop admission and wait.
	g.wg.Add(1)
	g.mu.Unlock()

	dir := ds.GetWorkspacePath()
	if wd := req.GetWorkingDirectory(); wd != "" {
		dir = guestPath(ds, wd)
	}
	failed := func(reason sandrunv1.ExecFailureReason, msg string) (*proc, error) {
		p.finish(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Failed{Failed: &sandrunv1.DevSessionExecFailed{Reason: reason, Message: msg}}})
		g.wg.Done()
		return p, nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return failed(sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_WORKING_DIRECTORY_INVALID, "the working directory does not exist")
	}
	ctx := g.ctx
	var cancel context.CancelFunc = func() {}
	if t := req.GetTimeout(); t != nil {
		ctx, cancel = context.WithTimeout(ctx, t.AsDuration())
	}
	cmd := newExecCommand(ctx, req.GetCommand(), req.GetEnv())
	cmd.Dir = dir
	cmd.Stdout = writerFunc(func(b []byte) (int, error) { p.append(0, b); return len(b), nil })
	cmd.Stderr = writerFunc(func(b []byte) (int, error) { p.append(1, b); return len(b), nil })
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		switch {
		case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
			return failed(sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND, "the program does not exist")
		case errors.Is(err, fs.ErrPermission):
			return failed(sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED, "the program cannot run")
		}
		return failed(sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_OTHER, err.Error())
	}
	go func() {
		defer g.wg.Done()
		defer cancel()
		_ = cmd.Wait()
		timedOut := ctx.Err() != nil && g.ctx.Err() == nil
		ex := &sandrunv1.DevSessionExecExited{TimedOut: timedOut, EndTime: timestamppb.Now()}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			ex.Status = &sandrunv1.DevSessionExecExited_Signal{Signal: int32(ws.Signal())}
		} else {
			ex.Status = &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: int64(cmd.ProcessState.ExitCode())}
		}
		p.finish(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{Exited: ex}})
	}()
	return p, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// chunkLimit bounds one output event of the fake.
const chunkLimit = 32 << 10

// serve sends the events of p from the given offsets until the final
// event, the end of ctx, or the stream event limit.
func (g *guest) serve(ctx context.Context, p *proc, so, se int64, send func(*sandrunv1.DevSessionExecEvent) error) error {
	limit := g.s.ExecStreamEventLimit
	sent := 0
	sendEvent := func(ev *sandrunv1.DevSessionExecEvent) (stop bool, err error) {
		if err := send(ev); err != nil {
			return true, err
		}
		sent++
		return limit > 0 && sent >= limit, nil
	}
	if stop, err := sendEvent(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Started{Started: &sandrunv1.DevSessionExecStarted{ExecId: p.execID, StartTime: timestamppb.New(p.start)}}}); stop {
		return err
	}
	offs := [2]int64{so, se}
	p.mu.Lock()
	if offs[0] < p.out[0].base || offs[1] < p.out[1].base {
		offs[0] = max(offs[0], p.out[0].base)
		offs[1] = max(offs[1], p.out[1].base)
		p.mu.Unlock()
		if stop, err := sendEvent(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: offs[0], StderrOffset: offs[1]}}}); stop {
			return err
		}
	} else {
		p.mu.Unlock()
	}
	for {
		p.mu.Lock()
		var ev *sandrunv1.DevSessionExecEvent
		for i := range 2 {
			o := &p.out[i]
			if offs[i] < o.base {
				// The kept output moved past this stream.
				offs[0] = max(offs[0], p.out[0].base)
				offs[1] = max(offs[1], p.out[1].base)
				ev = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: offs[0], StderrOffset: offs[1]}}}
				break
			}
			end := o.base + int64(len(o.data))
			if offs[i] < end {
				from := offs[i] - o.base
				to := min(int64(len(o.data)), from+chunkLimit)
				chunk := &sandrunv1.DevSessionExecOutput{Offset: offs[i], Data: slices.Clone(o.data[from:to])}
				offs[i] += to - from
				if i == 0 {
					ev = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{Stdout: chunk}}
				} else {
					ev = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stderr{Stderr: chunk}}
				}
				break
			}
		}
		final := p.final
		wait := p.changed
		p.mu.Unlock()
		switch {
		case ev != nil:
		case final != nil:
			_, err := sendEvent(final)
			return err
		default:
			select {
			case <-ctx.Done():
				return nil
			case <-wait:
				continue
			}
		}
		if stop, err := sendEvent(ev); stop {
			return err
		}
	}
}
