// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	// snapshotErr, when set, makes Snapshot fail after it wrote its content,
	// like a runtime whose resume failed after a completed capture.
	snapshotErr error
	// deleteFails is the number of Delete calls that fail before one works.
	deleteFails int
	deleteCalls int
	// deleteStarted and deleteBlock, when set, make Delete signal that the
	// stop started and wait until deleteBlock is closed, like a VM stop that
	// is still running.
	deleteStarted chan struct{}
	deleteBlock   chan struct{}
	// pauseErr and resumeErr, when set, make Pause and Resume fail.
	pauseErr  error
	resumeErr error
	// pauseCalls and resumeCalls count the Pause and Resume calls that
	// reached the fake, including failing ones.
	pauseCalls  int
	resumeCalls int
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
	r.mu.Lock()
	r.pauseCalls++
	r.mu.Unlock()
	if r.pauseErr != nil {
		return r.pauseErr
	}
	r.mu.Lock()
	r.paused[vm.SandboxID()] = true
	r.mu.Unlock()
	return nil
}
func (r *fakeRuntime) Resume(_ context.Context, vm vmruntime.VM) error {
	r.mu.Lock()
	r.resumeCalls++
	r.mu.Unlock()
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
	r.mu.Lock()
	snapshotErr := r.snapshotErr
	r.mu.Unlock()
	if snapshotErr != nil {
		return vmruntime.SnapshotInfo{}, snapshotErr
	}
	return vmruntime.SnapshotInfo{SizeBytes: 4}, nil
}
func (r *fakeRuntime) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	r.deleteCalls++
	if r.deleteBlock != nil {
		started, block := r.deleteStarted, r.deleteBlock
		r.mu.Unlock()
		close(started)
		<-block
		r.mu.Lock()
	}
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
	// calls counts the SandboxEnded notifications of each sandbox.
	calls map[string]int
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
	if e.calls == nil {
		e.calls = map[string]int{}
	}
	e.calls[id]++
}

func (e *execs) endedCalls(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
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

// TestDeleteMarkerIsNotReportedUntilItIsSaved checks that a delete of an
// unknown sandbox whose marker cannot be saved fails, leaves no in-memory
// marker, and that a retry saves the marker so that it survives a restart and
// a delayed create is still rejected. The marker file is blocked in one case
// and the revision counter in the other.
func TestDeleteMarkerIsNotReportedUntilItIsSaved(t *testing.T) {
	cases := []struct {
		name  string
		fault func(t *testing.T, e *env, id string) (restore func())
	}{
		{"record save fails", func(t *testing.T, e *env, id string) func() {
			blockRecord(t, e.st, id)
			return func() { unblockRecord(t, e.st, id) }
		}},
		{"revision save fails", func(t *testing.T, e *env, id string) func() {
			return failRevisions(t, e.st)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			cmd := e.createCmd(1, time.Hour, time.Hour)
			id := cmd.GetSandboxId()
			del := &ownsandv1.DeleteSandboxCommand{SandboxId: id}
			restore := tc.fault(t, e, id)
			for attempt := 1; attempt <= 2; attempt++ {
				en, err := e.m.Delete(ctx, del)
				requireInternal(t, err)
				if en != nil {
					t.Fatalf("delete attempt %d returned entry %v with its error", attempt, en)
				}
				if _, ok := e.m.State(id); ok {
					t.Fatalf("delete attempt %d left an unsaved marker in memory", attempt)
				}
				if got := e.out.states(id); len(got) != 0 {
					t.Fatalf("delete attempt %d sent updates %v for an unsaved marker", attempt, got)
				}
			}
			restore()
			en, err := e.m.Delete(ctx, del)
			if err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
				t.Fatalf("delete after the store recovered = %v, %v", en, err)
			}
			if got := persistedState(t, e.st, id); got != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
				t.Fatalf("persisted state = %v; want DELETED", got)
			}
			e.restart()
			if _, err := e.m.Create(ctx, cmd); err == nil {
				t.Fatal("create after the restart accepted; the delete marker was lost")
			}
			if e.rt.startCount() != 0 {
				t.Fatal("VM created after the delete")
			}
		})
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

// failCallerDelete pauses the sandbox when pause is set, makes the next
// runtime Delete fail and runs a caller delete. It returns when the failed
// attempt left the record in DELETING with the ending flag cleared.
func (e *env) failCallerDelete(id string, pause bool) {
	e.t.Helper()
	if pause {
		if en, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil || en.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			e.t.Fatalf("pause: %v %v", en, err)
		}
	}
	e.rt.mu.Lock()
	e.rt.deleteFails = 1
	e.rt.mu.Unlock()
	if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err == nil {
		e.t.Fatal("delete succeeded although the runtime Delete failed")
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		e.t.Fatalf("state after failed delete %v, want DELETING", s)
	}
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		// A non-terminal entry carries no end reason on the wire.
		e.t.Fatalf("entry end reason %v while DELETING", r)
	}
	waitFor(e.t, func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return !e.m.sandboxes[id].ending
	})
}

