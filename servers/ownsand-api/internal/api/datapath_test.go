// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
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

// TestRelayGrantFailureLimit checks that every grant check rejection (an
// unknown grant, a grant of another principal and a request that does not
// match the grant) takes a token from one failures bucket per caller, that a
// valid grant takes none, and that the bucket refills.
func TestRelayGrantFailureLimit(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	agent.ConnectRelay(serveExec)
	keyClients := env.Clients(org.APIKey)
	execWith := func(token, grantID string, r *ownsandv1.ExecRequest) error {
		stream, err := env.ClientsWith(token, grantHeader(grantID)).Relay.Exec(ctx, connect.NewRequest(r))
		if err != nil {
			return err
		}
		_, err = collectExec(t, stream)
		return err
	}
	validExec := func() error {
		// AuthorizeRelay allows 10 calls per second per principal; the short
		// advance keeps it under that limit and adds less than a tenth of a
		// token to the failures bucket (one token per 6 seconds).
		env.Clock.Advance(200 * time.Millisecond)
		req := newExec(org.ID, sandboxID)
		grant, err := authorizeExec(t, keyClients, req)
		if err != nil {
			t.Fatal(err)
		}
		return execWith(org.APIKey, grant.GetGrantId(), req)
	}
	// The grant of the API key, used for the principal and binding mismatch.
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, keyClients, req)
	if err != nil {
		t.Fatal(err)
	}
	unknown := func(token string) func() error {
		return func() error {
			id, _ := uuidv7.NewString()
			return execWith(token, id, req)
		}
	}
	otherPrincipal := func() error { return execWith(testenv.UserToken(org.Member), grant.GetGrantId(), req) }
	otherOperation := func() error { return execWith(org.APIKey, grant.GetGrantId(), newExec(org.ID, sandboxID)) }
	requireGrantInvalid := func(err error, what string) {
		t.Helper()
		if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetSessionEndReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
			t.Fatalf("%s: %v %v", what, err, errDetail(err))
		}
	}
	requireDenied := func(err error, what string) {
		t.Helper()
		if testenv.Code(err) != connect.CodePermissionDenied {
			t.Fatalf("%s: %v", what, err)
		}
	}
	requireLimited := func(err error, what string) {
		t.Helper()
		if testenv.Code(err) != connect.CodeResourceExhausted || errDetail(err).GetRetryAfter().AsDuration() <= 0 {
			t.Fatalf("%s: want resource_exhausted with retry_after, got %v %v", what, err, errDetail(err))
		}
	}

	// Valid requests never take a token, also beyond the burst of 10.
	for i := 0; i < 12; i++ {
		if err := validExec(); err != nil {
			t.Fatalf("valid request %d: %v", i, err)
		}
	}

	// The API key takes the whole burst with a mix of the three rejections:
	// they share one bucket.
	for i := 0; i < 4; i++ {
		requireDenied(otherOperation(), "binding mismatch under the limit")
	}
	for i := 0; i < 3; i++ {
		requireGrantInvalid(unknown(org.APIKey)(), "unknown grant under the limit")
	}
	// A valid request between failures does not take a token.
	if err := validExec(); err != nil {
		t.Fatalf("valid request between failures: %v", err)
	}
	// The member's rejections go to the member's own bucket; the API key's
	// bucket has 3 tokens left, so 3 further failures of any category pass.
	for i := 0; i < 3; i++ {
		requireDenied(otherOperation(), "binding mismatch with the last tokens")
	}
	requireLimited(otherOperation(), "binding mismatch with an empty bucket")
	requireLimited(unknown(org.APIKey)(), "unknown grant with an empty bucket")
	// Another caller has its own bucket: the member gets permission_denied
	// for the API key's grant until its own 10 tokens are gone.
	for i := 0; i < 10; i++ {
		requireDenied(otherPrincipal(), "principal mismatch under the limit")
	}
	requireLimited(otherPrincipal(), "principal mismatch with an empty bucket")
	requireLimited(unknown(testenv.UserToken(org.Member))(), "member unknown grant with an empty bucket")
	// A valid grant of the limited API key still opens the stream.
	if err := validExec(); err != nil {
		t.Fatalf("valid request with an empty failures bucket: %v", err)
	}
	// The retry delay is the refill time of one token: 10 per minute.
	env.Clock.Advance(6 * time.Second)
	requireDenied(otherOperation(), "binding mismatch after one token refilled")
	requireLimited(otherOperation(), "binding mismatch after the refilled token")
	// The RPC log records the status and no credential or payload.
	logs := env.Logs.String()
	if !strings.Contains(logs, `"code":"resource_exhausted"`) || !strings.Contains(logs, `"code":"permission_denied"`) {
		t.Fatal("the RPC log does not record the rejection statuses")
	}
	for _, secret := range []string{"secret-command", "secret-env", org.APIKey, testenv.UserToken(org.Member)} {
		if strings.Contains(logs, secret) {
			t.Fatalf("%q appears in the logs", secret)
		}
	}
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
	// EXEC and ATTACH_EXEC grants need the canonical lowercase operation_id
	// and nothing is reserved for a rejected request, so the same
	// authorization_id still works with the lowercase spelling.
	upperReq := newExec(org.ID, sandboxID)
	upperAuthID, _ := uuidv7.NewString()
	upperExec := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: strings.ToUpper(upperReq.GetExecId()), RequestDigest: relaybinding.ExecDigest(upperReq), AuthorizationId: upperAuthID}
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(upperExec)); testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "operation_id") {
		t.Fatalf("EXEC with an uppercase operation_id: %v", err)
	}
	upperAttach := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC,
		OperationId: strings.ToUpper(upperReq.GetExecId()), AuthorizationId: upperAuthID}
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(upperAttach)); testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "operation_id") {
		t.Fatalf("ATTACH_EXEC with an uppercase operation_id: %v", err)
	}
	upperExec.OperationId = upperReq.GetExecId()
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(upperExec)); err != nil {
		t.Fatalf("EXEC with the lowercase operation_id after a rejected uppercase one: %v", err)
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

// TestAuthorizeRelayChannelLostWhilePending covers a pending grant whose
// agent channel closes without an acknowledgement: the caller gets
// unavailable without a retry delay (not the grant-limit resource_exhausted
// result) and the pending grant is removed.
func TestAuthorizeRelayChannelLostWhilePending(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	receivedCh := make(chan *ownsandv1.RelayGrant, 1)
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck { receivedCh <- g; return nil })
	errCh := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.DataPath.AuthorizeRelay(context.Background(), connect.NewRequest(areq))
		errCh <- err
	}()
	received := <-receivedCh
	// The channel closes while the caller waits, well before the
	// acknowledgement timeout, so the removal of the pending grant ends the wait.
	agent.Close()
	var err error
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("AuthorizeRelay did not return after the agent channel closed")
	}
	if testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("channel lost while pending: %v", err)
	}
	if errDetail(err).GetRetryAfter() != nil {
		t.Fatalf("channel loss carries a retry delay: %v", err)
	}
	if env.App.Plane.Grants.Get(received.GetGrantId()) != nil {
		t.Fatal("pending grant kept after the channel closed")
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
	// stop is idempotent; the deferred call stops the keepalive when the
	// fixture fails before the explicit stop below.
	defer stop()
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

// stalledExecCaller sets up an Exec whose caller reads nothing while the
// agent fills the buffers between the handler and the caller with stdout, so
// that the handler blocks in its send and stops dequeuing frames. The relay
// stream buffer exceeds the HTTP/2 and socket buffers on the path, so the
// agent stays within its credit window while the handler blocks.
func stalledExecCaller(t *testing.T, env *testenv.Env, org *testenv.Org, sandboxID string, r *testenv.FakeRelay, afterFill func(s *testenv.RelayStream)) *connect.ServerStreamForClient[ownsandv1.ExecResponse] {
	t.Helper()
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	r.SetOnRequest(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		s.Send(&ownsandv1.RequestAccepted{})
		chunk := make([]byte, 1<<20)
		for i := 0; i < 40; i++ {
			s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: int64(i) << 20, Data: chunk}}})
		}
		afterFill(s)
	})
	es, err := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestRelayBoundsFramesWithoutCredit(t *testing.T) {
	limits := relay.Limits{StreamBuffer: 48 << 20, MaxStreamsPerConn: 64, MaxStreamsPerGrant: 4, Keepalive: time.Second, IdleTimeout: time.Minute,
		MaxQueuedControlFrames: 4, MaxQueuedControlBytes: 64 << 10}
	env := testenv.New(t, func(o *app.Options) { o.RelayLimits = &limits })
	org, agent, sandboxID := runningSandbox(t, env)

	// An agent that queues frames without credit beyond the limit while the
	// caller reads nothing loses its connection, and the call ends with
	// unavailable.
	r := agent.ConnectRelay(serveExec)
	es := stalledExecCaller(t, env, org, sandboxID, r, func(s *testenv.RelayStream) {
		for i := 0; i < 20; i++ {
			s.Send(&ownsandv1.RequestAccepted{})
		}
	})
	select {
	case <-r.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("a connection that queued frames without credit stayed open")
	}
	if _, err := collectExec(t, es); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("call on the closed connection: %v, want unavailable", err)
	}

	// A compliant exchange with a slow caller (one accepted frame, credit
	// frames, one terminal frame) never reaches the limit.
	r2 := agent.ConnectRelay(serveExec)
	stop := keepAlive(r2)
	defer stop()
	es = stalledExecCaller(t, env, org, sandboxID, r2, func(s *testenv.RelayStream) {
		s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}}})
	})
	time.Sleep(500 * time.Millisecond) // the handler is blocked while the caller reads nothing
	events, err := collectExec(t, es)
	if err != nil {
		t.Fatalf("compliant exchange: %v", err)
	}
	if last := events[len(events)-1]; last.GetExited() == nil {
		t.Fatalf("last event = %v, want the exit", last)
	}
	select {
	case <-r2.Closed():
		t.Fatal("a compliant exchange closed the connection")
	default:
	}

	// An unknown frame kind fails only its stream; the connection stays open.
	r2.SetOnRequest(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		r2.SendFrame(&ownsandv1.DataFrame{StreamId: s.ID, SandboxId: s.SandboxID, Kind: ownsandv1.DataFrameKind(9999)})
	})
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	es, err = env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collectExec(t, es); err == nil {
		t.Fatal("a stream with an unknown frame kind completed")
	}
	select {
	case <-r2.Closed():
		t.Fatal("an unknown frame kind closed the connection")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRelayUnknownFrameKind checks that an unknown frame kind from the agent
// on a stream ends only that stream: the control plane sends one UNIMPLEMENTED
// error frame on it, no stream close frame, and the RPC returns unimplemented,
// while another stream of the same connection continues.
func TestRelayUnknownFrameKind(t *testing.T) {
	env := testenv.New(t)
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(serveExec)
	stop := keepAlive(r)
	defer stop()

	reqOK, reqBad := newExec(org.ID, sandboxID), newExec(org.ID, sandboxID)
	release := make(chan struct{})
	r.SetOnRequest(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		if f.GetExecRequest().GetExecId() == reqOK.GetExecId() {
			// The healthy stream answers only after the other stream failed.
			s.Send(&ownsandv1.RequestAccepted{})
			<-release
			serveExec(s, f)
			return
		}
		s.Send(&ownsandv1.RequestAccepted{})
		r.SendFrame(&ownsandv1.DataFrame{StreamId: s.ID, SandboxId: s.SandboxID, Kind: ownsandv1.DataFrameKind(9999)})
	})
	call := func(req *ownsandv1.ExecRequest) *connect.ServerStreamForClient[ownsandv1.ExecResponse] {
		t.Helper()
		grant, err := authorizeExec(t, org.OwnerClient, req)
		if err != nil {
			t.Fatal(err)
		}
		es, err := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(context.Background(), connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return es
	}
	ok := call(reqOK)
	// The healthy stream is open (its stream open frame reached the agent)
	// before the failing one starts.
	r.WaitFrame("healthy request", func(f *ownsandv1.DataFrame) bool { return f.GetExecRequest().GetExecId() == reqOK.GetExecId() })
	bad := call(reqBad)

	_, err := collectExec(t, bad)
	if testenv.Code(err) != connect.CodeUnimplemented {
		t.Fatalf("stream with an unknown frame kind: %v, want unimplemented", err)
	}
	var badID uint64
	for _, f := range r.Frames() {
		if f.GetExecRequest().GetExecId() == reqBad.GetExecId() {
			badID = f.GetStreamId()
		}
	}
	ef := r.WaitFrame("error frame", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == badID && f.GetError() != nil })
	if ef.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED || ef.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR {
		t.Fatalf("error frame = %v, want ERROR_CODE_UNIMPLEMENTED", ef)
	}
	if msg := ef.GetError().GetMessage(); !strings.Contains(msg, "9999") || !strings.Contains(msg, fmt.Sprintf("stream %d", badID)) {
		t.Fatalf("message %q does not name the kind and the stream", msg)
	}
	// Neither a second error frame nor a stream close frame follows.
	time.Sleep(300 * time.Millisecond)
	errorFrames := 0
	for _, f := range r.Frames() {
		if f.GetStreamId() != badID {
			continue
		}
		if f.GetError() != nil {
			errorFrames++
		}
		if f.GetRelayStreamClose() != nil {
			t.Fatalf("a stream close frame followed the error frame: %v", f)
		}
	}
	if errorFrames != 1 {
		t.Fatalf("error frames on the stream = %d, want 1", errorFrames)
	}

	// The other stream and the connection continue.
	close(release)
	events, err := collectExec(t, ok)
	if err != nil {
		t.Fatalf("healthy stream: %v", err)
	}
	if last := events[len(events)-1]; last.GetExited() == nil {
		t.Fatalf("last event of the healthy stream = %v, want the exit", last)
	}
	select {
	case <-r.Closed():
		t.Fatal("an unknown frame kind closed the connection")
	default:
	}
}

