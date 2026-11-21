// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeVM struct {
	id     string
	exited chan struct{}
	once   sync.Once
}

func (v *fakeVM) SandboxID() string { return v.id }
func (v *fakeVM) Dial(context.Context) (net.Conn, error) {
	return nil, errors.New("fake VM has no guest")
}
func (v *fakeVM) RestorePath() ownsandv1.SandboxRestorePath {
	return ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
}
func (v *fakeVM) Exited() <-chan struct{} { return v.exited }
func (v *fakeVM) stop()                   { v.once.Do(func() { close(v.exited) }) }

type fakeRuntime struct {
	mu        sync.Mutex
	vms       map[string]*fakeVM
	paused    map[string]bool
	createErr error
	block     chan struct{}
	// snapStarted and snapBlock, when set, make Snapshot signal that the
	// capture started and wait until snapBlock is closed.
	snapStarted chan struct{}
	snapBlock   chan struct{}
	// deleteFails is the number of Delete calls that fail before one works.
	deleteFails int
	deleteCalls int
	// pauseErr and resumeErr, when set, make Pause and Resume fail.
	pauseErr  error
	resumeErr error
	// onCreate, when set, runs in Create after the VM started.
	onCreate func(spec vmruntime.Spec)
	// files holds the sandbox IDs whose runtime files exist. Create adds an
	// ID, a successful Delete removes it, and a test adds an ID to model the
	// files that a lost VM leaves behind.
	files  map[string]bool
	starts int // Create calls, counted before any block or error
}

func (r *fakeRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{vms: map[string]*fakeVM{}, paused: map[string]bool{}, files: map[string]bool{}}
}

func (r *fakeRuntime) Kind() config.RuntimeKind { return config.RuntimeKindProcess }
func (r *fakeRuntime) Capabilities(context.Context) vmruntime.Capabilities {
	return vmruntime.Capabilities{}
}
func (r *fakeRuntime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.createErr != nil {
		return nil, r.createErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &fakeVM{id: spec.SandboxID, exited: make(chan struct{})}
	r.vms[spec.SandboxID] = v
	if r.onCreate != nil {
		r.onCreate(spec)
	}
	r.files[spec.SandboxID] = true
	return v, nil
}
func (r *fakeRuntime) Pause(_ context.Context, vm vmruntime.VM) error {
	if r.pauseErr != nil {
		return r.pauseErr
	}
	r.mu.Lock()
	r.paused[vm.SandboxID()] = true
	r.mu.Unlock()
	return nil
}
func (r *fakeRuntime) Resume(_ context.Context, vm vmruntime.VM) error {
	if r.resumeErr != nil {
		return r.resumeErr
	}
	r.mu.Lock()
	r.paused[vm.SandboxID()] = false
	r.mu.Unlock()
	return nil
}
func (r *fakeRuntime) Snapshot(_ context.Context, _ vmruntime.VM, dir string) (vmruntime.SnapshotInfo, error) {
	if r.snapStarted != nil {
		close(r.snapStarted)
		<-r.snapBlock
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "disk"), []byte("disk"), 0o600)
	return vmruntime.SnapshotInfo{SizeBytes: 4}, nil
}
func (r *fakeRuntime) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	r.deleteCalls++
	if r.deleteFails > 0 {
		r.deleteFails--
		r.mu.Unlock()
		return errors.New("injected stop failure")
	}
	v := r.vms[id]
	delete(r.vms, id)
	delete(r.files, id)
	r.mu.Unlock()
	if v != nil {
		v.stop()
	}
	return nil
}

func (r *fakeRuntime) hasFiles(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files[id]
}
func (r *fakeRuntime) Running(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.vms {
		ids = append(ids, id)
	}
	return ids, nil
}
func (r *fakeRuntime) Close() error { return nil }

type outbox struct {
	mu      sync.Mutex
	entries []*ownsandv1.SandboxStateEntry
}

func (o *outbox) SendSandboxUpdate(e *ownsandv1.SandboxStateEntry) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.entries = append(o.entries, e)
	return true
}

func (o *outbox) states(id string) []ownsandv1.SandboxState {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []ownsandv1.SandboxState
	for _, e := range o.entries {
		if e.GetSandboxId() == id && (len(out) == 0 || out[len(out)-1] != e.GetState()) {
			out = append(out, e.GetState())
		}
	}
	return out
}

type execs struct {
	mu      sync.Mutex
	running map[string]bool
	ended   map[string]ownsandv1.SandboxEndReason
}

