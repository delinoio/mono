package sandbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime/runtimetest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type recorder struct {
	mu      sync.Mutex
	updates []*ownsandv1.SandboxReport
	snaps   []*ownsandv1.SnapshotUpdate
}

func (r *recorder) SandboxUpdate(u *ownsandv1.SandboxReport) {
	r.mu.Lock()
	r.updates = append(r.updates, u)
	r.mu.Unlock()
}

func (r *recorder) SnapshotUpdate(u *ownsandv1.SnapshotUpdate) {
	r.mu.Lock()
	r.snaps = append(r.snaps, u)
	r.mu.Unlock()
}

func (r *recorder) last(id string) *ownsandv1.SandboxReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.updates) - 1; i >= 0; i-- {
		if r.updates[i].GetSandboxId() == id {
			return r.updates[i]
		}
	}
	return nil
}

type gate struct {
	mu  sync.Mutex
	err error
}

func (g *gate) BeginCreate() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return nil, g.err
	}
	return func() {}, nil
}

type env struct {
	t     *testing.T
	m     *Manager
	rt    *runtimetest.Runtime
	clock *clock
	rec   *recorder
	gate  *gate
	dir   *state.Dir
}

func newEnv(t *testing.T, dir *state.Dir, rt *runtimetest.Runtime) *env {
	t.Helper()
	if dir == nil {
		var err error
		dir, err = state.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
	}
	if rt == nil {
		rt = runtimetest.New(t.TempDir())
	}
	e := &env{t: t, rt: rt, clock: &clock{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}, rec: &recorder{}, gate: &gate{}, dir: dir}
	m, err := New(context.Background(), Config{
		Runtime:  rt,
		Dir:      dir,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      e.clock.Now,
		Reporter: e.rec,
		Gate:     e.gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLimits(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100})
	e.m = m
	return e
}

func (e *env) do(cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
	e.t.Helper()
	ch := make(chan *ownsandv1.AgentCommandResult, 1)
	if !e.m.Handle(cmd, func(r *ownsandv1.AgentCommandResult) { ch <- r }) {
		e.t.Fatal("command not handled")
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		e.t.Fatal("no command result")
		return nil
	}
}

func (e *env) createCmd(id string, cpu int64) *ownsandv1.AgentCommand {
	now := e.clock.Now()
	return &ownsandv1.AgentCommand{CommandId: datapath.NewID(), Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxCommand{
		SandboxId:    id,
		Source:       &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
		Resources:    &ownsandv1.Resources{VcpuCount: cpu, MemoryMib: 1024, DiskGib: 10},
		NetworkMode:  ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleDeadline: timestamppb.New(now.Add(10 * time.Minute)),
		MaxDeadline:  timestamppb.New(now.Add(time.Hour)),
		IdleTtl:      durationpb.New(10 * time.Minute),
		Limits:       &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100},
	}}}
}

func deleteCmd(id string) *ownsandv1.AgentCommand {
	return &ownsandv1.AgentCommand{CommandId: datapath.NewID(), Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{
		SandboxId: id, Reason: ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER,
	}}}
}

func (e *env) waitState(id string, want ownsandv1.SandboxState) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.m.SandboxState(id) == want || (e.m.Report(id) != nil && e.m.Report(id).GetState() == want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("sandbox %s did not reach %v (now %v)", id, want, e.m.SandboxState(id))
}

func TestCreateRunsVMAndWaitsForGuest(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	res := e.do(e.createCmd(id, 2))
	if res.GetErrorCode() != "" {
		t.Fatalf("create failed: %s %s", res.GetErrorCode(), res.GetErrorMessage())
	}
	if res.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || res.GetSandbox().GetRestorePath() != ownsandv1.RestorePath_RESTORE_PATH_BASE_IMAGE {
		t.Fatalf("unexpected report %v", res.GetSandbox())
	}
	if !e.rt.WaitClockSets(1, 5*time.Second) {
		t.Fatal("guest clock was not set")
	}
	if got := e.rec.last(id); got.GetRevision().GetCounter() != 2 || got.GetRevision().GetEpoch() != e.m.Epoch() {
		t.Fatalf("unexpected revision %v", got.GetRevision())
	}
	// A retry of the same create returns the current state without a new VM.
	res = e.do(e.createCmd(id, 2))
	if res.GetErrorCode() != "" || len(e.rt.CallLog()) != 2 {
		t.Fatalf("retry created again: %v %v", res, e.rt.CallLog())
	}
}