// TestCallerDeleteRetryKeepsReasonWhenIdleDeadlinePassed checks that a Tick
// retry of a failed caller delete of a paused sandbox ends it as
// DELETED_BY_CALLER. The pause suspended the idle deadline, but the record in
// DELETING no longer says paused, so the stale idle deadline counts as passed
// and used to replace the caller reason with IDLE_TTL_EXPIRED.
func TestCallerDeleteRetryKeepsReasonWhenIdleDeadlinePassed(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 24*time.Hour).GetSandboxId()
	e.failCallerDelete(id, true)
	// Only the stale idle deadline passed; the maximum deadline did not.
	e.clk.Advance(time.Hour)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason after retry %v, want DELETED_BY_CALLER", r)
	}
	e.ex.mu.Lock()
	r, ended := e.ex.ended[id]
	e.ex.mu.Unlock()
	if !ended || r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("exec tracker end reason %v (notified %v), want DELETED_BY_CALLER", r, ended)
	}
}

// TestCallerDeleteRetryKeepsReasonWhenMaxDeadlinePassed checks that the
// recorded caller reason also wins over MAX_TTL_EXPIRED on a Tick retry.
func TestCallerDeleteRetryKeepsReasonWhenMaxDeadlinePassed(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, time.Hour).GetSandboxId()
	e.failCallerDelete(id, true)
	e.clk.Advance(2 * time.Hour)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason after retry %v, want DELETED_BY_CALLER", r)
	}
}

// TestCallerDeleteRetryWaitsForDeadline checks that the retry cadence stays
// as it was: Tick does not retry a failed caller delete while no deadline
// passed, so a stuck delete is not retried every second.
func TestCallerDeleteRetryWaitsForDeadline(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 24*time.Hour).GetSandboxId()
	e.failCallerDelete(id, false)
	e.rt.mu.Lock()
	e.rt.deleteCalls = 0
	e.rt.mu.Unlock()
	e.m.Tick(context.Background(), false)
	time.Sleep(50 * time.Millisecond)
	e.rt.mu.Lock()
	calls := e.rt.deleteCalls
	e.rt.mu.Unlock()
	if calls != 0 {
		t.Fatalf("Tick retried the runtime Delete %d times before a deadline passed", calls)
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("state %v, want DELETING", s)
	}
}

// TestRestartKeepsCallerReasonOfDeletingSandbox checks that an agent start
// finishes a caller delete that failed before the restart with
// DELETED_BY_CALLER, also when the idle deadline or both deadlines passed
// while the agent was stopped.
func TestRestartKeepsCallerReasonOfDeletingSandbox(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pause bool
		wait  time.Duration
	}{
		{"idle deadline passed", true, time.Hour},
		{"both deadlines passed", true, 48 * time.Hour},
		{"no deadline passed", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.create(10*time.Minute, 24*time.Hour).GetSandboxId()
			e.failCallerDelete(id, tc.pause)
			// The agent restarts; the VM ended with the agent process.
			e.rt = newFakeRuntime()
			e.clk.Advance(tc.wait)
			e.restart()
			if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
				t.Fatalf("state after restart %v, want DELETED", s)
			}
			if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
				t.Fatalf("end reason after restart %v, want DELETED_BY_CALLER", r)
			}
		})
	}
}

// TestCallerDeleteEndsFailedTTLEnd checks that a caller delete after a failed
// TTL end records DELETED_BY_CALLER, and that a failed TTL end keeps its TTL
// reason when Tick retries it (see TestTTLEndRetriesAfterStopFailure and
// TestMaxTTLReasonSurvivesRetryOfPausedSandbox for the Tick retry).
func TestCallerDeleteEndsFailedTTLEnd(t *testing.T) {
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
	waitFor(t, func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return !e.m.sandboxes[id].ending
	})
	if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil {
		t.Fatalf("caller delete after a failed TTL end: %v", err)
	}
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("end reason %v, want DELETED_BY_CALLER", r)
	}
	e.ex.mu.Lock()
	r := e.ex.ended[id]
	e.ex.mu.Unlock()
	if r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("exec tracker end reason %v, want DELETED_BY_CALLER", r)
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

func TestEndReasonAtKeepsRecordedReasonOfDeletingRecord(t *testing.T) {
	now := time.Now()
	deleting := func(reason ownsandv1.SandboxEndReason, idle, max time.Time) Record {
		return Record{State: ownsandv1.SandboxState_SANDBOX_STATE_DELETING, EndReason: reason, IdleDeadline: idle, MaxDeadline: max}
	}
	const (
		caller = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
		idle   = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
		maxTTL = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
		none   = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED
	)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	for _, tc := range []struct {
		name string
		rec  Record
		want ownsandv1.SandboxEndReason
	}{
		{"caller, idle passed", deleting(caller, past, future), caller},
		{"caller, max passed", deleting(caller, future, past), caller},
		{"caller, both passed", deleting(caller, past, past), caller},
		{"caller, none passed", deleting(caller, future, future), none},
		{"idle TTL, max passed", deleting(idle, future, past), idle},
		{"max TTL, idle passed", deleting(maxTTL, past, future), maxTTL},
		{"running, idle passed", Record{State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, IdleDeadline: past, MaxDeadline: future}, idle},
	} {
		if got := tc.rec.endReasonAt(now); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
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

// loseVMWithFailedEnd stops the VM of the sandbox as a lost VM while the next
// runtime Delete fails, and returns when the failed end left the record in
// DELETING with the ending flag cleared and the runtime Delete was called once.
func (e *env) loseVMWithFailedEnd(id string) {
	e.t.Helper()
	e.rt.mu.Lock()
	e.rt.deleteFails = 1
	e.rt.deleteCalls = 0
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
	waitFor(e.t, func() bool {
		e.rt.mu.Lock()
		calls := e.rt.deleteCalls
		e.rt.mu.Unlock()
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return calls == 1 && !e.m.sandboxes[id].ending
	})
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		e.t.Fatalf("state after the failed end %v, want DELETING", s)
	}
	e.m.mu.Lock()
	reason := e.m.sandboxes[id].rec.EndReason
	e.m.mu.Unlock()
	if reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		e.t.Fatalf("recorded end reason %v, want LOST_ON_DEVICE", reason)
	}
}