func (e *execs) HasRunningExec(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}
func (e *execs) SandboxEnded(id string, r ownsandv1.SandboxEndReason) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended[id] = r
}

type env struct {
	t   *testing.T
	st  *store.Store
	rt  *fakeRuntime
	clk *clock
	m   *Manager
	out *outbox
	ex  *execs
}

func newEnv(t *testing.T) *env {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, rt: newFakeRuntime(), clk: &clock{now: time.Now()}, out: &outbox{}, ex: &execs{running: map[string]bool{}, ended: map[string]ownsandv1.SandboxEndReason{}}}
	e.restart()
	// Registered after t.TempDir, so it runs before the state directory is
	// removed: Close waits for the background ends that still write records.
	t.Cleanup(func() { e.m.Close() })
	return e
}

func (e *env) restart() {
	if e.m != nil {
		e.m.Close()
	}
	m, err := New(context.Background(), Options{Store: e.st, Runtime: e.rt, Now: e.clk.Now, FreeDisk: func() (int64, error) { return 1 << 50, nil }})
	if err != nil {
		e.t.Fatal(err)
	}
	m.SetOutbox(e.out)
	m.SetExecTracker(e.ex)
	e.m = m
}

var limits = &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20, VcpuOvercommitPercent: 100}

func (e *env) createCmd(vcpu int32, idle, max time.Duration) *ownsandv1.CreateSandboxCommand {
	id, _ := uuidv7.NewString()
	return &ownsandv1.CreateSandboxCommand{
		SandboxId:   id,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources:   &ownsandv1.Resources{VcpuCount: vcpu, MemoryMib: 512, DiskGib: 1},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs:     ownsandv1.GuestOs_GUEST_OS_LINUX,
		IdleTtl:     durationpb.New(idle),
		MaxTtl:      durationpb.New(max),
		CreateTime:  timestamppb.New(e.clk.Now()),
		MaxDeadline: timestamppb.New(e.clk.Now().Add(max)),
		Limits:      limits,
	}
}

func (e *env) create(idle, max time.Duration) *ownsandv1.SandboxStateEntry {
	e.t.Helper()
	entry, err := e.m.Create(context.Background(), e.createCmd(1, idle, max))
	if err != nil {
		e.t.Fatal(err)
	}
	return entry
}

func waitState(t *testing.T, m *Manager, id string, want ownsandv1.SandboxState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := m.State(id); s == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := m.State(id)
	t.Fatalf("sandbox state %v, want %v", s, want)
}

func endReason(m *Manager, id string) ownsandv1.SandboxEndReason {
	for _, e := range m.Entries() {
		if e.GetSandboxId() == id {
			return e.GetEndReason()
		}
	}
	return 0
}

func TestCreateDeleteLifecycle(t *testing.T) {
	e := newEnv(t)
	entry := e.create(time.Hour, 2*time.Hour)
	id := entry.GetSandboxId()
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || entry.GetIdleDeadline() == nil || entry.GetRevision().GetEpoch() != e.m.Epoch() {
		t.Fatalf("entry %v", entry)
	}
	// A repeated create returns the current state.
	again, err := e.m.Create(context.Background(), &ownsandv1.CreateSandboxCommand{SandboxId: id, Source: entry2src(), Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 1, DiskGib: 1}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX, IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(time.Hour), MaxDeadline: timestamppb.Now(), Limits: limits})
	if err != nil || again.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("repeated create %v %v", again, err)
	}
	del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
	if err != nil || del.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || del.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("delete %v %v", del, err)
	}
	want := []ownsandv1.SandboxState{
		ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		ownsandv1.SandboxState_SANDBOX_STATE_DELETING, ownsandv1.SandboxState_SANDBOX_STATE_DELETED,
	}
	if got := e.out.states(id); len(got) != 4 || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("updates %v", got)
	}
	if e.ex.ended[id] != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatal("execs not ended with the sandbox")
	}
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 {
		t.Fatal("VM still runs")
	}
	// Delete is idempotent.
	if again, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil || again.GetRevision().GetCounter() != del.GetRevision().GetCounter() {
		t.Fatalf("repeated delete %v %v", again, err)
	}
	// Revisions increase.
	var last uint64
	for _, en := range e.out.entries {
		if en.GetRevision().GetCounter() <= last {
			t.Fatal("revision did not increase")
		}
		last = en.GetRevision().GetCounter()
	}
}