func hasAccepted(events []*ownsandv1.ExecEvent) bool {
	for _, e := range events {
		if e.GetAccepted() != nil {
			return true
		}
	}
	return false
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
		case "large-then-accepted", "small-then-accepted":
			// The acceptance timeout is 1 s in tests. A frame at 0.6 s moves the
			// deadline to 1.6 s only when it is large; the notice at 1.3 s
			// comes after the original deadline.
			size := 1
			if modeV.Load().(string) == "large-then-accepted" {
				size = 2 * relay.LargeFrameBytes
			}
			time.Sleep(600 * time.Millisecond)
			s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 0, Data: make([]byte, size)}}})
			time.Sleep(700 * time.Millisecond)
			s.Send(&ownsandv1.RequestAccepted{})
			s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}}})
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
			return nil, err, time.Since(start)
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
	// No accepted notice while the agent keeps sending keepalives every 50 ms:
	// the keepalives do not extend the wait, so the call ends with
	// unavailable after the acceptance timeout (1 s in tests), with keepalive
	// events before it and a stream close sent.
	modeV.Store("silent")
	events, err, took := run()
	if testenv.Code(err) != connect.CodeUnavailable || len(events) == 0 || events[0].GetKeepalive() == nil {
		t.Fatalf("acceptance timeout: %v, %d events", err, len(events))
	}
	if took > 2*time.Second {
		t.Fatalf("acceptance timeout after %v, want about 1 s although keepalives kept arriving", took)
	}
	// A small frame of the stream does not extend the wait either: the notice
	// after the original deadline finds the call already ended.
	modeV.Store("small-then-accepted")
	if _, err, took = run(); testenv.Code(err) != connect.CodeUnavailable || took > 1500*time.Millisecond {
		t.Fatalf("small frame: %v after %v, want unavailable at about 1 s", err, took)
	}
	// A large frame extends the wait by one acceptance timeout from its
	// arrival, so the notice behind it is accepted.
	modeV.Store("large-then-accepted")
	events, err, took = run()
	if err != nil || took < 1300*time.Millisecond {
		t.Fatalf("large frame: %v after %v, want the notice accepted after the original deadline", err, took)
	}
	if !hasAccepted(events) {
		t.Fatalf("large frame: events = %v, want an accepted event", events)
	}
	// Accepted and silent: keepalives, then the rotation with an OK status
	// and no final event before the rotation limit (3 s in tests).
	modeV.Store("long")
	events, err, took = run()
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

