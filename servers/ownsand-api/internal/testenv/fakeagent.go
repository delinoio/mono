// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// FakeAgent is an in-process device agent on the real agent channel. It
// keeps sandbox records with revisions, answers commands with a default
// behavior that tests can override, and records every envelope it
// receives.
type FakeAgent struct {
	T      testing.TB
	Env    *Env
	Device *Device
	// HostOS, Arch and Capabilities are reported at connect.
	HostOS       ownsandv1.HostOs
	Arch         ownsandv1.Arch
	Capabilities *ownsandv1.DeviceCapabilities
	// OnCommand, when set, answers a command instead of the default
	// behavior; returning nil sends no result.
	OnCommand func(a *FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult
	// OnGrant answers a relay grant; nil accepts it. Returning nil sends no
	// acknowledgement.
	OnGrant func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck
	// OnOffer answers a signaling offer; nil answers with a fixed SDP.
	OnOffer func(o *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer

	ctx    context.Context
	cancel context.CancelFunc
	stream *connect.BidiStreamForClient[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]
	sendMu sync.Mutex

	mu        sync.Mutex
	counter   uint64
	sandboxes map[string]*ownsandv1.SandboxStateEntry
	snapshots map[string]*ownsandv1.SnapshotStateEntry
	received  []*ownsandv1.AgentEnvelope
	changed   chan struct{}
	closed    chan struct{}
	closeErr  error
}

// DefaultLinuxCapabilities reports a Linux host that runs Linux guests.
func DefaultLinuxCapabilities() *ownsandv1.DeviceCapabilities {
	return &ownsandv1.DeviceCapabilities{
		LogicalCpuCount: 8, MemoryMib: 16384, FreeDiskBytes: 200 << 30,
		GuestOses:       []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX},
		BuiltBaseImages: []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX},
	}
}

// NewFakeAgent returns an agent for d that is not connected yet.
func (e *Env) NewFakeAgent(d *Device) *FakeAgent {
	return &FakeAgent{
		T: e.T, Env: e, Device: d,
		HostOS: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, Capabilities: DefaultLinuxCapabilities(),
		sandboxes: map[string]*ownsandv1.SandboxStateEntry{}, snapshots: map[string]*ownsandv1.SnapshotStateEntry{},
		changed: make(chan struct{}),
	}
}

// Connect authenticates, opens the channel, sends the capability report
// and a full state report, and waits until the server applied it.
func (a *FakeAgent) Connect() {
	a.T.Helper()
	if err := a.Env.App.Lease.Wait(context.Background()); err != nil {
		a.T.Fatal(err)
	}
	cred := a.Env.SessionCredential(a.Device)
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.T.Cleanup(a.Close)
	a.stream = a.Env.Clients(cred).Agent.Channel(a.ctx)
	a.closed = make(chan struct{})
	go a.read()
	a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: &ownsandv1.CapabilityReport{
		HostOs: a.HostOS, Arch: a.Arch, AgentVersion: "1.0.0", Capabilities: a.Capabilities,
	}}})
	a.SendStateReport()
	a.Eventually("device online", func() bool { return a.Env.App.Service.Hub.Live(a.Device.ID) != nil })
}

// Close closes the channel.
func (a *FakeAgent) Close() {
	if a.cancel != nil {
		a.cancel()
		a.sendMu.Lock()
		_ = a.stream.CloseRequest()
		a.sendMu.Unlock()
		_ = a.stream.CloseResponse()
		select {
		case <-a.closed:
		case <-time.After(5 * time.Second):
			a.T.Errorf("the fake agent channel did not close")
		}
	}
}

// Closed is closed when the server ends the channel.
func (a *FakeAgent) Closed() <-chan struct{} { return a.closed }

// Send sends one envelope.
func (a *FakeAgent) Send(env *ownsandv1.AgentEnvelope) {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	_ = a.stream.Send(&ownsandv1.ChannelRequest{Envelope: env})
}

// Heartbeat sends one heartbeat.
func (a *FakeAgent) Heartbeat(seq uint64) {
	a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{
		Sequence: seq, UpdateState: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE,
	}}})
}

// SendStateReport sends the agent's records.
func (a *FakeAgent) SendStateReport() {
	a.mu.Lock()
	report := &ownsandv1.StateReport{ReportSequence: a.counter, StateEpoch: a.Device.Epoch}
	for _, e := range a.sandboxes {
		report.Sandboxes = append(report.Sandboxes, proto.Clone(e).(*ownsandv1.SandboxStateEntry))
	}
	for _, s := range a.snapshots {
		report.Snapshots = append(report.Snapshots, proto.Clone(s).(*ownsandv1.SnapshotStateEntry))
	}
	report.BuiltBaseImages = a.Capabilities.GetBuiltBaseImages()
	a.mu.Unlock()
	a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: report}})
}