// TestRestartEndsLostVMAsFailedAfterFailedEnd checks that a lost VM whose end
// failed (the record stays in DELETING with LOST_ON_DEVICE) ends FAILED with
// LOST_ON_DEVICE at the next start, not DELETED.
func TestRestartEndsLostVMAsFailedAfterFailedEnd(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.loseVMWithFailedEnd(id)
	e.restart()
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || endReason(e.m, id) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("state after restart %v %v, want FAILED LOST_ON_DEVICE", s, endReason(e.m, id))
	}
}

// TestTickRetriesFailedEndOfLostVM checks that a running agent ends a lost VM
// whose first end failed as FAILED/LOST_ON_DEVICE at a later Tick, without a
// passed deadline, and notifies the exec tracker exactly once.
func TestTickRetriesFailedEndOfLostVM(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.loseVMWithFailedEnd(id)
	if n := e.ex.endedCalls(id); n != 0 {
		t.Fatalf("exec tracker notified %d times before the VM was stopped", n)
	}
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("end reason %v, want LOST_ON_DEVICE", r)
	}
	// Further Ticks leave the terminal record alone.
	e.m.Tick(context.Background(), false)
	e.m.Tick(context.Background(), true)
	time.Sleep(50 * time.Millisecond)
	if n := e.ex.endedCalls(id); n != 1 {
		t.Fatalf("exec tracker notified %d times, want 1", n)
	}
	e.rt.mu.Lock()
	calls := e.rt.deleteCalls
	e.rt.mu.Unlock()
	if calls != 2 {
		t.Fatalf("runtime Delete calls %d, want 2", calls)
	}
}

// TestFailedEndOfLostVMKeepsDeadlineWinning checks that a passed TTL deadline
// wins over the recorded LOST_ON_DEVICE of a failed end, at a Tick and at a
// restart.
func TestFailedEndOfLostVMKeepsDeadlineWinning(t *testing.T) {
	for _, restart := range []bool{false, true} {
		e := newEnv(t)
		id := e.create(5*time.Minute, 2*time.Hour).GetSandboxId()
		e.loseVMWithFailedEnd(id)
		e.clk.Advance(10 * time.Minute)
		if restart {
			e.restart()
		} else {
			e.m.Tick(context.Background(), false)
		}
		waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
		if r := endReason(e.m, id); r != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
			t.Fatalf("restart=%v: end reason %v, want IDLE_TTL_EXPIRED", restart, r)
		}
	}
}

// TestRestartEndsFailedCallerDeleteAsDeleted checks that a DELETING record
// with a caller or TTL reason still ends DELETED with that reason at a start.
func TestRestartEndsFailedCallerDeleteAsDeleted(t *testing.T) {
	e := newEnv(t)
	caller := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.failCallerDelete(caller, false)
	ttl := e.create(10*time.Minute, time.Hour).GetSandboxId()
	e.rt.mu.Lock()
	e.rt.deleteFails = 1
	e.rt.mu.Unlock()
	e.clk.Advance(11 * time.Minute)
	e.m.Tick(context.Background(), false)
	waitFor(t, func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		sb := e.m.sandboxes[ttl]
		return !sb.ending && sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	})
	e.restart()
	if s, _ := e.m.State(caller); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || endReason(e.m, caller) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		t.Fatalf("caller delete %v %v", s, endReason(e.m, caller))
	}
	if s, _ := e.m.State(ttl); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || endReason(e.m, ttl) != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("ttl end %v %v", s, endReason(e.m, ttl))
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
	e.m.runningBackoff = time.Millisecond
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