// failGrantSender makes the relay grant enqueue fail with err while the
// returned switch is on, and queues on the live channel otherwise. It
// reproduces a live channel whose outbound queue cannot take the grant before
// the caller's context ends.
func failGrantSender(env *testenv.Env, err error) (fail *atomic.Bool, entered chan struct{}) {
	fail = &atomic.Bool{}
	fail.Store(true)
	entered = make(chan struct{}, 8)
	env.App.Plane.SetGrantSender(func(ctx context.Context, conn *hub.Conn, e *ownsandv1.AgentEnvelope) error {
		entered <- struct{}{}
		if fail.Load() {
			return err
		}
		return conn.Send(ctx, e)
	})
	return fail, entered
}

func grantsReceived(agent *testenv.FakeAgent) (ids []string) {
	for _, e := range agent.Received() {
		if g := e.GetRelayGrant(); g != nil {
			ids = append(ids, g.GetGrantId())
		}
	}
	return ids
}

// TestAuthorizeRelayEnqueueFailure covers a relay grant that cannot be queued
// on a live agent channel: the reservation is released, so a retry with the
// same authorization_id sends the grant again and returns its ID after the
// agent acknowledges it; the failure is logged with the grant ID and an error
// category.
func TestAuthorizeRelayEnqueueFailure(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	fail, _ := failGrantSender(env, context.Canceled)
	_, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetRetryAfter() != nil {
		t.Fatalf("enqueue failure: %v", err)
	}
	if n := env.App.Plane.Grants.Len(); n != 0 {
		t.Fatalf("%d grants after the failed enqueue", n)
	}
	if len(grantsReceived(agent)) != 0 {
		t.Fatal("the agent received a grant that was never queued")
	}
	logs := env.Logs.String()
	if !strings.Contains(logs, `"msg":"relay_grant_enqueue_failed"`) || !strings.Contains(logs, `"error_category":"context_canceled"`) || !strings.Contains(logs, `"grant_id"`) {
		t.Fatalf("no structured enqueue failure event in the logs")
	}
	// The retry reserves and sends again, and returns the ID after the ack.
	fail.Store(false)
	retry, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if ids := grantsReceived(agent); len(ids) != 1 || ids[0] != retry.Msg.GetGrantId() {
		t.Fatalf("grants sent = %v, returned %s", ids, retry.Msg.GetGrantId())
	}
	if env.App.Plane.Grants.Len() != 1 || env.App.Plane.Grants.Admittable(retry.Msg.GetGrantId()) == nil {
		t.Fatal("the retried grant is not the one live grant")
	}
	// An identical retry of the installed grant returns the same ID.
	again, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil || again.Msg.GetGrantId() != retry.Msg.GetGrantId() {
		t.Fatalf("identical retry: %v %v", again, err)
	}
}