func TestCreateChecksLimits(t *testing.T) {
	e := newEnv(t, nil, nil)
	if r := e.do(e.createCmd(datapath.NewID(), 16)); r.GetErrorCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition above the limits, got %q", r.GetErrorCode())
	}
	if r := e.do(e.createCmd(datapath.NewID(), 6)); r.GetErrorCode() != "" {
		t.Fatalf("first create failed: %v", r)
	}
	if r := e.do(e.createCmd(datapath.NewID(), 4)); r.GetErrorCode() != "resource_exhausted" {
		t.Fatalf("want resource_exhausted above the free capacity, got %q", r.GetErrorCode())
	}
	// The stricter of the configuration and the command limits applies.
	e.m.SetLimits(&ownsandv1.DeviceLimits{VcpuCount: 1, MemoryMib: 8192, DiskGib: 100})
	if r := e.do(e.createCmd(datapath.NewID(), 2)); r.GetErrorCode() != "failed_precondition" {
		t.Fatalf("want failed_precondition with stricter configuration limits, got %q", r.GetErrorCode())
	}
	e.gate.err = errors.New("installing")
	e.m.SetLimits(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 8192, DiskGib: 100})
	if r := e.do(e.createCmd(datapath.NewID(), 1)); r.GetErrorCode() == "" {
		t.Fatal("create accepted while the update gate is closed")
	}
}

func TestDeleteBeforeCreateLeavesMarker(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	if r := e.do(deleteCmd(id)); r.GetErrorCode() != "" {
		t.Fatalf("delete failed: %v", r)
	}
	if got := e.rec.last(id); got.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatalf("marker not reported as deleted: %v", got)
	}
	if r := e.do(e.createCmd(id, 1)); r.GetErrorCode() != "failed_precondition" {
		t.Fatalf("want the create rejected after a delete marker, got %q", r.GetErrorCode())
	}
	if e.rt.HasVM(id) {
		t.Fatal("a VM exists for a deleted sandbox")
	}
}

func TestDeleteCancelsRunningCreate(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.rt.StartGate = make(chan struct{})
	id := datapath.NewID()
	results := make(chan *ownsandv1.AgentCommandResult, 2)
	e.m.Handle(e.createCmd(id, 1), func(r *ownsandv1.AgentCommandResult) { results <- r })
	e.waitState(id, ownsandv1.SandboxState_SANDBOX_STATE_CREATING)
	e.m.Handle(deleteCmd(id), func(r *ownsandv1.AgentCommandResult) { results <- r })
	first, second := <-results, <-results
	if first.GetErrorCode() != "aborted" || second.GetErrorCode() != "" {
		t.Fatalf("unexpected results %v / %v", first, second)
	}
	if e.m.Report(id).GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || e.rt.HasVM(id) {
		t.Fatal("sandbox not deleted after the cancelled create")
	}
}

func TestTTLReason(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name       string
		idle, max  time.Time
		running    bool
		wantReason ownsandv1.SandboxEndReason
	}{
		{"none", now.Add(time.Second), now.Add(time.Hour), true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED},
		{"idle", now.Add(-time.Second), now.Add(time.Hour), true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED},
		{"idle while paused", now.Add(-time.Second), now.Add(time.Hour), false, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED},
		{"max", now.Add(time.Hour), now, true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED},
		{"max while paused", now.Add(-time.Hour), now.Add(-time.Second), false, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED},
		{"both, idle earlier", now.Add(-time.Hour), now.Add(-time.Second), true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED},
		{"both, max earlier", now.Add(-time.Second), now.Add(-time.Hour), true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED},
		{"both equal", now.Add(-time.Second), now.Add(-time.Second), true, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED},
	}
	for _, c := range cases {
		if got := TTLReason(now, c.idle, c.max, c.running); got != c.wantReason {
			t.Errorf("%s: got %v, want %v", c.name, got, c.wantReason)
		}
	}
}

