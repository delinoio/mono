// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// runningSandbox sets up an organization, a connected agent and a running
// sandbox.
func runningSandbox(t *testing.T, env *testenv.Env) (*testenv.Org, *testenv.FakeAgent, string) {
	t.Helper()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, id, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return org, agent, id
}

func newExec(orgID, sandboxID string) *ownsandv1.ExecRequest {
	execID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	return &ownsandv1.ExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: execID, Command: []string{"/bin/echo", "secret-command"},
		Env: map[string]string{"TOKEN": "secret-env"}, BindingSalt: salt}
}

func authorizeExec(t *testing.T, c *testenv.Clients, req *ownsandv1.ExecRequest) (*ownsandv1.AuthorizeRelayResponse, error) {
	t.Helper()
	authID, _ := uuidv7.NewString()
	resp, err := c.DataPath.AuthorizeRelay(context.Background(), connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
		OrganizationId: req.GetOrganizationId(), SandboxId: req.GetSandboxId(), OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func grantHeader(grantID string) http.Header {
	h := http.Header{}
	h.Set(api.RelayGrantHeader, grantID)
	return h
}

// serveExec answers an exec request like an agent: accepted, started, one
// stdout chunk and the exit code.
func serveExec(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
	s.Send(&ownsandv1.RequestAccepted{})
	execID := f.GetExecRequest().GetExecId()
	s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started{Started: &ownsandv1.ExecStarted{ExecId: execID}}})
	s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 0, Data: []byte("secret-output\n")}}})
	s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}}})
}

func collectExec(t *testing.T, stream *connect.ServerStreamForClient[ownsandv1.ExecResponse]) ([]*ownsandv1.ExecEvent, error) {
	t.Helper()
	var out []*ownsandv1.ExecEvent
	for stream.Receive() {
		out = append(out, stream.Msg().GetEvent())
	}
	return out, stream.Err()
}

func errDetail(err error) *ownsandv1.ErrorDetail {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil
	}
	for _, d := range ce.Details() {
		if v, err := d.Value(); err == nil {
			if detail, ok := v.(*ownsandv1.ErrorDetail); ok {
				return detail
			}
		}
	}
	return nil
}

func TestGetIceServersAndOpenSession(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	key := env.Clients(org.APIKey)
	ice, err := key.DataPath.GetIceServers(ctx, connect.NewRequest(&ownsandv1.GetIceServersRequest{OrganizationId: org.ID}))
	if err != nil || len(ice.Msg.GetIceServers()) != 1 || ice.Msg.GetIceServers()[0].GetUrls()[0] != "stun:127.0.0.1:3478" {
		t.Fatalf("ice servers: %v %v", ice, err)
	}
	resp, err := key.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0 secret-offer"}))
	if err != nil || resp.Msg.GetSdpAnswer() != "v=0 answer" || resp.Msg.GetExpiresIn().AsDuration() > time.Hour {
		t.Fatalf("open session: %v %v", resp, err)
	}
	offer := agent.WaitFor("offer", func(e *ownsandv1.AgentEnvelope) bool { return e.GetSignaling().GetOffer() != nil }).GetSignaling().GetOffer()
	if offer.GetPrincipal().GetKind() != ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY || offer.GetSandboxId() != sandboxID || offer.GetSessionId() != resp.Msg.GetSessionId() {
		t.Fatalf("offer = %v", offer)
	}
	if strings.Contains(env.Logs.String(), "secret-offer") {
		t.Fatal("the SDP offer appears in the logs")
	}
	// The agent's error answer becomes the RPC error.
	agent.SetOnOffer(func(o *ownsandv1.SignalingOffer) *ownsandv1.SignalingAnswer {
		return &ownsandv1.SignalingAnswer{SessionId: o.GetSessionId(), Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: "too many sessions"}}
	})
	if _, err := key.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0"})); testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("agent error: %v", err)
	}
	// Revoking the API key revokes its open session on the agent.
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("session revocation", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetSessionId() == resp.Msg.GetSessionId()
	})
	testenv.Eventually(t, "revocation acknowledged", func() bool { return env.App.Plane.PendingRevocations() == 0 })
	// A paused sandbox gets failed_precondition.
	if _, err := org.OwnerClient.Sandbox.PauseSandbox(ctx, connect.NewRequest(&ownsandv1.PauseSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0"})); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("paused sandbox: %v", err)
	}
}