func TestWithRunning(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	called := false
	if !e.m.WithRunning(id, func() { called = true }) || !called {
		t.Fatal("WithRunning skipped a running sandbox")
	}
	unknown, _ := uuidv7.NewString()
	if e.m.WithRunning(unknown, func() { t.Fatal("called for an unknown sandbox") }) {
		t.Fatal("WithRunning accepted an unknown sandbox")
	}
	if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	if e.m.WithRunning(id, func() { t.Fatal("called for a deleted sandbox") }) {
		t.Fatal("WithRunning accepted a deleted sandbox")
	}
}

func entry2src() *ownsandv1.CreateSandboxCommand_BaseImage {
	return &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}
}

func TestDeleteBeforeCreateRejectsCreate(t *testing.T) {
	e := newEnv(t)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: cmd.GetSandboxId()})
	if err != nil || del.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete %v %v", del, err)
	}
	if _, err := e.m.Create(context.Background(), cmd); err == nil {
		t.Fatal("create after delete accepted")
	}
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 {
		t.Fatal("VM created after delete")
	}
}

// blockingGate is a CreateGate whose BeginCreate waits until release is closed.
// entered receives one value per BeginCreate call that started waiting.
type blockingGate struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingGate() *blockingGate {
	return &blockingGate{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (g *blockingGate) BeginCreate() (func(), error) {
	g.entered <- struct{}{}
	<-g.release
	return func() {}, nil
}

func TestCreateGateRecheckAfterConcurrentDelete(t *testing.T) {
	e := newEnv(t)
	gate := newBlockingGate()
	e.m.SetCreateGate(gate)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	type result struct {
		entry *ownsandv1.SandboxStateEntry
		err   error
	}
	done := make(chan result, 1)
	go func() {
		entry, err := e.m.Create(context.Background(), cmd)
		done <- result{entry, err}
	}()
	<-gate.entered
	del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: cmd.GetSandboxId()})
	if err != nil || del.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete %v %v", del, err)
	}
	close(gate.release)
	res := <-done
	var ce *CommandError
	if !errors.As(res.err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("create after concurrent delete: %v", res.err)
	}
	if n := e.rt.startCount(); n != 0 {
		t.Fatalf("VM starts after delete = %d, want 0", n)
	}
	e.m.mu.Lock()
	got := e.m.sandboxes[cmd.GetSandboxId()].rec.State
	e.m.mu.Unlock()
	if got != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("record state %v, want DELETED", got)
	}
}

func TestCreateGateRecheckDuplicateCreateStartsOnce(t *testing.T) {
	e := newEnv(t)
	gate := newBlockingGate()
	e.m.SetCreateGate(gate)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	type result struct {
		entry *ownsandv1.SandboxStateEntry
		err   error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			entry, err := e.m.Create(context.Background(), cmd)
			done <- result{entry, err}
		}()
	}
	<-gate.entered
	<-gate.entered
	close(gate.release)
	for range 2 {
		res := <-done
		if res.err != nil || res.entry.GetSandboxId() != cmd.GetSandboxId() {
			t.Fatalf("create: %v %v", res.entry, res.err)
		}
	}
	if n := e.rt.startCount(); n != 1 {
		t.Fatalf("VM starts = %d, want 1", n)
	}
}

func TestCreateGateControlStartsOnce(t *testing.T) {
	e := newEnv(t)
	gate := newBlockingGate()
	close(gate.release)
	e.m.SetCreateGate(gate)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil || entry.GetSandboxId() != cmd.GetSandboxId() {
		t.Fatalf("create: %v %v", entry, err)
	}
	if n := e.rt.startCount(); n != 1 {
		t.Fatalf("VM starts = %d, want 1", n)
	}
}

func TestDeleteCancelsRunningCreate(t *testing.T) {
	e := newEnv(t)
	e.rt.block = make(chan struct{})
	cmd := e.createCmd(1, time.Hour, time.Hour)
	done := make(chan error, 1)
	go func() {
		_, err := e.m.Create(context.Background(), cmd)
		done <- err
	}()
	waitState(t, e.m, cmd.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_CREATING)
	del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: cmd.GetSandboxId()})
	if err != nil || del.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("delete %v %v", del, err)
	}
	if err := <-done; err == nil {
		t.Fatal("canceled create reported success")
	}
}