func TestIdleTTLExpiresAndActivityMovesDeadline(t *testing.T) {
	e := newEnv(t, nil, nil)
	busy, idle := datapath.NewID(), datapath.NewID()
	e.do(e.createCmd(busy, 1))
	e.do(e.createCmd(idle, 1))
	end := e.m.BeginActivity(busy)
	e.clock.Advance(11 * time.Minute)
	e.m.CheckTTL()
	e.waitState(idle, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := e.m.Report(idle); r.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
	if e.m.SandboxState(busy) != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatal("a sandbox with a running exec expired")
	}
	end()
	if got := e.m.Report(busy).GetIdleDeadline().AsTime(); !got.Equal(e.clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("idle deadline %v not moved", got)
	}
	// A running exec keeps the sandbox active until max_ttl ends it.
	defer e.m.BeginActivity(busy)()
	e.clock.Advance(time.Hour)
	e.m.CheckTTL()
	e.waitState(busy, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if r := e.m.Report(busy); r.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("end reason %v", r.GetEndReason())
	}
}

func TestPausedSandboxIdleClockStopsAndResumeSetsClock(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	e.do(e.createCmd(id, 1))
	pause := &ownsandv1.AgentCommand{CommandId: "p", Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: id}}}
	if r := e.do(pause); r.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatalf("pause: %v", r)
	}
	if r := e.do(pause); r.GetErrorCode() != "" {
		t.Fatalf("pause of a paused sandbox must succeed: %v", r)
	}
	e.clock.Advance(30 * time.Minute)
	e.m.CheckTTL()
	if e.m.SandboxState(id) != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		t.Fatal("idle TTL ran while paused")
	}
	e.rt.WaitClockSets(1, 5*time.Second)
	clocks := e.rt.ClockSetCount()
	resume := &ownsandv1.AgentCommand{CommandId: "r", Command: &ownsandv1.AgentCommand_ResumeSandbox{ResumeSandbox: &ownsandv1.ResumeSandboxCommand{SandboxId: id}}}
	if r := e.do(resume); r.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("resume: %v", r)
	}
	if !e.rt.WaitClockSets(clocks+1, 5*time.Second) {
		t.Fatal("guest clock not set after resume")
	}
}

func TestRestartEnforcesTTLAndReportsLostVMs(t *testing.T) {
	dir, _ := state.Open(t.TempDir())
	rt := runtimetest.New(t.TempDir())
	e := newEnv(t, dir, rt)
	expired, lost := datapath.NewID(), datapath.NewID()
	e.do(e.createCmd(expired, 1))
	e.do(e.createCmd(lost, 1))
	epoch := e.m.Epoch()
	// The agent stops; its VMs stop with it.
	rt.SimulateLoss(expired)
	rt.SimulateLoss(lost)
	// A VM without a record (lost records) must be deleted too.
	stray := datapath.NewID()
	rt.AddRunningVM(stray)

	e2 := newEnv(t, dir, rt)
	if e2.m.Epoch() != epoch {
		t.Fatal("the epoch changed although the records were kept")
	}
	// Move the max deadline of one sandbox into the past.
	e2.m.mu.Lock()
	e2.m.recs.Sandboxes[expired].MaxDeadline = e2.clock.Now().Add(-time.Minute)
	e2.m.mu.Unlock()
	if err := e2.m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := e2.m.Report(expired); r.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || r.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED {
		t.Fatalf("TTL reason must win over LOST_ON_DEVICE: %v", r)
	}
	if r := e2.m.Report(lost); r.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || r.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("want LOST_ON_DEVICE: %v", r)
	}
	if rt.HasVM(stray) {
		t.Fatal("a VM without a record survived recovery")
	}
	// The state report keeps the ended records until it is acknowledged.
	rep := e2.m.StateReport()
	if len(rep.GetSandboxes()) != 2 {
		t.Fatalf("state report has %d sandboxes", len(rep.GetSandboxes()))
	}
	e2.m.AckReport(rep.GetReportId())
	if len(e2.m.StateReport().GetSandboxes()) != 0 || len(e2.m.PendingUpdates()) != 0 {
		t.Fatal("acknowledged ended records were not dropped")
	}
}