// TestAuthorizeRelayReplacementEnqueueFailure covers a replacement whose
// grant cannot be queued: the predecessor stays live and can be replaced
// again, and the retry of the replacement succeeds.
func TestAuthorizeRelayReplacementEnqueueFailure(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	first, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil {
		t.Fatal(err)
	}
	prev := first.Msg.GetGrantId()
	repID, _ := uuidv7.NewString()
	rep := proto.Clone(areq).(*ownsandv1.AuthorizeRelayRequest)
	rep.AuthorizationId = repID
	rep.ReplacesGrantId = &prev
	fail, _ := failGrantSender(env, context.Canceled)
	if _, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(rep)); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("replacement enqueue failure: %v", err)
	}
	if env.App.Plane.Grants.Admittable(prev) == nil || env.App.Plane.Grants.Len() != 1 {
		t.Fatalf("the predecessor was not kept alone: len=%d", env.App.Plane.Grants.Len())
	}
	if got := env.App.Plane.Grants.Get(prev); got.Successor != "" {
		t.Fatalf("the predecessor keeps the successor link %q", got.Successor)
	}
	// The retry is a replacement again (not aborted by a stale successor).
	fail.Store(false)
	repl, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(rep))
	if err != nil {
		t.Fatalf("replacement retry: %v", err)
	}
	agent.WaitFor("replacement grant", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetRelayGrant().GetGrantId() == repl.Msg.GetGrantId() && e.GetRelayGrant().GetReplacesGrantId() == prev
	})
	testenv.Eventually(t, "predecessor replaced", func() bool {
		return env.App.Plane.Grants.Get(prev) == nil && env.App.Plane.Grants.Admittable(repl.Msg.GetGrantId()) != nil
	})
}