// failRevisions makes every later revision write fail, and returns the
// function that repairs the store. A directory in place of agent.json makes
// the store unable to read the file that holds the revision counter; the
// fault does not depend on directory permissions, and the sandbox record
// directory stays writable, so only the revision persistence fails. The
// repair is idempotent, and the cleanup also covers a failed assertion.
func failRevisions(t *testing.T, st *store.Store) (restore func()) {
	t.Helper()
	agentFile := st.Path(store.AgentFile)
	saved := agentFile + ".saved"
	if err := os.Rename(agentFile, saved); err != nil {
		t.Error(err)
		return func() {}
	}
	if err := os.Mkdir(agentFile, 0o700); err != nil {
		t.Error(err)
		return func() {}
	}
	restore = func() {
		if _, err := os.Stat(saved); err != nil {
			return
		}
		if err := os.Remove(agentFile); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(saved, agentFile); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// persistedState returns the state of the saved record of a sandbox.
func persistedState(t *testing.T, st *store.Store, id string) ownsandv1.SandboxState {
	t.Helper()
	var state ownsandv1.SandboxState
	found := false
	if err := st.LoadSandboxes(func(data []byte) error {
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		if rec.SandboxID == id {
			state, found = rec.State, true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no saved record of %s", id)
	}
	return state
}

func requireStates(t *testing.T, got, want []ownsandv1.SandboxState) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("reported states %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reported states %v, want %v", got, want)
		}
	}
}

// TestCreateRecoversFromOneRevisionFailure checks that one failed revision
// write after the VM started does not fail the create: the next attempt
// publishes RUNNING, with the one VM that already runs.
func TestCreateRecoversFromOneRevisionFailure(t *testing.T) {
	e := newEnv(t)
	e.m.runningBackoff = time.Millisecond
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	var restore func()
	e.rt.onCreate = func(vmruntime.Spec) { restore = failRevisions(t, e.st) }
	attempts := 0
	e.m.onRunningCommitFailed = func(attempt int, willRetry bool) {
		attempts++
		if !willRetry {
			t.Error("the first failure did not schedule a retry")
		}
		if state, _ := e.m.State(id); state != ownsandv1.SandboxState_SANDBOX_STATE_CREATING {
			t.Errorf("a revision failure changed the record to %v", state)
		}
		restore()
	}
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create %v %v", entry, err)
	}
	if attempts != 1 || e.rt.startCount() != 1 {
		t.Fatalf("failed attempts %d, VM starts %d, want 1 and 1", attempts, e.rt.startCount())
	}
	if got := persistedState(t, e.st, id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("saved record state %v, want RUNNING", got)
	}
	requireStates(t, e.out.states(id), []ownsandv1.SandboxState{ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING})
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 1 {
		t.Fatalf("running VMs %v, want one", ids)
	}
}

// TestCreateRecoversFromOneRecordSaveFailure checks the same recovery for a
// failed record save. Unlike a revision failure, a save failure leaves the new
// state in the record, and the retry still publishes it.
func TestCreateRecoversFromOneRecordSaveFailure(t *testing.T) {
	e := newEnv(t)
	e.m.runningBackoff = time.Millisecond
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	e.rt.onCreate = func(vmruntime.Spec) { blockRecord(t, e.st, id) }
	e.m.onRunningCommitFailed = func(int, bool) {
		if state, _ := e.m.State(id); state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Errorf("a save failure left the record in %v, want the new RUNNING state", state)
		}
		unblockRecord(t, e.st, id)
	}
	entry, err := e.m.Create(context.Background(), cmd)
	if err != nil || entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("create %v %v", entry, err)
	}
	if got := persistedState(t, e.st, id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("saved record state %v, want RUNNING", got)
	}
	if e.rt.startCount() != 1 {
		t.Fatalf("VM starts %d, want 1", e.rt.startCount())
	}
}

// TestCreateStopsVMWhenRunningRecordCannotBePublished checks that a create
// whose RUNNING record fails every attempt does not report success with a VM
// that nobody tracks: it ends the sandbox as CREATE_FAILED and stops the VM.
func TestCreateStopsVMWhenRunningRecordCannotBePublished(t *testing.T) {
	e := newEnv(t)
	e.m.runningBackoff = time.Millisecond
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	var restore func()
	e.rt.onCreate = func(vmruntime.Spec) { restore = failRevisions(t, e.st) }
	attempts := 0
	e.m.onRunningCommitFailed = func(attempt int, willRetry bool) {
		attempts++
		if attempt != attempts || willRetry != (attempt < DefaultRunningAttempts) {
			t.Errorf("attempt %d willRetry %v after %d failures", attempt, willRetry, attempts)
		}
		if !willRetry {
			// The store works again for the teardown.
			restore()
		}
	}
	entry, err := e.m.Create(context.Background(), cmd)
	requireInternal(t, err)
	if attempts != DefaultRunningAttempts {
		t.Fatalf("failed attempts %d, want %d", attempts, DefaultRunningAttempts)
	}
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatalf("entry %v", entry)
	}
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 || e.rt.hasFiles(id) {
		t.Fatalf("VM %v or its files remain", ids)
	}
	if got := persistedState(t, e.st, id); got != ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		t.Fatalf("saved record state %v, want FAILED", got)
	}
	if e.ex.ended[id] != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
		t.Fatal("execs not ended with the sandbox")
	}
	for _, state := range e.out.states(id) {
		if state == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatal("RUNNING reported for a sandbox that did not publish it")
		}
	}
	if e.rt.startCount() != 1 {
		t.Fatalf("VM starts %d, want 1", e.rt.startCount())
	}
}

// TestCreateKeepsSandboxTrackedWhenTheEndFails checks that, when neither the
// RUNNING record nor the end of the sandbox can be recorded, the sandbox and
// its VM stay tracked, and that a delete or a TTL tick ends them later.
func TestCreateKeepsSandboxTrackedWhenTheEndFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(e *env, id string)
		want ownsandv1.SandboxEndReason
	}{
		{"delete", func(e *env, id string) {
			if _, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id}); err != nil {
				t.Fatal(err)
			}
		}, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER},
		{"ttl", func(e *env, id string) {
			e.clk.Advance(2 * time.Hour)
			e.m.Tick(context.Background(), false)
			waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
		}, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.m.runningBackoff = time.Millisecond
			cmd := e.createCmd(1, time.Hour, time.Hour)
			id := cmd.GetSandboxId()
			var restore func()
			e.rt.onCreate = func(vmruntime.Spec) { restore = failRevisions(t, e.st) }
			_, err := e.m.Create(context.Background(), cmd)
			requireInternal(t, err)
			if _, ok := e.m.State(id); !ok {
				t.Fatal("sandbox with a running VM is not tracked")
			}
			if ids, _ := e.rt.Running(context.Background()); len(ids) != 1 {
				t.Fatalf("running VMs %v, want the one that started", ids)
			}
			e.m.mu.Lock()
			ending := e.m.sandboxes[id].ending
			e.m.mu.Unlock()
			if ending {
				t.Fatal("the failed end left the sandbox ending, so no later end could retry")
			}
			restore()
			tc.end(e, id)
			if state, _ := e.m.State(id); state != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
				t.Fatalf("state %v, want DELETED", state)
			}
			if got := endReason(e.m, id); got != tc.want {
				t.Fatalf("end reason %v, want %v", got, tc.want)
			}
			if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 || e.rt.hasFiles(id) {
				t.Fatalf("VM %v or its files remain", ids)
			}
		})
	}
}