func TestRelayExecReadWrite(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		switch {
		case f.GetExecRequest() != nil:
			serveExec(s, f)
		case f.GetFileReadRequest() != nil:
			s.Send(&ownsandv1.RequestAccepted{})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 0, Data: []byte("secret-file")}}})
			s.Send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: 11}}})
		case f.GetFileWriteChunk() != nil:
			w := f.GetFileWriteChunk()
			s.Send(&ownsandv1.WriteFileResponse{WriteId: w.GetWriteId(), Offset: w.GetOffset(), Length: int64(len(w.GetData()))})
		}
	})
	r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

	key := env.Clients(org.APIKey)
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, key, req)
	if err != nil {
		t.Fatal(err)
	}
	g := agent.WaitFor("relay grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == grant.GetGrantId() }).GetRelayGrant()
	if !relaybinding.Equal(g.GetRequestDigest(), relaybinding.ExecDigest(req)) || g.GetStreamDeadlineIn().AsDuration() != 30*time.Minute || g.GetExpiresIn().AsDuration() != 15*time.Minute {
		t.Fatalf("grant = %v", g)
	}
	relayClient := env.ClientsWith(org.APIKey, grantHeader(grant.GetGrantId())).Relay
	stream, err := relayClient.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectExec(t, stream)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		switch e.GetEvent().(type) {
		case *ownsandv1.ExecEvent_Accepted:
			kinds = append(kinds, "accepted")
		case *ownsandv1.ExecEvent_Started:
			kinds = append(kinds, "started")
		case *ownsandv1.ExecEvent_Stdout:
			kinds = append(kinds, "stdout:"+string(e.GetStdout().GetData()))
		case *ownsandv1.ExecEvent_Exited:
			kinds = append(kinds, "exited")
		case *ownsandv1.ExecEvent_Keepalive:
		}
	}
	if strings.Join(kinds, ",") != "accepted,started,stdout:secret-output\n,exited" {
		t.Fatalf("events = %v", kinds)
	}
	open := r.WaitFrame("stream open", func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	if open.GetStreamId() != 1 || open.GetRelayStreamOpen().GetGrantId() != grant.GetGrantId() || open.GetRelayStreamOpen().GetInitialWindowBytes() < 1<<20+64<<10 {
		t.Fatalf("stream open = %v", open)
	}
	r.WaitFrame("window update toward the agent", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == 1 && f.GetRelayWindowUpdate() != nil })
	r.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == 1 && f.GetRelayStreamClose() != nil })

	// AttachExec under the EXEC grant uses a new stream_id.
	attach, err := relayClient.AttachExec(ctx, connect.NewRequest(&ownsandv1.AttachExecRequest{OrganizationId: org.ID, SandboxId: sandboxID, ExecId: req.GetExecId()}))
	if err != nil {
		t.Fatal(err)
	}
	attachFrame := r.WaitFrame("attach request", func(f *ownsandv1.DataFrame) bool { return f.GetExecAttachRequest() != nil })
	if attachFrame.GetStreamId() != 2 {
		t.Fatalf("attach stream_id = %d", attachFrame.GetStreamId())
	}
	r.SendFrame(attachEnd(attachFrame))
	for attach.Receive() {
	}
	if testenv.Code(attach.Err()) != connect.CodeNotFound {
		t.Fatalf("attach result: %v", attach.Err())
	}

	// ReadFile and WriteFile.
	readID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	read := &ownsandv1.ReadFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, ReadId: readID, Path: "/etc/secret-path", BindingSalt: salt}
	rg := authorize(t, key, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, readID, relaybinding.ReadFileDigest(read))
	rs, err := env.ClientsWith(org.APIKey, grantHeader(rg)).Relay.ReadFile(ctx, connect.NewRequest(read))
	if err != nil {
		t.Fatal(err)
	}
	var data string
	var size int64
	for rs.Receive() {
		data += string(rs.Msg().GetEvent().GetChunk().GetData())
		if end := rs.Msg().GetEvent().GetEnd(); end != nil {
			size = end.GetSizeBytes()
		}
	}
	if rs.Err() != nil || data != "secret-file" || size != 11 {
		t.Fatalf("read: %q %d %v", data, size, rs.Err())
	}
	writeID, _ := uuidv7.NewString()
	write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: []byte("secret-chunk")}
	wg := authorize(t, key, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(write))
	wr, err := env.ClientsWith(org.APIKey, grantHeader(wg)).Relay.WriteFile(ctx, connect.NewRequest(write))
	if err != nil || wr.Msg.GetLength() != 12 {
		t.Fatalf("write: %v %v", wr, err)
	}
	// Forwarding without persistence or logging of content.
	logs := env.Logs.String()
	for _, secret := range []string{"secret-command", "secret-env", "secret-output", "secret-file", "secret-chunk", "secret-path"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("%q appears in the logs", secret)
		}
	}
	if !strings.Contains(logs, `"msg":"relay_rpc"`) {
		t.Fatal("missing relay RPC log record")
	}
	// The agent releases a grant: it no longer counts and the RPC fails.
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantRelease{RelayGrantRelease: &ownsandv1.RelayGrantRelease{GrantId: wg}}})
	testenv.Eventually(t, "grant released", func() bool { return env.App.Plane.Grants.Get(wg) == nil })
	if _, err := env.ClientsWith(org.APIKey, grantHeader(wg)).Relay.WriteFile(ctx, connect.NewRequest(write)); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("released grant: %v", err)
	}
}