// TestAuthorizeRelayKeepsPredecessorAfterRelease covers a predecessor that the
// agent releases after the replacement message was built and before it is
// queued: the queued grant keeps its original replaces_grant_id, and the
// registry has already freed the predecessor.
func TestAuthorizeRelayKeepsPredecessorAfterRelease(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	first, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil {
		t.Fatal(err)
	}
	prev := first.Msg.GetGrantId()
	repID, _ := uuidv7.NewString()
	rep := proto.Clone(areq).(*ownsandv1.AuthorizeRelayRequest)
	rep.AuthorizationId = repID
	rep.ReplacesGrantId = &prev
	// The sender hook runs after the message is built and before it is
	// queued: release the predecessor through the agent channel there.
	env.App.Plane.SetGrantSender(func(ctx context.Context, conn *hub.Conn, e *ownsandv1.AgentEnvelope) error {
		agent.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantRelease{RelayGrantRelease: &ownsandv1.RelayGrantRelease{GrantId: prev}}})
		testenv.Eventually(t, "predecessor released", func() bool { return env.App.Plane.Grants.Get(prev) == nil })
		return conn.Send(ctx, e)
	})
	repl, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(rep))
	if err != nil {
		t.Fatalf("replacement: %v", err)
	}
	g := agent.WaitFor("replacement grant", func(e *ownsandv1.AgentEnvelope) bool { return e.GetRelayGrant().GetGrantId() == repl.Msg.GetGrantId() }).GetRelayGrant()
	if g.ReplacesGrantId == nil || g.GetReplacesGrantId() != prev {
		t.Fatalf("queued grant lost its predecessor: replaces_grant_id = %v", g.ReplacesGrantId)
	}
	if env.App.Plane.Grants.Get(prev) != nil || env.App.Plane.Grants.Admittable(repl.Msg.GetGrantId()) == nil {
		t.Fatal("the registry did not free the predecessor and install the successor")
	}
}