func TestCreateLimits(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		vcpu int32
		code ownsandv1.ErrorCode
	}{
		{5, ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION},
	} {
		_, err := e.m.Create(context.Background(), e.createCmd(c.vcpu, time.Hour, time.Hour))
		var ce *CommandError
		if !errors.As(err, &ce) || ce.Code != c.code {
			t.Fatalf("vcpu %d: %v", c.vcpu, err)
		}
	}
	for range 4 {
		if _, err := e.m.Create(context.Background(), e.createCmd(1, time.Hour, time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.m.Create(context.Background(), e.createCmd(1, time.Hour, time.Hour))
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("fifth vCPU: %v", err)
	}
	// Overcommit 200% doubles the vCPU limit, from the stricter limits.
	e.m.SetLimits(&ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20, VcpuOvercommitPercent: 200})
	cmd := e.createCmd(1, time.Hour, time.Hour)
	cmd.Limits = &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20, VcpuOvercommitPercent: 200}
	if _, err := e.m.Create(context.Background(), cmd); err != nil {
		t.Fatalf("overcommit: %v", err)
	}
}

func TestTTLExpiry(t *testing.T) {
	e := newEnv(t)
	idle := e.create(10*time.Minute, time.Hour).GetSandboxId()
	maxed := e.create(10*time.Minute, 20*time.Minute).GetSandboxId()
	busy := e.create(10*time.Minute, time.Hour).GetSandboxId()
	paused := e.create(10*time.Minute, time.Hour).GetSandboxId()
	if _, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: paused}); err != nil {
		t.Fatal(err)
	}
	e.ex.mu.Lock()
	e.ex.running[busy] = true
	e.ex.running[maxed] = true
	e.ex.mu.Unlock()
	e.clk.Advance(11 * time.Minute)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, idle, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, idle); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("idle reason %v", r)
	}
	for _, id := range []string{busy, paused, maxed} {
		if s, _ := e.m.State(id); s == ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("sandbox %s ended early", id)
		}
	}
	e.clk.Advance(10 * time.Minute)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, maxed, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, maxed); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("max reason %v", r)
	}
	// The paused sandbox's idle clock does not run; max TTL still applies.
	if s, _ := e.m.State(paused); s != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("paused state %v", s)
	}
	e.clk.Advance(time.Hour)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, paused, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	// A running exec does not hold the sandbox past its max TTL.
	waitState(t, e.m, busy, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, busy); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("busy reason %v", r)
	}
}

// TestTTLEndRetriesAfterStopFailure checks that a failed stop during TTL
// termination leaves the sandbox in DELETING and that a later Tick completes
// the end exactly once.
func TestTTLEndRetriesAfterStopFailure(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, time.Hour).GetSandboxId()
	e.rt.mu.Lock()
	e.rt.deleteFails = 1
	e.rt.deleteCalls = 0
	e.rt.mu.Unlock()
	e.clk.Advance(11 * time.Minute)
	e.m.Tick(context.Background(), false)
	waitFor(t, func() bool {
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		return e.rt.deleteCalls == 1
	})
	// The failed attempt must clear the ending flag so Tick can select the
	// sandbox again. It stays DELETING until the retry.
	waitFor(t, func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return !e.m.sandboxes[id].ending
	})
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("state after failed stop %v, want DELETING", s)
	}
	e.ex.mu.Lock()
	_, ended := e.ex.ended[id]
	e.ex.mu.Unlock()
	if ended {
		t.Fatal("exec tracker notified before the VM stopped")
	}
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("end reason %v", r)
	}
	e.rt.mu.Lock()
	calls := e.rt.deleteCalls
	e.rt.mu.Unlock()
	if calls != 2 {
		t.Fatalf("runtime Delete calls %d, want 2", calls)
	}
	e.ex.mu.Lock()
	r := e.ex.ended[id]
	e.ex.mu.Unlock()
	if r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("exec tracker end reason %v", r)
	}
	// A further Tick does nothing for the terminal record.
	e.m.Tick(context.Background(), false)
	time.Sleep(50 * time.Millisecond)
	e.rt.mu.Lock()
	calls = e.rt.deleteCalls
	e.rt.mu.Unlock()
	if calls != 2 {
		t.Fatalf("runtime Delete calls after terminal %d, want 2", calls)
	}
}