func attachEnd(f *ownsandv1.DataFrame) *ownsandv1.DataFrame {
	return &ownsandv1.DataFrame{StreamId: f.GetStreamId(), SandboxId: f.GetSandboxId(), Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR,
		Payload: &ownsandv1.DataFrame_Error{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, Message: "exec not found"}}}
}

func authorize(t *testing.T, c *testenv.Clients, orgID, sandboxID string, kind ownsandv1.RelayOperationKind, opID string, digest []byte) string {
	t.Helper()
	authID, _ := uuidv7.NewString()
	resp, err := c.DataPath.AuthorizeRelay(context.Background(), connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{
		OrganizationId: orgID, SandboxId: sandboxID, OperationKind: kind, OperationId: opID, RequestDigest: digest, AuthorizationId: authID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetGrantId()
}

func TestRelayAuthentication(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	agent.ConnectRelay(serveExec)
	key := env.Clients(org.APIKey)
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, key, req)
	if err != nil {
		t.Fatal(err)
	}
	execWith := func(token, grantID string, r *ownsandv1.ExecRequest) error {
		stream, err := env.ClientsWith(token, grantHeader(grantID)).Relay.Exec(ctx, connect.NewRequest(r))
		if err != nil {
			return err
		}
		_, err = collectExec(t, stream)
		return err
	}
	unknown, _ := uuidv7.NewString()
	err = execWith(org.APIKey, unknown, req)
	if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetSessionEndReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("unknown grant: %v %v", err, errDetail(err))
	}
	if err := execWith(testenv.UserToken(org.Member), grant.GetGrantId(), req); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("other principal: %v", err)
	}
	other := newExec(org.ID, sandboxID)
	if err := execWith(org.APIKey, grant.GetGrantId(), other); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("other operation: %v", err)
	}
	// Revoking the API key revokes the grant on the agent and the key no
	// longer authenticates.
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("grant revocation", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetRelayGrantId() == grant.GetGrantId()
	})
	if err := execWith(org.APIKey, grant.GetGrantId(), req); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	// A member's grant ends when the membership ends.
	member := env.User(org.Member)
	mreq := newExec(org.ID, sandboxID)
	mgrant, err := authorizeExec(t, member, mreq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Member})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("membership revocation", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetRelayGrantId() == mgrant.GetGrantId()
	})
	if err := execWith(testenv.UserToken(org.Member), mgrant.GetGrantId(), mreq); testenv.Code(err) != connect.CodeNotFound && testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("ended membership: %v", err)
	}
	// Expiry: admittable within the 30-second tolerance, not after it.
	oreq := newExec(org.ID, sandboxID)
	ogrant, err := authorizeExec(t, org.OwnerClient, oreq)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(15*time.Minute + 20*time.Second)
	if err := execWith(testenv.UserToken(org.Owner), ogrant.GetGrantId(), oreq); err != nil {
		t.Fatalf("within the tolerance: %v", err)
	}
	env.Clock.Advance(20 * time.Second)
	if err := execWith(testenv.UserToken(org.Owner), ogrant.GetGrantId(), oreq); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("past the tolerance: %v", err)
	}
}