// TestCreateYieldsToConcurrentTTLEnd checks that a TTL end that started while
// the create retries its RUNNING record keeps control of the teardown: the
// create commits no RUNNING record afterwards, and the TTL end stops the VM.
func TestCreateYieldsToConcurrentTTLEnd(t *testing.T) {
	e := newEnv(t)
	e.m.runningBackoff = time.Millisecond
	cmd := e.createCmd(1, time.Hour, time.Hour)
	id := cmd.GetSandboxId()
	var restore func()
	e.rt.onCreate = func(vmruntime.Spec) { restore = failRevisions(t, e.st) }
	e.m.onRunningCommitFailed = func(int, bool) {
		// The TTL passed while the create waits; the end goroutine blocks on
		// the command lock that the create holds.
		e.clk.Advance(2 * time.Hour)
		e.m.Tick(context.Background(), false)
		restore()
	}
	entry, err := e.m.Create(context.Background(), cmd)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
		t.Fatalf("create %v %v, want an aborted error", entry, err)
	}
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if got := endReason(e.m, id); got != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("end reason %v, want MAX_TTL_EXPIRED", got)
	}
	for _, state := range e.out.states(id) {
		if state == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			t.Fatal("a RUNNING update overwrote the TTL end")
		}
	}
	if ids, _ := e.rt.Running(context.Background()); len(ids) != 0 || e.rt.hasFiles(id) {
		t.Fatalf("VM %v or its files remain", ids)
	}
	if e.rt.startCount() != 1 {
		t.Fatalf("VM starts %d, want 1", e.rt.startCount())
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

// count returns the number of entries sent so far.
func (o *outbox) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.entries)
}

// lastEntry returns the newest outbox entry of the sandbox.
func (o *outbox) lastEntry(id string) *ownsandv1.SandboxStateEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.entries) - 1; i >= 0; i-- {
		if o.entries[i].GetSandboxId() == id {
			return o.entries[i]
		}
	}
	return nil
}

// requireTerminalResend checks that every resend Tick sends the unacknowledged
// terminal entry of the sandbox again, with the given state and end reason.
func requireTerminalResend(t *testing.T, e *env, id string, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason) {
	t.Helper()
	// The record turns terminal before the end sends its update.
	waitFor(t, func() bool { return e.out.lastEntry(id).GetState() == state })
	for i := 0; i < 3; i++ {
		n := e.out.count()
		e.m.Tick(context.Background(), true)
		if e.out.count() != n+1 {
			t.Fatalf("resend %d: %d entries, want %d", i, e.out.count(), n+1)
		}
		got := e.out.lastEntry(id)
		if got.GetState() != state || got.GetEndReason() != reason {
			t.Fatalf("resend %d: state %v end reason %v, want %v %v", i, got.GetState(), got.GetEndReason(), state, reason)
		}
	}
}

// TestResendTerminalUpdateAfterDelete checks that Tick resends the
// unacknowledged terminal update of a deleted sandbox until the control plane
// acknowledges it, and then stops.
func TestResendTerminalUpdateAfterDelete(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
	if err != nil {
		t.Fatal(err)
	}
	requireTerminalResend(t, e, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
	// The acknowledgement drops the record, so nothing is resent.
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: del.GetRevision()})
	n := e.out.count()
	e.m.Tick(context.Background(), true)
	if e.out.count() != n {
		t.Fatal("acknowledged terminal update resent")
	}
}

func TestResendTerminalUpdateAfterTTL(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	e.clk.Advance(11 * time.Minute)
	e.m.Tick(context.Background(), false)
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	requireTerminalResend(t, e, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED)
}

func TestResendTerminalUpdateAfterLostVM(t *testing.T) {
	e := newEnv(t)
	id := e.create(time.Hour, 2*time.Hour).GetSandboxId()
	e.rt.mu.Lock()
	e.rt.vms[id].stop()
	e.rt.mu.Unlock()
	waitState(t, e.m, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	requireTerminalResend(t, e, id, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE)
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: e.out.lastEntry(id).GetRevision()})
	n := e.out.count()
	e.m.Tick(context.Background(), true)
	if e.out.count() != n {
		t.Fatal("acknowledged lost-VM update resent")
	}
}