// TestAuthorizeRelayAckWithoutReplacedGrant covers an acknowledgement of a
// replacement that does not name the predecessor: the predecessor stays
// installed and a later replacement of it succeeds instead of aborting.
func TestAuthorizeRelayAckWithoutReplacedGrant(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	first, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
	if err != nil {
		t.Fatal(err)
	}
	prev := first.Msg.GetGrantId()
	agent.SetOnGrant(func(g *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
		return &ownsandv1.RelayGrantAck{GrantId: g.GetGrantId(), Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}
	})
	replace := func() (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
		id, _ := uuidv7.NewString()
		rep := proto.Clone(areq).(*ownsandv1.AuthorizeRelayRequest)
		rep.AuthorizationId = id
		rep.ReplacesGrantId = &prev
		return c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(rep))
	}
	if _, err := replace(); err != nil {
		t.Fatalf("first replacement: %v", err)
	}
	if env.App.Plane.Grants.Admittable(prev) == nil {
		t.Fatal("the predecessor was removed by an acknowledgement that did not name it")
	}
	if _, err := replace(); err != nil {
		t.Fatalf("second replacement of the same predecessor: %v", err)
	}
}

// TestAuthorizeRelayEnqueueFailureWakesWaiters covers a concurrent call of
// the same authorization_id that waits on the reservation: when the enqueue
// fails it returns unavailable (retryable, no grant-limit delay), not
// resource_exhausted.
func TestAuthorizeRelayEnqueueFailureWakesWaiters(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, _, sandboxID := runningSandbox(t, env)
	c := org.OwnerClient
	req := newExec(org.ID, sandboxID)
	authID, _ := uuidv7.NewString()
	areq := &ownsandv1.AuthorizeRelayRequest{OrganizationId: org.ID, SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: req.GetExecId(), RequestDigest: relaybinding.ExecDigest(req), AuthorizationId: authID}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	env.App.Plane.SetGrantSender(func(ctx context.Context, conn *hub.Conn, e *ownsandv1.AgentEnvelope) error {
		entered <- struct{}{}
		<-release
		return context.DeadlineExceeded
	})
	errs := make(chan error, 2)
	go func() {
		_, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
		errs <- err
	}()
	<-entered
	go func() {
		_, err := c.DataPath.AuthorizeRelay(ctx, connect.NewRequest(areq))
		errs <- err
	}()
	// Let the second call find the pending reservation and wait on it. If it
	// is slower, it reserves after the removal and fails in the sender too.
	time.Sleep(200 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if testenv.Code(err) != connect.CodeUnavailable || errDetail(err).GetRetryAfter() != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call did not return after the enqueue failure")
		}
	}
	if n := env.App.Plane.Grants.Len(); n != 0 {
		t.Fatalf("%d grants left", n)
	}
}