func TestAuthorizeRelayRules(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	// No acknowledgement: unavailable; a late acknowledgement installs the
	// pending grant and a retry with the same authorization_id returns it.
	heldCh := make(chan *ownsandv1.RelayGrant, 1)
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck { heldCh <- g; return nil })
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq)); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("no ack: %v", err)
	}
	agent.SetOnGrant(nil)
	held := <-heldCh
	agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: &ownsandv1.RelayGrantAck{GrantId: held.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}}})
	testenv.Eventually(t, "late ack", func() bool { return env.App.Plane.Grants.Admittable(held.GetGrantId()) != nil })
	retry, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil || retry.Msg.GetGrantId() != held.GetGrantId() {
		t.Fatalf("retry after a lost response: %v %v", retry, err)
	}
	// Replacement names the swap.
	authID2, _ := uuidv7.NewString()
	rep := proto.Clone(areq).(*ownsandv1.AuthorizeRelayRequest)
	rep.AuthorizationId = authID2
	prev := held.GetGrantId()
	rep.ReplacesGrantId = &prev
	repl, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(rep))
	if err != nil {
		t.Fatal(err)
	}
	g := agent.WaitFor("replacement grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == repl.Msg.GetGrantId() }).GetRelayGrant()
	if g.GetReplacesGrantId() != prev {
		t.Fatalf("replacement grant = %v", g)
	}
	testenv.Eventually(t, "predecessor removed", func() bool { return env.App.Plane.Grants.Get(prev) == nil })
	// The agent's grant limit rejection.
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
		return &ownsandv1.RelayGrantAck{GrantId: g.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT}
	})
	other := newExec(org.ID, sandboxID)
	_, err = authorizeExec(t, c, other)
	if testenv.Code(err) != connect.CodeResourceExhausted || errDetail(err).GetRetryAfter() == nil {
		t.Fatalf("agent limit: %v", err)
	}
	agent.SetOnGrant(nil)
	// Field rules.
	attach := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC,
		OperationId: req.GetExecId(), RequestDigest: []byte("x"), AuthorizationId: authID2}
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(attach)); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("attach with a digest: %v", err)
	}
	// The per-principal rate limit of 10 calls per second.
	limited := false
	for i := 0; i < 15 && !limited; i++ {
		_, err := authorizeExec(t, c, newExec(org.ID, sandboxID))
		limited = testenv.Code(err) == connect.CodeResourceExhausted
	}
	if !limited {
		t.Fatal("no rate limit")
	}
	// A stale replaces_grant_id after the agent channel was lost creates a
	// separate grant.
	agent.Close()
	agent2 := env.NewFakeAgent(agent.Device)
	agent2.SetSandbox(sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	agent2.Connect()
	env.Clock.Advance(2 * time.Second)
	authID3, _ := uuidv7.NewString()
	stale := proto.Clone(areq).(*ownsandv1.AuthorizeRelayRequest)
	stale.AuthorizationId = authID3
	stalePrev := repl.Msg.GetGrantId()
	stale.ReplacesGrantId = &stalePrev
	fresh, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(stale))
	if err != nil {
		t.Fatal(err)
	}
	g = agent2.WaitFor("fresh grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == fresh.Msg.GetGrantId() }).GetRelayGrant()
	if g.ReplacesGrantId != nil {
		t.Fatalf("stale replacement forwarded: %v", g)
	}
}