// TestResendSkipsSandboxWhoseEndIsRunning checks that Tick sends no terminal
// update for a sandbox whose VM stop has not returned, and starts no second
// end for it.
func TestResendSkipsSandboxWhoseEndIsRunning(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	e.rt.mu.Lock()
	e.rt.deleteStarted = make(chan struct{})
	e.rt.deleteBlock = make(chan struct{})
	started, block := e.rt.deleteStarted, e.rt.deleteBlock
	e.rt.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
		done <- err
	}()
	<-started
	n := e.out.count()
	e.clk.Advance(11 * time.Minute)
	e.m.Tick(context.Background(), true)
	if e.out.count() != n {
		t.Fatalf("Tick sent %d entries while the end was running", e.out.count()-n)
	}
	if s, _ := e.m.State(id); s != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		t.Fatalf("state %v, want DELETING", s)
	}
	e.rt.mu.Lock()
	calls := e.rt.deleteCalls
	e.rt.mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d runtime Delete calls while the end was running, want 1", calls)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireTerminalResend(t, e, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
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

// TestSnapshotErrorNeverYieldsReady checks that an error from the runtime
// snapshot, for example a resume that failed after a completed capture, fails
// the command with INTERNAL and leaves no snapshot record, no snapshot
// content and no READY entry in the state report, and that a retry works.
func TestSnapshotErrorNeverYieldsReady(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	snapID, _ := uuidv7.NewString()
	e.rt.mu.Lock()
	e.rt.snapshotErr = errors.New("resume after snapshot: hcs: resume rejected")
	e.rt.mu.Unlock()
	entry, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id})
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL || entry != nil {
		t.Fatalf("snapshot %v %v, want an INTERNAL error and no entry", entry, err)
	}
	if e.m.HasSnapshot(snapID) {
		t.Fatal("failed snapshot is known to the manager")
	}
	for _, s := range e.m.StateReport(2, nil).GetSnapshots() {
		t.Fatalf("state report holds the snapshot %v after a failed capture", s)
	}
	if dir, err := e.st.SnapshotDir(snapID); err != nil {
		t.Fatal(err)
	} else {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("snapshot directory %s remains: %v", dir, err)
		}
	}
	// The error leaves the capture unregistered, so a retry can succeed.
	e.rt.mu.Lock()
	e.rt.snapshotErr = nil
	e.rt.mu.Unlock()
	if en, err := e.m.CreateSnapshot(context.Background(), &ownsandv1.CreateSnapshotCommand{SnapshotId: snapID, SandboxId: id}); err != nil || en.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("retry %v %v", en, err)
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

// vmState reports the pause and call counters of the fake runtime for a sandbox.
func (r *fakeRuntime) vmState(id string) (paused bool, pauseCalls, resumeCalls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[id], r.pauseCalls, r.resumeCalls
}

// breakFile replaces the file at path with a directory so that the atomic
// write of that file fails, and returns the function that restores the bytes.
func breakFile(t *testing.T, path string) (restore func()) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPauseAndResumeUndoTheRuntimeWhenTheRevisionSaveFails checks that a pause
// or a resume that the runtime accepted is undone when the revision counter
// cannot be persisted, so that the VM keeps matching the unchanged record.
func TestPauseAndResumeUndoTheRuntimeWhenTheRevisionSaveFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	agentPath := filepath.Join(e.st.Dir(), store.AgentFile)
	pauseCmd := &ownsandv1.PauseSandboxCommand{SandboxId: id}
	resumeCmd := &ownsandv1.ResumeSandboxCommand{SandboxId: id}
	check := func(step string, en *ownsandv1.SandboxStateEntry, err error, wantErr bool, state ownsandv1.SandboxState, paused bool, pauseCalls, resumeCalls int) {
		t.Helper()
		if wantErr {
			if err == nil || !strings.Contains(err.Error(), "sandboxes: persist revision:") || en != nil {
				t.Fatalf("%s: entry %v, error %v; want a nil entry and a persist-revision error", step, en, err)
			}
		} else if err != nil || en.GetState() != state {
			t.Fatalf("%s: entry %v, error %v; want state %v", step, en, err, state)
		}
		if got, _ := e.m.State(id); got != state {
			t.Fatalf("%s: record state = %v; want %v", step, got, state)
		}
		gotPaused, gotPause, gotResume := e.rt.vmState(id)
		if gotPaused != paused || gotPause != pauseCalls || gotResume != resumeCalls {
			t.Fatalf("%s: paused=%v pause calls=%d resume calls=%d; want %v %d %d", step, gotPaused, gotPause, gotResume, paused, pauseCalls, resumeCalls)
		}
	}

	restore := breakFile(t, agentPath)
	en, err := e.m.Pause(ctx, pauseCmd)
	check("pause with a failing revision save", en, err, true, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, false, 1, 1)
	restore()
	en, err = e.m.Pause(ctx, pauseCmd)
	check("pause after the store recovered", en, err, false, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, true, 2, 1)

	restore = breakFile(t, agentPath)
	en, err = e.m.Resume(ctx, resumeCmd)
	check("resume with a failing revision save", en, err, true, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, true, 3, 2)
	restore()
	en, err = e.m.Resume(ctx, resumeCmd)
	check("resume after the store recovered", en, err, false, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, false, 3, 3)
}

// TestFailedUndoJoinsTheErrors checks that a failed undo is returned together
// with the revision error and that the record stays unchanged.
func TestFailedUndoJoinsTheErrors(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	restore := breakFile(t, filepath.Join(e.st.Dir(), store.AgentFile))
	defer restore()
	undoErr := errors.New("injected undo failure")
	e.rt.resumeErr = undoErr
	en, err := e.m.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: id})
	if en != nil || err == nil || !strings.Contains(err.Error(), "sandboxes: persist revision:") || !errors.Is(err, undoErr) {
		t.Fatalf("pause = %v, %v; want the persist-revision error joined with the undo error", en, err)
	}
	if got, _ := e.m.State(id); got != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("record state = %v; want RUNNING", got)
	}
}