// TestRelayRotationWithQueuedFrames checks that the rotation deadline holds
// on every receive, not only on an empty wake. The long keepalive keeps the
// handler's ticker from waking it near the deadline, so the only receives
// after the deadline carry frames: the call must end with an OK status at the
// first of them, and neither that frame nor the exit event behind it is
// forwarded.
func TestRelayRotationWithQueuedFrames(t *testing.T) {
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 2 * time.Second, Keepalive: 30 * time.Second, Acceptance: 30 * time.Second,
			RequestDeadline: 10 * time.Second, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	r := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		s.Send(&ownsandv1.RequestAccepted{})
		s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 0, Data: []byte("before")}}})
		time.Sleep(2500 * time.Millisecond) // past the rotation deadline
		s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 6, Data: []byte("after")}}})
		s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}}})
	})
	stop := keepAlive(r)
	defer stop()
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	es, err := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay.Exec(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectExec(t, es)
	if err != nil {
		t.Fatalf("rotation with queued frames: %v", err)
	}
	// The events are the accepted notice and the output before the deadline.
	if len(events) != 2 || events[0].GetAccepted() == nil || string(events[1].GetStdout().GetData()) != "before" {
		t.Fatalf("events = %v, want only the events before the deadline", events)
	}
}

// sweepRevocationsNow runs the periodic revocation sweep and returns the
// number of pending revocations that remain.
func sweepRevocationsNow(env *testenv.Env) int {
	env.App.Plane.SweepExpired(context.Background())
	return env.App.Plane.PendingRevocations()
}

// TestUnacknowledgedSessionRevocationExpires checks that a pending session
// revocation that the agent never acknowledges is deleted at the absolute
// expiry of the session, and not before, while the channel stays open.
func TestUnacknowledgedSessionRevocationExpires(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	agent.SetAckRevocations(false)
	key := env.Clients(org.APIKey)
	resp, err := key.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("session revocation", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetSessionId() == resp.Msg.GetSessionId()
	})
	if n := env.App.Plane.PendingRevocations(); n != 1 {
		t.Fatalf("pending revocations = %d, want 1", n)
	}
	env.Clock.Advance(time.Hour - time.Second)
	if n := sweepRevocationsNow(env); n != 1 {
		t.Fatalf("pending revocations one second before the session expiry = %d, want 1", n)
	}
	env.Clock.Advance(time.Second)
	if n := sweepRevocationsNow(env); n != 0 {
		t.Fatalf("pending revocations at the session expiry = %d, want 0", n)
	}
	if !strings.Contains(env.Logs.String(), "session_revocation_expired") {
		t.Fatal("the expiry of the revocation is not logged")
	}
}

// TestUnacknowledgedGrantRevocationExpires checks the same for a relay grant:
// the revocation lives until the grant stops being revocable (stream deadline
// plus the delivery margin), and OpenSession sweeps it without the periodic
// worker.
func TestUnacknowledgedGrantRevocationExpires(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org, agent, sandboxID := runningSandbox(t, env)
	agent.SetAckRevocations(false)
	req := newExec(org.ID, sandboxID)
	grant, err := authorizeExec(t, env.Clients(org.APIKey), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.OwnerClient.APIKey.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: org.APIKeyID})); err != nil {
		t.Fatal(err)
	}
	agent.WaitFor("grant revocation", func(e *ownsandv1.AgentEnvelope) bool {
		return e.GetSessionRevocation().GetRelayGrantId() == grant.GetGrantId()
	})
	end := grants.StreamDeadline + grants.DeliveryMargin
	env.Clock.Advance(end - time.Second)
	if n := sweepRevocationsNow(env); n != 1 {
		t.Fatalf("pending revocations one second before the grant stops being revocable = %d, want 1", n)
	}
	env.Clock.Advance(time.Second)
	// An OpenSession of another principal runs the sweep.
	if _, err := org.OwnerClient.DataPath.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: org.ID, SandboxId: sandboxID, SdpOffer: "v=0"})); err != nil {
		t.Fatal(err)
	}
	if n := env.App.Plane.PendingRevocations(); n != 0 {
		t.Fatalf("pending revocations when the grant stops being revocable = %d, want 0", n)
	}
}