func (a *FakeAgent) read() {
	defer close(a.closed)
	for {
		resp, err := a.stream.Receive()
		if err != nil {
			a.mu.Lock()
			a.closeErr = err
			a.mu.Unlock()
			return
		}
		env := resp.GetEnvelope()
		a.mu.Lock()
		a.received = append(a.received, env)
		ch := a.changed
		a.changed = make(chan struct{})
		a.mu.Unlock()
		close(ch)
		if cmd := env.GetCommand(); cmd != nil {
			go a.answer(cmd)
		}
		if g := env.GetRelayGrant(); g != nil {
			go a.ackGrant(g)
		}
		if o := env.GetSignaling().GetOffer(); o != nil {
			go a.answerOffer(o)
		}
		if r := env.GetSessionRevocation(); r != nil {
			go a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: r.GetRevocationId()}}})
		}
	}
}

// SetOnGrant replaces the relay grant hook while the agent runs.
func (a *FakeAgent) SetOnGrant(fn func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck) {
	a.mu.Lock()
	a.OnGrant = fn
	a.mu.Unlock()
}

// SetOnOffer replaces the signaling hook while the agent runs.
func (a *FakeAgent) SetOnOffer(fn func(o *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer) {
	a.mu.Lock()
	a.OnOffer = fn
	a.mu.Unlock()
}

// SetOnCommand replaces the command hook while the agent runs.
func (a *FakeAgent) SetOnCommand(fn func(a *FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult) {
	a.mu.Lock()
	a.OnCommand = fn
	a.mu.Unlock()
}

func (a *FakeAgent) ackGrant(g *ownsandv1.RelayGrant) {
	ack := &ownsandv1.RelayGrantAck{GrantId: g.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED, ReplacedGrantId: g.GetReplacesGrantId()}
	a.mu.Lock()
	hook := a.OnGrant
	a.mu.Unlock()
	if hook != nil {
		ack = hook(g)
	}
	if ack != nil {
		a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: ack}})
	}
}

func (a *FakeAgent) answerOffer(o *ownsandv1.SignalingOffer) {
	ans := &ownsandv1.SignalingAnswer{SessionId: o.GetSessionId(), Sdp: "v=0 answer"}
	a.mu.Lock()
	hook := a.OnOffer
	a.mu.Unlock()
	if hook != nil {
		ans = hook(o)
	}
	if ans != nil {
		a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Signaling{Signaling: &ownsandv1.SignalingMessage{
			Message: &ownsandv1.SignalingMessage_Answer{Answer: ans},
		}}})
	}
}

// SetSandbox stores a sandbox record with the next revision and returns
// the entry.
func (a *FakeAgent) SetSandbox(id string, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason) *ownsandv1.SandboxStateEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.counter++
	e := &ownsandv1.SandboxStateEntry{
		SandboxId: id, State: state, EndReason: reason,
		Revision:     &ownsandv1.SandboxRevision{Epoch: a.Device.Epoch, Counter: a.counter},
		IdleDeadline: timestamppb.New(time.Now().Add(time.Hour)),
	}
	a.sandboxes[id] = e
	return proto.Clone(e).(*ownsandv1.SandboxStateEntry)
}

// ForgetSandbox removes a sandbox record, as after a lost state directory.
func (a *FakeAgent) ForgetSandbox(id string) {
	a.mu.Lock()
	delete(a.sandboxes, id)
	a.mu.Unlock()
}

// Sandbox returns the agent's record of a sandbox.
func (a *FakeAgent) Sandbox(id string) *ownsandv1.SandboxStateEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.sandboxes[id]; e != nil {
		return proto.Clone(e).(*ownsandv1.SandboxStateEntry)
	}
	return nil
}

// SendUpdate sends a sandbox update for the agent's current record.
func (a *FakeAgent) SendUpdate(e *ownsandv1.SandboxStateEntry) {
	a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: e}}})
}

func (a *FakeAgent) answer(cmd *ownsandv1.AgentCommand) {
	var res *ownsandv1.AgentCommandResult
	a.mu.Lock()
	hook := a.OnCommand
	a.mu.Unlock()
	if hook != nil {
		res = hook(a, cmd)
	} else {
		res = a.DefaultAnswer(cmd)
	}
	if res != nil {
		res.CommandId = cmd.GetCommandId()
		a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
	}
}