// TestPauseDoesNotUndoWhenOnlyTheRecordSaveFails checks that a failure of
// SaveSandbox after the in-memory change applied keeps the new state and does
// not call the opposite runtime method.
func TestPauseDoesNotUndoWhenOnlyTheRecordSaveFails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	breakFile(t, filepath.Join(e.st.Dir(), store.SandboxesDir, id+".json"))
	if en, err := e.m.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: id}); err == nil || en != nil {
		t.Fatalf("pause = %v, %v; want an error and a nil entry", en, err)
	}
	if got, _ := e.m.State(id); got != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("record state = %v; want PAUSED", got)
	}
	if paused, pauseCalls, resumeCalls := e.rt.vmState(id); !paused || pauseCalls != 1 || resumeCalls != 0 {
		t.Fatalf("paused=%v pause calls=%d resume calls=%d; want true 1 0", paused, pauseCalls, resumeCalls)
	}
	if _, err := e.m.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: id}); err != nil {
		t.Fatalf("second pause = %v", err)
	}
	if paused, pauseCalls, _ := e.rt.vmState(id); !paused || pauseCalls != 1 {
		t.Fatalf("paused=%v pause calls=%d after the second pause; want true 1", paused, pauseCalls)
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

func TestMacOSSnapshotHardwareModels(t *testing.T) {
	e := newEnv(t)
	add := func(guest ownsandv1.GuestOs, model string) string {
		id, _ := uuidv7.NewString()
		dir, err := e.m.ImportDir(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, contentDir), 0o700); err != nil {
			t.Fatal(err)
		}
		entry, err := e.m.AddImportedSnapshot(&ownsandv1.SnapshotStateEntry{
			SnapshotId: id, GuestOs: guest, Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 64},
			MacosHardwareModel: []byte(model),
		})
		if err != nil || string(entry.GetMacosHardwareModel()) != model {
			t.Fatalf("import %v %v", entry, err)
		}
		return id
	}
	add(ownsandv1.GuestOs_GUEST_OS_MACOS, "model-a")
	add(ownsandv1.GuestOs_GUEST_OS_MACOS, "model-a")
	b := add(ownsandv1.GuestOs_GUEST_OS_MACOS, "model-b")
	// A Linux snapshot never reports a macOS hardware model.
	add(ownsandv1.GuestOs_GUEST_OS_LINUX, "model-c")
	models := map[string]bool{}
	for _, m := range e.m.MacOSHardwareModels() {
		models[string(m)] = true
	}
	if len(models) != 2 || !models["model-a"] || !models["model-b"] {
		t.Fatalf("models %v", models)
	}
	// The record keeps the model across an agent restart.
	e.restart()
	if len(e.m.MacOSHardwareModels()) != 2 {
		t.Fatalf("models after restart %q", e.m.MacOSHardwareModels())
	}
	if err := e.m.DeleteSnapshot(&ownsandv1.DeleteSnapshotCommand{SnapshotId: b}); err != nil {
		t.Fatal(err)
	}
	if got := e.m.MacOSHardwareModels(); len(got) != 1 || string(got[0]) != "model-a" {
		t.Fatalf("models after delete %q", got)
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

func TestLockSnapshotImportSerializesOneIDOnly(t *testing.T) {
	e := newEnv(t)
	idA, _ := uuidv7.NewString()
	idB, _ := uuidv7.NewString()
	unlockA, err := e.m.LockSnapshotImport(context.Background(), idA)
	if err != nil {
		t.Fatal(err)
	}
	// Another ID takes its own lock at once.
	unlockB, err := e.m.LockSnapshotImport(context.Background(), idB)
	if err != nil {
		t.Fatal(err)
	}
	unlockB()
	// The same ID waits for the holder.
	acquired := make(chan func(), 1)
	go func() {
		unlock, err := e.m.LockSnapshotImport(context.Background(), idA)
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		acquired <- unlock
	}()
	select {
	case <-acquired:
		t.Fatal("second import of the same ID did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlockA()
	unlockA() // A second release is a no-op.
	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("waiting import did not acquire the released lock")
	}
	e.m.mu.Lock()
	left := len(e.m.importLocks)
	e.m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d import locks remain after every release", left)
	}
}

func TestLockSnapshotImportWaitEndsWithItsContext(t *testing.T) {
	e := newEnv(t)
	id, _ := uuidv7.NewString()
	unlock, err := e.m.LockSnapshotImport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = e.m.LockSnapshotImport(ctx, id)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
		t.Fatalf("canceled wait = %v", err)
	}
	// The canceled waiter does not break the holder's lock or leak its entry.
	unlock()
	e.m.mu.Lock()
	left := len(e.m.importLocks)
	e.m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d import locks remain after every release", left)
	}
	again, err := e.m.LockSnapshotImport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestLockSnapshotImportCanceledContextAbortsWithFreeLock(t *testing.T) {
	e := newEnv(t)
	id, _ := uuidv7.NewString()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// With a free lock and a canceled context both select cases are ready, so
	// repeat to make a random pick of the lock case practically certain.
	for i := 0; i < 200; i++ {
		unlock, err := e.m.LockSnapshotImport(ctx, id)
		var ce *CommandError
		if !errors.As(err, &ce) || ce.Code != ownsandv1.ErrorCode_ERROR_CODE_ABORTED {
			if unlock != nil {
				unlock()
			}
			t.Fatalf("iteration %d: pre-canceled import with a free lock = %v, want ABORTED", i, err)
		}
		if unlock != nil {
			t.Fatalf("iteration %d: aborted import returned an unlock function", i)
		}
		e.m.mu.Lock()
		left := len(e.m.importLocks)
		e.m.mu.Unlock()
		if left != 0 {
			t.Fatalf("iteration %d: %d import locks remain after the aborted import", i, left)
		}
	}
	// The aborted imports left no token behind: a live context takes the lock.
	again, err := e.m.LockSnapshotImport(context.Background(), id)
	if err != nil {
		t.Fatalf("lock after aborted imports = %v", err)
	}
	again()
	e.m.mu.Lock()
	left := len(e.m.importLocks)
	e.m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d import locks remain after every release", left)
	}
}