// TestMaxTTLReasonSurvivesRetryOfPausedSandbox checks that a retry after a
// failed stop keeps MAX_TTL_EXPIRED for a paused sandbox. The record in
// DELETING no longer says paused, so recomputing the reason would apply the
// idle deadline that the pause suspended.
func TestMaxTTLReasonSurvivesRetryOfPausedSandbox(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, time.Hour).GetSandboxId()
	if en, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause: %v %v", en, err)
	}
	e.rt.mu.Lock()
	e.rt.deleteFails = 1
	e.rt.deleteCalls = 0
	e.rt.mu.Unlock()
	// Only the maximum deadline passed; the idle deadline is stale.
	e.clk.Advance(2 * time.Hour)
	e.m.Tick(context.Background(), false)
	waitFor(t, func() bool {
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		return e.rt.deleteCalls == 1
	})
	waitFor(t, func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return !e.m.sandboxes[id].ending
	})
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("state after failed stop %v, want DELETING", s)
	}
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("end reason after retry %v, want MAX_TTL_EXPIRED", r)
	}
	e.ex.mu.Lock()
	r := e.ex.ended[id]
	e.ex.mu.Unlock()
	if r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("exec tracker end reason %v, want MAX_TTL_EXPIRED", r)
	}
}

// TestCloseWaitsForTTLEnds checks that Close returns only after the TTL
// ends that Tick started have written their records, and that a Tick after
// Close starts no end.
func TestCloseWaitsForTTLEnds(t *testing.T) {
	e := newEnv(t)
	first := e.create(time.Minute, time.Hour).GetSandboxId()
	second := e.create(time.Minute, time.Hour).GetSandboxId()
	e.clk.Advance(2 * time.Minute)
	e.m.Tick(context.Background(), false)
	e.m.Close()
	for _, id := range []string{first, second} {
		if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatalf("sandbox %s state %v after Close", id, s)
		}
	}
	third := e.create(time.Minute, time.Hour).GetSandboxId()
	e.clk.Advance(2 * time.Minute)
	e.m.Tick(context.Background(), false)
	if s, _ := e.m.State(third); s != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("Tick after Close ended sandbox: %v", s)
	}
}

func TestEqualDeadlinesGiveMaxTTL(t *testing.T) {
	now := time.Now()
	r := Record{State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, IdleDeadline: now, MaxDeadline: now}
	if got := r.ttlReason(now); got != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("equal deadlines: %v", got)
	}
	r.IdleDeadline = now.Add(-time.Second)
	if got := r.ttlReason(now); got != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("earlier idle: %v", got)
	}
	if got := r.ttlReason(now.Add(-2 * time.Second)); got != 0 {
		t.Fatalf("no deadline passed: %v", got)
	}
}

func TestRestartReconcile(t *testing.T) {
	e := newEnv(t)
	lost := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	expired := e.create(5*time.Minute, 2*time.Hour).GetSandboxId()
	epoch := e.m.Epoch()
	// The agent restarts: every VM ended with the agent process.
	e.rt = newFakeRuntime()
	e.clk.Advance(10 * time.Minute)
	e.restart()
	if e.m.Epoch() != epoch {
		t.Fatal("epoch changed without a lost state directory")
	}
	if s, _ := e.m.State(lost); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(e.m, lost) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("lost sandbox %v %v", s, endReason(e.m, lost))
	}
	// A passed TTL wins over LOST_ON_DEVICE.
	if s, _ := e.m.State(expired); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || endReason(e.m, expired) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("expired sandbox %v %v", s, endReason(e.m, expired))
	}
	// A lost state directory gets a new epoch.
	os.Remove(e.st.Path(store.AgentFile))
	e.restart()
	if e.m.Epoch() == epoch {
		t.Fatal("epoch kept after the state was lost")
	}
}

// TestRestartRemovesRuntimeFilesOfLostSandboxes checks that a restart removes
// the runtime files of every live record whose VM is gone, before the record
// ends, for a lost VM and for a passed TTL.
func TestRestartRemovesRuntimeFilesOfLostSandboxes(t *testing.T) {
	e := newEnv(t)
	lost := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	expired := e.create(5*time.Minute, 2*time.Hour).GetSandboxId()
	// The agent restarts: the VMs ended with the agent process, and their
	// files stay on the device.
	e.rt = newFakeRuntime()
	e.rt.files[lost] = true
	e.rt.files[expired] = true
	e.clk.Advance(10 * time.Minute)
	e.restart()
	if e.rt.hasFiles(lost) || e.rt.hasFiles(expired) {
		t.Fatal("runtime files of a lost sandbox kept")
	}
	if s, _ := e.m.State(lost); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(e.m, lost) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("lost sandbox %v %v", s, endReason(e.m, lost))
	}
	if s, _ := e.m.State(expired); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || endReason(e.m, expired) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("expired sandbox %v %v", s, endReason(e.m, expired))
	}
	// The terminal records are dropped after the acknowledgement.
	var acks []*ownsandv1.SandboxUpdateAck
	for _, entry := range e.m.Entries() {
		acks = append(acks, &ownsandv1.SandboxUpdateAck{SandboxId: entry.GetSandboxId(), Revision: entry.GetRevision()})
	}
	e.m.AckStateReport(&ownsandv1.StateReportAck{ReportSequence: 1, Sandboxes: acks})
	for _, id := range []string{lost, expired} {
		if _, ok := e.m.State(id); ok {
			t.Fatalf("terminal record of %s kept after the acknowledgement", id)
		}
	}
}