// DefaultAnswer runs a command on the fake records: creates run, pauses
// pause, resumes run, deletes delete, snapshots become ready, exports
// upload with signed URLs and imports succeed.
func (a *FakeAgent) DefaultAnswer(cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
	switch c := cmd.GetCommand().(type) {
	case *ownsandv1.AgentCommand_CreateSandbox:
		e := a.SetSandbox(c.CreateSandbox.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
		e.RestorePath = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_CreateSandbox{CreateSandbox: &ownsandv1.CreateSandboxResult{Sandbox: e}}}
	case *ownsandv1.AgentCommand_PauseSandbox:
		e := a.SetSandbox(c.PauseSandbox.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, 0)
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: e}}}
	case *ownsandv1.AgentCommand_ResumeSandbox:
		e := a.SetSandbox(c.ResumeSandbox.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: e}}}
	case *ownsandv1.AgentCommand_DeleteSandbox:
		e := a.SetSandbox(c.DeleteSandbox.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_DELETED, c.DeleteSandbox.GetEndReason())
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_Sandbox{Sandbox: &ownsandv1.SandboxCommandResult{Sandbox: e}}}
	case *ownsandv1.AgentCommand_CreateSnapshot:
		entry := &ownsandv1.SnapshotStateEntry{SnapshotId: c.CreateSnapshot.GetSnapshotId(), SourceSandboxId: c.CreateSnapshot.GetSandboxId(),
			State: ownsandv1.SnapshotState_SNAPSHOT_STATE_READY, GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX, HasMemoryState: true, SizeBytes: 3 << 20}
		a.mu.Lock()
		a.snapshots[entry.SnapshotId] = entry
		a.mu.Unlock()
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotResult{Snapshot: entry}}}
	case *ownsandv1.AgentCommand_ExportSnapshot:
		reqID := cmd.GetCommandId()
		a.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlRequest{SignedUrlRequest: &ownsandv1.SignedUrlRequest{
			RequestId: reqID, SnapshotId: c.ExportSnapshot.GetSnapshotId(), Operation: ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD, PartNumbers: []int32{1, 2},
		}}})
		grant := a.WaitFor("signed url grant", func(env *ownsandv1.AgentEnvelope) bool { return env.GetSignedUrlGrant().GetRequestId() == reqID }).GetSignedUrlGrant()
		if grant.GetError() != nil {
			return &ownsandv1.AgentCommandResult{Error: grant.GetError()}
		}
		var parts []*ownsandv1.SnapshotPart
		for _, u := range grant.GetPartUrls() {
			sum := sha256.Sum256([]byte(u.GetUrl()))
			parts = append(parts, &ownsandv1.SnapshotPart{PartNumber: u.GetPartNumber(), SizeBytes: 1 << 20, Sha256: sum[:]})
		}
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotResult{Parts: parts, SizeBytes: 2 << 20}}}
	case *ownsandv1.AgentCommand_ImportSnapshot:
		entry := proto.Clone(c.ImportSnapshot.GetSnapshot()).(*ownsandv1.SnapshotStateEntry)
		a.mu.Lock()
		a.snapshots[entry.SnapshotId] = entry
		a.mu.Unlock()
		return &ownsandv1.AgentCommandResult{Result: &ownsandv1.AgentCommandResult_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotResult{Snapshot: entry}}}
	case *ownsandv1.AgentCommand_CleanUp, *ownsandv1.AgentCommand_UpdateAgent:
		return &ownsandv1.AgentCommandResult{}
	}
	return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "unknown command"}}
}

// Received returns the envelopes received so far.
func (a *FakeAgent) Received() []*ownsandv1.AgentEnvelope {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*ownsandv1.AgentEnvelope(nil), a.received...)
}

// WaitFor waits up to 10 seconds for a received envelope that matches and
// returns it.
func (a *FakeAgent) WaitFor(what string, match func(*ownsandv1.AgentEnvelope) bool) *ownsandv1.AgentEnvelope {
	a.T.Helper()
	deadline := time.After(10 * time.Second)
	seen := 0
	for {
		a.mu.Lock()
		envs := a.received[seen:]
		ch := a.changed
		a.mu.Unlock()
		for _, env := range envs {
			seen++
			if match(env) {
				return env
			}
		}
		select {
		case <-ch:
		case <-deadline:
			a.T.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}
}

// Commands returns the commands received so far.
func (a *FakeAgent) Commands() []*ownsandv1.AgentCommand {
	var out []*ownsandv1.AgentCommand
	for _, env := range a.Received() {
		if c := env.GetCommand(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// Eventually polls cond for up to 10 seconds.
func (a *FakeAgent) Eventually(what string, cond func() bool) {
	a.T.Helper()
	Eventually(a.T, what, cond)
}

// Eventually polls cond every 10 ms for up to 10 seconds.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
