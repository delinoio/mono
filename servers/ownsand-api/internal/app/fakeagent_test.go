// SPDX-License-Identifier: FSL-1.1-ALv2

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/nonce"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// fakeAgent is an in-process device agent that speaks the real AgentService
// over h2c Connect streams.
type fakeAgent struct {
	t        *testing.T
	h        *harness
	deviceID string
	orgID    string
	priv     ed25519.PrivateKey
	epoch    string
	caps     *ownsandv1.DeviceCapabilities
	client   ownsandv1connect.AgentServiceClient

	mu       sync.Mutex
	stream   *connect.BidiStreamForClient[ownsandv1.AgentEnvelope, ownsandv1.AgentEnvelope]
	cancel   context.CancelFunc
	in       chan *ownsandv1.AgentEnvelope
	closed   chan struct{}
	revision uint64
}

func linuxCaps() *ownsandv1.DeviceCapabilities {
	return &ownsandv1.DeviceCapabilities{
		HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0",
		LogicalCpuCount: 16, MemoryMib: 65536, FreeDiskBytes: 1 << 40,
		GuestOses: []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}, BuiltBaseImages: []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX},
	}
}

// enrollAgent enrolls a new device in the organization through the RPC.
func (h *harness) enrollAgent(orgID, ownerSub string) *fakeAgent {
	h.t.Helper()
	ctx := context.Background()
	tokenValue := secret.New(secret.KindEnrollmentToken)
	_, err := h.store.CreateEnrollmentToken(ctx, orgID, "dev", time.Hour, store.Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: ownerSub},
		secret.Hash(tokenValue), secret.DisplayPrefix(tokenValue), "")
	must(h.t, err)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &fakeAgent{t: h.t, h: h, priv: priv, epoch: "epoch-1", caps: linuxCaps(), client: ownsandv1connect.NewAgentServiceClient(h.client, h.url)}
	resp, err := a.client.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{EnrollmentToken: tokenValue, PublicKey: pub, Capabilities: a.caps}))
	must(h.t, err)
	a.deviceID, a.orgID = resp.Msg.DeviceId, resp.Msg.OrganizationId
	return a
}

// authenticate runs CreateChallenge and Authenticate with the given label.
func (a *fakeAgent) authenticateWith(label string) (*ownsandv1.AuthenticateResponse, []byte, error) {
	ctx := context.Background()
	ch, err := a.client.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: a.deviceID}))
	if err != nil {
		return nil, nil, err
	}
	n := ch.Msg.Nonce
	resp, err := a.client.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId: a.deviceID, Nonce: n, Signature: ed25519.Sign(a.priv, nonce.SignedMessage(label, n)), StateEpoch: a.epoch,
	}))
	if err != nil {
		return nil, n, err
	}
	return resp.Msg, n, nil
}

func (a *fakeAgent) credential() string {
	a.t.Helper()
	resp, _, err := a.authenticateWith(nonce.LabelAuthenticate)
	must(a.t, err)
	cred := resp.GetDeviceSessionCredential()
	if cred == "" {
		a.t.Fatal("no device session credential")
	}
	return cred
}

// connect opens the agent channel, expects the configuration, sends the
// capability and state reports and waits for the state acknowledgement.
func (a *fakeAgent) connect(report *ownsandv1.StateReport) *ownsandv1.AgentConfiguration {
	a.t.Helper()
	stream, err := a.open(a.credential())
	must(a.t, err)
	_ = stream
	cfg := a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetConfiguration() != nil }).GetConfiguration()
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CapabilityReport{CapabilityReport: a.caps}})
	if report == nil {
		report = &ownsandv1.StateReport{}
	}
	if report.ReportId == 0 {
		report.ReportId = 1
	}
	if report.AgentVersion == "" {
		report.AgentVersion = a.caps.AgentVersion
		report.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE
	}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateReport{StateReport: report}})
	id := report.ReportId
	a.expect(func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetStateAck().GetReportId() == id && e.GetStateAck() != nil
	})
	return cfg
}

func (a *fakeAgent) open(cred string) (*connect.BidiStreamForClient[ownsandv1.AgentEnvelope, ownsandv1.AgentEnvelope], error) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := a.client.Channel(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+cred)
	in := make(chan *ownsandv1.AgentEnvelope, 256)
	closed := make(chan struct{})
	a.mu.Lock()
	a.stream, a.cancel, a.in, a.closed = stream, cancel, in, closed
	a.mu.Unlock()
	// The first send flushes the request headers.
	if err := stream.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: 0}}}); err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(closed)
		for {
			env, err := stream.Receive()
			if err != nil {
				return
			}
			in <- env
		}
	}()
	a.t.Cleanup(cancel)
	return stream, nil
}

func (a *fakeAgent) send(env *ownsandv1.AgentEnvelope) {
	a.t.Helper()
	a.mu.Lock()
	stream := a.stream
	a.mu.Unlock()
	must(a.t, stream.Send(env))
}

// expect returns the next received envelope that matches, skipping others.
func (a *fakeAgent) expect(match func(*ownsandv1.AgentEnvelope) bool) *ownsandv1.AgentEnvelope {
	a.t.Helper()
	a.mu.Lock()
	in, closed := a.in, a.closed
	a.mu.Unlock()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case env := <-in:
			if match(env) {
				return env
			}
		case <-closed:
			a.t.Fatal("agent channel closed while waiting")
		case <-timeout:
			a.t.Fatal("timed out waiting for an agent channel message")
		}
	}
}

// waitClosed waits until the server ends the channel.
func (a *fakeAgent) waitClosed(d time.Duration) bool {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	select {
	case <-closed:
		return true
	case <-time.After(d):
		return false
	}
}

func (a *fakeAgent) disconnect() {
	a.mu.Lock()
	cancel, closed := a.cancel, a.closed
	a.mu.Unlock()
	cancel()
	<-closed
}

// command waits for the next command.
func (a *fakeAgent) command() *ownsandv1.AgentCommand {
	a.t.Helper()
	return a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetCommand() != nil }).GetCommand()
}

// report sends a sandbox update with the next revision and waits for its ack.
func (a *fakeAgent) report(sandboxID string, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason) *ownsandv1.SandboxReport {
	a.t.Helper()
	a.revision++
	r := &ownsandv1.SandboxReport{SandboxId: sandboxID, State: state, EndReason: reason, Revision: &ownsandv1.SandboxRevision{Epoch: a.epoch, Counter: a.revision}}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdate{Sandbox: r}}})
	a.expect(func(e *ownsandv1.AgentEnvelope) bool { return e.GetStateAck().GetSandbox().GetSandboxId() == sandboxID })
	return r
}

// result answers a command.
func (a *fakeAgent) result(cmd *ownsandv1.AgentCommand, errorCode string, sb *ownsandv1.SandboxReport) {
	res := &ownsandv1.AgentCommandResult{CommandId: cmd.CommandId, ErrorCode: errorCode}
	if sb != nil {
		res.Result = &ownsandv1.AgentCommandResult_Sandbox{Sandbox: sb}
	}
	a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_CommandResult{CommandResult: res}})
}

func (a *fakeAgent) nextRevision() *ownsandv1.SandboxRevision {
	a.revision++
	return &ownsandv1.SandboxRevision{Epoch: a.epoch, Counter: a.revision}
}