// TestRestartKeepsRecordLiveWhenFileRemovalFails checks that a failed Delete
// at start leaves the record live and persisted, so the next start retries,
// and that the terminal state is reported only after the files are gone.
func TestRestartKeepsRecordLiveWhenFileRemovalFails(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.rt = newFakeRuntime()
	e.rt.files[id] = true
	e.rt.deleteFails = 1
	e.restart()
	if !e.rt.hasFiles(id) {
		t.Fatal("files removed although Delete failed")
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("state after failed Delete %v, want RUNNING", s)
	}
	// The record file still holds the live state.
	e.restart()
	if e.rt.hasFiles(id) {
		t.Fatal("files kept after the retry")
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("state after retry %v %v", s, endReason(e.m, id))
	}
}

// TestRestartPendingCleanupRejectsVMOperations checks that a live record kept
// for a cleanup retry has no VM, so pause, resume and snapshot are rejected
// instead of passing a nil VM to the runtime.
func TestRestartPendingCleanupRejectsVMOperations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	running := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	paused := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	if _, err := e.m.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: paused}); err != nil {
		t.Fatal(err)
	}
	e.rt = newFakeRuntime()
	e.rt.files[running], e.rt.files[paused] = true, true
	e.rt.deleteFails = 2
	e.restart()
	if _, err := e.m.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: running}); err == nil {
		t.Fatal("pause accepted while the restart cleanup is pending")
	}
	if _, err := e.m.Resume(ctx, &ownsandv1.ResumeSandboxCommand{SandboxId: paused}); err == nil {
		t.Fatal("resume accepted while the restart cleanup is pending")
	}
	for _, id := range []string{running, paused} {
		snapshotID, err := uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.m.CreateSnapshot(ctx, &ownsandv1.CreateSnapshotCommand{SnapshotId: snapshotID, SandboxId: id}); err == nil {
			t.Fatalf("snapshot of %s accepted while the restart cleanup is pending", id)
		}
	}
	if s, _ := e.m.State(running); s != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("state %v, want RUNNING", s)
	}
	if s, _ := e.m.State(paused); s != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("state %v, want PAUSED", s)
	}
}

func TestSurvivingVMIsStoppedAtStart(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.restart() // same runtime: the VM survived
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 {
		t.Fatal("surviving VM not stopped")
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
}

func TestLostVM(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.rt.mu.Lock()
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatal("lost VM reason")
	}
}

// blockRecord makes every later save of the sandbox record fail: it replaces
// the record file with a non-empty directory, which the atomic rename cannot
// overwrite.
func blockRecord(t *testing.T, st *store.Store, id string) {
	t.Helper()
	path := st.Path(store.SandboxesDir, id+".json")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func unblockRecord(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := os.RemoveAll(st.Path(store.SandboxesDir, id+".json")); err != nil {
		t.Fatal(err)
	}
}

func requireInternal(t *testing.T, err error) {
	t.Helper()
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("want an internal error, got %v", err)
	}
}

func TestCreateFirstRecordFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	blockRecord(t, e.st, id)
	_, err := e.m.Create(context.Background(), cmd)
	requireInternal(t, err)
	if e.rt.startCount() != 0 {
		t.Fatal("a VM started although the record was not saved")
	}
	if _, ok := e.m.State(id); ok {
		t.Fatal("unrecorded sandbox is still known")
	}
	if len(e.m.Entries()) != 0 || len(e.m.StateReport(1, nil).GetSandboxes()) != 0 || e.m.Busy() {
		t.Fatal("unrecorded sandbox is still reported or busy")
	}
	// The rolled-back sandbox does not count against the limits: three more
	// one-vCPU sandboxes and the retry below fill the four-vCPU device.
	for range 3 {
		if _, err := e.m.Create(context.Background(), e.createCmd(1, time.Hour, time.Hour)); err != nil {
			t.Fatalf("create after rollback: %v", err)
		}
	}
	// A retry of the same command after the store recovers creates one VM.
	unblockRecord(t, e.st, id)
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("retry %v %v", entry, err)
	}
	if e.rt.startCount() != 4 {
		t.Fatalf("starts %d, want 4", e.rt.startCount())
	}
}