func TestLostRecordsStartNewEpoch(t *testing.T) {
	dir, _ := state.Open(t.TempDir())
	e := newEnv(t, dir, nil)
	epoch := e.m.Epoch()
	if err := dir.SaveRecords(&state.Records{}); err != nil {
		t.Fatal(err)
	}
	e2 := newEnv(t, dir, nil)
	if e2.m.Epoch() == epoch || e2.m.Epoch() == "" {
		t.Fatal("a lost records file must start a new epoch")
	}
}

func TestLostVMWhileRunning(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	e.do(e.createCmd(id, 1))
	e.rt.SimulateLoss(id)
	e.m.CheckLost(context.Background())
	if r := e.m.Report(id); r.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED || r.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("want LOST_ON_DEVICE, got %v", r)
	}
}

func TestSandboxAckStopsResendAndDropsEndedRecord(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	e.do(e.createCmd(id, 1))
	if len(e.m.PendingUpdates()) != 1 {
		t.Fatal("the update is not pending before its acknowledgement")
	}
	r := e.m.Report(id)
	e.m.AckSandbox(id, &ownsandv1.SandboxRevision{Epoch: "other", Counter: r.GetRevision().GetCounter()})
	if len(e.m.PendingUpdates()) != 1 {
		t.Fatal("an acknowledgement of another epoch cleared the update")
	}
	e.m.AckSandbox(id, r.GetRevision())
	if len(e.m.PendingUpdates()) != 0 {
		t.Fatal("the acknowledged update is still pending")
	}
	e.do(deleteCmd(id))
	r = e.m.Report(id)
	e.m.AckSandbox(id, r.GetRevision())
	if e.m.Report(id) != nil {
		t.Fatal("the acknowledged terminal record was kept")
	}
	// The delete command stays idempotent after the record is gone: it
	// leaves a marker and succeeds.
	if res := e.do(deleteCmd(id)); res.GetErrorCode() != "" {
		t.Fatalf("repeated delete failed: %v", res)
	}
}

func TestSnapshotAndCreateFromSnapshot(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := datapath.NewID()
	e.do(e.createCmd(id, 1))
	snapID := datapath.NewID()
	res := e.do(&ownsandv1.AgentCommand{CommandId: "s", Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{SandboxId: id, SnapshotId: snapID}}})
	if res.GetErrorCode() != "" || res.GetSnapshot().GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("snapshot: %v", res)
	}
	cmd := e.createCmd(datapath.NewID(), 1)
	cmd.GetCreateSandbox().Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}
	cmd.GetCreateSandbox().TryMemoryRestore = true
	res = e.do(cmd)
	if res.GetSandbox().GetRestorePath() != ownsandv1.RestorePath_RESTORE_PATH_MEMORY_STATE {
		t.Fatalf("want a memory restore, got %v", res.GetSandbox())
	}
	cmd = e.createCmd(datapath.NewID(), 2)
	cmd.GetCreateSandbox().Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}
	cmd.GetCreateSandbox().TryMemoryRestore = true
	res = e.do(cmd)
	if res.GetSandbox().GetRestorePath() != ownsandv1.RestorePath_RESTORE_PATH_COLD_BOOT {
		t.Fatalf("another vCPU count must boot cold, got %v", res.GetSandbox())
	}
	exp := e.do(&ownsandv1.AgentCommand{CommandId: "x", Command: &ownsandv1.AgentCommand_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotCommand{SnapshotId: snapID}}})
	if exp.GetErrorCode() != "unimplemented" {
		t.Fatalf("export must be unimplemented, got %v", exp)
	}
}