// TestIdleReportRetriesAfterRevisionWriteFailure checks that a failed revision
// write keeps the idle-deadline report pending: a later tick publishes the
// current deadline with a newer revision after storage recovers, without
// another activity event, and a successful report still follows the throttle.
func TestIdleReportRetriesAfterRevisionWriteFailure(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	acked := e.out.lastEntry(id)
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: acked.GetRevision()})
	n := e.out.count()

	e.clk.Advance(30 * time.Second)
	e.m.Touch(id)
	wantDeadline := e.clk.Now().Add(10 * time.Minute)

	// A directory in place of agent.json makes the revision write fail: the
	// store cannot read the file that holds the revision counter. The fault
	// does not depend on directory permissions, which neither block file
	// creation on Windows nor apply to root, and the sandbox record directory
	// stays writable, so only the revision persistence fails.
	agentFile := e.st.Path(store.AgentFile)
	savedAgentFile := agentFile + ".saved"
	if err := os.Rename(agentFile, savedAgentFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agentFile, 0o700); err != nil {
		t.Fatal(err)
	}
	// restoreAgentFile puts agent.json back; it is idempotent so that the
	// cleanup also covers a failed assertion before the explicit restore.
	restoreAgentFile := func() {
		if _, err := os.Stat(savedAgentFile); err != nil {
			return
		}
		if err := os.Remove(agentFile); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(savedAgentFile, agentFile); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(restoreAgentFile)
	e.m.Tick(context.Background(), false)
	if e.out.count() != n {
		t.Fatal("idle deadline reported although the revision write failed")
	}
	restoreAgentFile()

	// Storage recovered; no new Touch. The next tick retries.
	e.m.Tick(context.Background(), false)
	if e.out.count() != n+1 {
		t.Fatalf("idle deadline not retried after recovery: %d entries, want %d", e.out.count(), n+1)
	}
	got := e.out.lastEntry(id)
	if got.GetRevision().GetCounter() <= acked.GetRevision().GetCounter() {
		t.Fatalf("revision %d, want newer than %d", got.GetRevision().GetCounter(), acked.GetRevision().GetCounter())
	}
	if !got.GetIdleDeadline().AsTime().Equal(wantDeadline) {
		t.Fatalf("idle deadline %v, want %v", got.GetIdleDeadline().AsTime(), wantDeadline)
	}

	// The successful report starts the one-minute throttle.
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: got.GetRevision()})
	e.m.Touch(id)
	e.m.Tick(context.Background(), false)
	if e.out.count() != n+1 {
		t.Fatal("idle deadline reported again within a minute of a successful report")
	}
	e.clk.Advance(time.Minute)
	e.m.Tick(context.Background(), false)
	if e.out.count() != n+2 {
		t.Fatal("idle deadline not reported after the throttle")
	}
}

// TestIdleReportSkipsAcknowledgedTerminalRecord checks that an idle report
// that Tick collected cannot publish a revision or recreate the record file of
// a sandbox that ended and was acknowledged before the report ran.
func TestIdleReportSkipsAcknowledgedTerminalRecord(t *testing.T) {
	e := newEnv(t)
	id := e.create(10*time.Minute, 2*time.Hour).GetSandboxId()
	e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: e.out.lastEntry(id).GetRevision()})
	e.m.Touch(id)

	e.m.beforeIdleReports = func() {
		e.m.beforeIdleReports = nil
		del, err := e.m.Delete(context.Background(), &ownsandv1.DeleteSandboxCommand{SandboxId: id})
		if err != nil {
			t.Error(err)
			return
		}
		e.m.AckSandboxUpdate(&ownsandv1.SandboxUpdateAck{SandboxId: id, Revision: del.GetRevision()})
	}
	e.m.Tick(context.Background(), false)

	if e.m.beforeIdleReports != nil {
		t.Fatal("the tick did not collect an idle report")
	}
	if _, ok := e.m.State(id); ok {
		t.Fatal("acknowledged terminal record kept")
	}
	if _, err := os.Stat(e.st.Path(store.SandboxesDir, id+".json")); !os.IsNotExist(err) {
		t.Fatalf("record file recreated by a stale idle report: %v", err)
	}
	if got := e.out.lastEntry(id); got.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("last update state %v, want DELETED", got.GetState())
	}
	e.restart()
	if _, ok := e.m.State(id); ok {
		t.Fatal("restarted manager loaded the dropped record")
	}
}