func TestCreateRunningRecordFailureStillWatchesVM(t *testing.T) {
	e := newEnv(t)
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	// The first save works; the running save fails.
	e.rt.onCreate = func(vmruntime.Spec) { blockRecord(t, e.st, id) }
	_, err := e.m.Create(context.Background(), cmd)
	requireInternal(t, err)
	if e.rt.startCount() != 1 {
		t.Fatalf("starts %d, want 1", e.rt.startCount())
	}
	if _, ok := e.m.State(id); !ok {
		t.Fatal("sandbox with a running VM is not tracked")
	}
	// Once the store works again, the VM exit ends the record.
	unblockRecord(t, e.st, id)
	e.rt.mu.Lock()
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatal("lost VM reason")
	}
}

func TestAcksDropEndedRecords(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	del, _ := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
	// A stale acknowledgement keeps the record.
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: &ownsandv1.SandboxRevision{Epoch: e.m.Epoch(), Counter: del.GetRevision().GetCounter() - 1}})
	if _, ok := e.m.State(id); !ok {
		t.Fatal("record dropped by a stale ack")
	}
	report := e.m.StateReport(1, nil)
	if len(report.GetSandboxes()) != 1 || report.GetStateEpoch() != e.m.Epoch() {
		t.Fatalf("report %v", report)
	}
	e.m.AckStateReport(&ownsandv1.StateReportAck{ReportSequence: 1, Sandboxes: []*ownsandv1.SandboxUpdateAck{{SandboxId: id, Revision: del.GetRevision()}}})
	if _, ok := e.m.State(id); ok {
		t.Fatal("ended record kept after the acknowledgement")
	}
	if _, err := os.Stat(e.st.Path(store.SandboxesDir, id+".json")); !os.IsNotExist(err) {
		t.Fatal("ended record file kept")
	}
}

func TestResendAndIdleReports(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	n := len(e.out.entries)
	e.m.Tick(context.Background(), true)
	if len(e.out.entries) != n+1 {
		t.Fatal("unacknowledged update not resent")
	}
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: e.out.entries[len(e.out.entries)-1].GetRevision()})
	n = len(e.out.entries)
	e.m.Touch(id)
	e.m.Tick(context.Background(), true)
	if len(e.out.entries) != n+1 {
		t.Fatal("moved idle deadline not reported")
	}
	e.m.Touch(id)
	e.m.Tick(context.Background(), false)
	if len(e.out.entries) != n+1 {
		t.Fatal("idle deadline reported twice within a minute")
	}
	e.clk.Advance(time.Minute)
	e.m.Tick(context.Background(), false)
	if len(e.out.entries) != n+2 {
		t.Fatal("idle deadline not reported after a minute")
	}
}

func TestPauseResumeAndSnapshot(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	if en, err := e.m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume running %v %v", en, err)
	}
	if en, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause %v %v", en, err)
	}
	if !e.m.Busy() {
		t.Fatal("paused sandbox not busy")
	}
	snapID, _ := uuidv7.NewString()
	snap, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	if err != nil || snap.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY || snap.GetResources().GetVcpuCount() != 1 {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	e.clk.Advance(time.Hour)
	en, err := e.m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: id})
	if err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || !en.GetIdleDeadline().AsTime().After(e.clk.Now()) {
		t.Fatalf("resume %v %v", en, err)
	}
	// A sandbox from the snapshot.
	cmd := e.createCmd(1, time.Hour, time.Hour)
	cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapID}
	if _, err := e.m.Create(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	// Snapshot records survive a restart.
	e.restart()
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot lost at restart")
	}
	if report := e.m.StateReport(2, nil); len(report.GetSnapshots()) != 1 {
		t.Fatal("snapshot missing from the state report")
	}
}