func TestRelayConnectionRules(t *testing.T) {
	limits := relay.Limits{StreamBuffer: 2 << 20, MaxStreamsPerConn: 64, MaxStreamsPerGrant: 4, Keepalive: 100 * time.Millisecond, IdleTimeout: 400 * time.Millisecond}
	env := testenv.New(t, func(o *app.Options) { o.RelayLimits = &limits })
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	// Wrong credentials.
	stream := env.Clients(org.APIKey).RelayAgent.Connect(ctx)
	_ = stream.Send(&ownsandv1.ConnectRequest{})
	if _, err := stream.Receive(); testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("api key on Connect: %v", err)
	}
	r := agent.ConnectRelay(serveExec)
	// Echo and keepalives from the server.
	r.SendFrame(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE, Payload: &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: &ownsandv1.RelayKeepalive{EchoRequested: true}}})
	r.WaitFrame("keepalive", func(f *ownsandv1.DataFrame) bool { return f.GetRelayKeepalive() != nil })
	// The 90-second idle close (shortened): the agent sends nothing.
	select {
	case <-r.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("idle connection stayed open")
	}
	// Replacement: a second connection replaces the first.
	r1 := agent.ConnectRelay(serveExec)
	stop := keepAlive(r1)
	r2 := agent.ConnectRelay(serveExec)
	stop2 := keepAlive(r2)
	defer stop2()
	select {
	case <-r1.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("replaced connection stayed open")
	}
	stop()
	// A frame beyond the credit window closes the connection.
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	r2.SetOnRequest(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		big := make([]byte, 1<<20)
		for i := 0; i < 4; i++ {
			s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: big}}})
		}
	})
	es, err := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collectExec(t, es)
	select {
	case <-r2.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("a connection beyond its window stayed open")
	}
	// Closing the agent channel closes the relay connection.
	r3 := agent.ConnectRelay(serveExec)
	stop3 := keepAlive(r3)
	defer stop3()
	agent.Close()
	select {
	case <-r3.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("relay connection outlived its agent channel")
	}
}

// keepAlive sends relay keepalives until stopped.
func keepAlive(r *testenv.FakeRelay) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.SendFrame(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE, Payload: &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: &ownsandv1.RelayKeepalive{}}})
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func TestRelayStreamTimers(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	var modeV atomic.Value
	modeV.Store("")
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		switch modeV.Load().(string) {
		case "silent":
		case "session-end":
			s.Send(&ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED})
		case "long":
			s.Send(&ownsandv1.RequestAccepted{})
		}
	})
	stop := keepAlive(r)
	defer func() { stop() }()
	run := func() ([]*ownsandv1.ExecEvent, error, time.Duration) {
		req := newExec(org.ID, sandboxID)
		grant, err := authorizeExec(t, org.OwnerClient, req)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		es, err := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err, 0
		}
		events, err := collectExec(t, es)
		return events, err, time.Since(start)
	}
	// Session end from the agent: unavailable with the reason.
	modeV.Store("session-end")
	_, err, _ := run()
	if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetSessionEndReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
		t.Fatalf("session end: %v", err)
	}
	// No accepted notice (keepalives from the connection keep restarting the
	// acceptance timer, so stop them): unavailable, with keepalive events
	// before it and a stream close sent.
	stop()
	modeV.Store("silent")
	events, err, _ := run()
	if testenv.Code(err) != connect.CodeUnavailable || len(events) == 0 || events[0].GetKeepalive() == nil {
		t.Fatalf("acceptance timeout: %v, %d events", err, len(events))
	}
	// Accepted and silent: keepalives, then the rotation with an OK status
	// and no final event before the rotation limit (3 s in tests).
	stop = keepAlive(r)
	modeV.Store("long")
	events, err, took := run()
	if err != nil || took < 2900*time.Millisecond || events[len(events)-1].GetKeepalive() == nil {
		t.Fatalf("rotation: %v after %v", err, took)
	}
	keepalives := 0
	for _, e := range events {
		if e.GetKeepalive() != nil {
			keepalives++
		}
	}
	if keepalives < 5 {
		t.Fatalf("keepalives = %d", keepalives)
	}
}