// TestFailedPauseAndResumeKeepTheRecordedState checks that the manager records
// PAUSED or RUNNING only when the runtime reports success, for example when a
// runtime refuses a pause or a resume because the helper of the sandbox exited.
func TestFailedPauseAndResumeKeepTheRecordedState(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	var ce *CommandError

	e.rt.pauseErr = errors.New("injected pause failure")
	if _, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("failed pause = %v; want an internal error", err)
	}
	if got := e.m.StateReport(1, nil).GetSandboxes()[0].GetState(); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("state after the failed pause = %v; want RUNNING", got)
	}

	e.rt.pauseErr = nil
	if _, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	e.rt.resumeErr = errors.New("injected resume failure")
	if _, err := e.m.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: id}); !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("failed resume = %v; want an internal error", err)
	}
	if got := e.m.StateReport(2, nil).GetSandboxes()[0].GetState(); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("state after the failed resume = %v; want PAUSED", got)
	}
}

func TestDeleteSnapshotDuringCreate(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	e.rt.snapStarted = make(chan struct{})
	e.rt.snapBlock = make(chan struct{})
	type result struct {
		entry *ownsandv1.SnapshotStateEntry
		err   error
	}
	done := make(chan result, 1)
	go func() {
		entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
		done <- result{entry, err}
	}()
	<-e.rt.snapStarted
	// While the capture runs, the snapshot is not recorded and not reported.
	if e.m.HasSnapshot(snapID) {
		t.Fatal("capturing snapshot is recorded")
	}
	if report := e.m.StateReport(2, nil); len(report.GetSnapshots()) != 0 {
		t.Fatal("capturing snapshot is in the state report")
	}
	// A second create for the same snapshot does not touch the capture.
	var ce *CommandError
	other := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: other}); !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("duplicate create during capture = %v", err)
	}
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete during capture = %v", err)
	}
	close(e.rt.snapBlock)
	res := <-done
	if !errors.As(res.err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
		t.Fatalf("create after a delete = %v, %v", res.entry, res.err)
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot published after its delete")
	}
	if report := e.m.StateReport(3, nil); len(report.GetSnapshots()) != 0 {
		t.Fatal("deleted snapshot in the state report")
	}
	if dir, err := e.m.ImportDir(snapID); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after the aborted create: %v", err)
	}
	// The aborted create leaves no trace: a new create of the same ID works
	// and a repeated create returns the same snapshot.
	e.rt.snapStarted = nil
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id}); err != nil {
		t.Fatalf("create after the aborted create = %v", err)
	}
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id}); err != nil {
		t.Fatalf("duplicate create = %v", err)
	}
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot missing after a normal create")
	}
	e.restart()
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot missing after restart")
	}
}

func TestDeleteSnapshot(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	if _, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id}); err != nil {
		t.Fatal(err)
	}
	// A delete of a snapshot in use by two readers succeeds at once and
	// takes effect when the last reader ends; the snapshot takes no new
	// readers meanwhile.
	_, release, ok := e.m.AcquireSnapshot(snapID)
	if !ok {
		t.Fatal("AcquireSnapshot failed")
	}
	_, release2, ok := e.m.AcquireSnapshot(snapID)
	if !ok {
		t.Fatal("second AcquireSnapshot failed")
	}
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete in use = %v", err)
	}
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot in use deleted before its readers ended")
	}
	if _, _, ok := e.m.AcquireSnapshot(snapID); ok {
		t.Fatal("AcquireSnapshot of a snapshot being deleted succeeded")
	}
	if _, _, ok := e.m.SnapshotContent(snapID); ok {
		t.Fatal("SnapshotContent of a snapshot being deleted succeeded")
	}
	var ce *CommandError
	fromSnap := e.createCmd(1, 10*time.Minute, 2*time.Hour)
	fromSnap.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: snapID}
	if _, err := e.m.Create(context.Background(), fromSnap); !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("create from a snapshot being deleted = %v", err)
	}
	release()
	release() // A second call is a no-op.
	if !e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot deleted while a reader remains")
	}
	release2()
	if e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot still recorded after its last reader ended")
	}
	if dir, err := e.m.ImportDir(snapID); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory after the deferred delete: %v", err)
	}
	// Deleting an unknown snapshot succeeds.
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete again = %v", err)
	}
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: "nope"}); !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("delete invalid = %v", err)
	}
	// The deletion survives a restart.
	e.restart()
	if e.m.HasSnapshot(snapID) {
		t.Fatal("snapshot back after restart")
	}
	if report := e.m.StateReport(2, nil); len(report.GetSnapshots()) != 0 {
		t.Fatal("deleted snapshot in the state report")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
