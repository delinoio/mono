// SPDX-License-Identifier: Apache-2.0

package cli

// A small in-process control plane for the CLI tests, built on the
// generated ownsandv1connect handlers. Exec and file operations use the
// relay (the tests pass --transport relay); the SDK tests cover the
// peer-to-peer path.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func newID(t testing.TB) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type fakeServer struct {
	t     testing.TB
	srv   *httptest.Server
	org   string
	sb    string
	dev   string
	mu    sync.Mutex
	auths []string
	calls []string
	files map[string][]byte
	// limit is the last limits input of ReplaceDeviceLimits; its optional
	// fields keep the presence that the CLI sent.
	limit *ownsandv1.DeviceLimitsInput
	creat []*ownsandv1.CreateSandboxRequest
	// execIDs are the exec IDs of the relay Exec requests, in order.
	execIDs []string
	// billing and usage are the BillingService state; usage is served only
	// while billing.linked is true.
	billing *ownsandv1.OrganizationBilling
	usage   *ownsandv1.GetBillingUsageResponse
	// reject, when set, is the error of CreateSandbox, AuthorizeRelay and
	// the relay RPCs, as billing enforcement returns it.
	reject error
}

func newFakeServer(t testing.TB) *fakeServer {
	f := &fakeServer{t: t, org: newID(t), sb: newID(t), dev: newID(t), files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewOrganizationServiceHandler(&fakeOrg{f: f}))
	mux.Handle(ownsandv1connect.NewMemberServiceHandler(&fakeMember{f: f}))
	mux.Handle(ownsandv1connect.NewApiKeyServiceHandler(&fakeKeys{f: f}))
	mux.Handle(ownsandv1connect.NewDeviceServiceHandler(&fakeDevices{f: f}))
	mux.Handle(ownsandv1connect.NewSandboxServiceHandler(&fakeSandbox{f: f}))
	mux.Handle(ownsandv1connect.NewDataPathServiceHandler(&fakeDataPath{f: f}))
	mux.Handle(ownsandv1connect.NewRelayServiceHandler(&fakeRelay{f: f}))
	mux.Handle(ownsandv1connect.NewBillingServiceHandler(&fakeBilling{f: f}))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.calls = append(f.calls, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

func (f *fakeServer) rejected() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reject
}

func (f *fakeServer) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeServer) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auths) == 0 {
		return ""
	}
	return f.auths[len(f.auths)-1]
}

type fakeOrg struct {
	ownsandv1connect.UnimplementedOrganizationServiceHandler
	f *fakeServer
}

func (h *fakeOrg) ListOrganizations(context.Context, *connect.Request[ownsandv1.ListOrganizationsRequest]) (*connect.Response[ownsandv1.ListOrganizationsResponse], error) {
	return connect.NewResponse(&ownsandv1.ListOrganizationsResponse{Organizations: []*ownsandv1.Organization{{OrganizationId: h.f.org, Name: "acme", CallerRole: ownsandv1.Role_ROLE_OWNER}}}), nil
}

func (h *fakeOrg) GetOrganization(_ context.Context, r *connect.Request[ownsandv1.GetOrganizationRequest]) (*connect.Response[ownsandv1.GetOrganizationResponse], error) {
	return connect.NewResponse(&ownsandv1.GetOrganizationResponse{Organization: &ownsandv1.Organization{OrganizationId: r.Msg.GetOrganizationId(), Name: "acme"}}), nil
}

type fakeMember struct {
	ownsandv1connect.UnimplementedMemberServiceHandler
	f *fakeServer
}

func (h *fakeMember) CreateInvitation(_ context.Context, r *connect.Request[ownsandv1.CreateInvitationRequest]) (*connect.Response[ownsandv1.CreateInvitationResponse], error) {
	return connect.NewResponse(&ownsandv1.CreateInvitationResponse{
		Invitation:       &ownsandv1.Invitation{InvitationId: newID(h.f.t), OrganizationId: r.Msg.GetOrganizationId(), Role: r.Msg.GetRole(), SecretPrefix: "osi_abc"},
		InvitationSecret: "osi_abcdefsecret",
	}), nil
}

func (h *fakeMember) AcceptInvitation(_ context.Context, r *connect.Request[ownsandv1.AcceptInvitationRequest]) (*connect.Response[ownsandv1.AcceptInvitationResponse], error) {
	if r.Msg.GetInvitationSecret() != "osi_from_stdin" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown invitation"))
	}
	return connect.NewResponse(&ownsandv1.AcceptInvitationResponse{Organization: &ownsandv1.Organization{OrganizationId: h.f.org}}), nil
}

type fakeKeys struct {
	ownsandv1connect.UnimplementedApiKeyServiceHandler
	f *fakeServer
}

func (h *fakeKeys) CreateApiKey(_ context.Context, r *connect.Request[ownsandv1.CreateApiKeyRequest]) (*connect.Response[ownsandv1.CreateApiKeyResponse], error) {
	return connect.NewResponse(&ownsandv1.CreateApiKeyResponse{
		ApiKey:       &ownsandv1.ApiKey{ApiKeyId: newID(h.f.t), OrganizationId: r.Msg.GetOrganizationId(), Name: r.Msg.GetName(), KeyPrefix: "osk_new"},
		ApiKeySecret: "osk_newsecretvalue",
	}), nil
}

func (h *fakeKeys) ListApiKeys(context.Context, *connect.Request[ownsandv1.ListApiKeysRequest]) (*connect.Response[ownsandv1.ListApiKeysResponse], error) {
	return connect.NewResponse(&ownsandv1.ListApiKeysResponse{ApiKeys: []*ownsandv1.ApiKey{{ApiKeyId: newID(h.f.t), Name: "ci", State: ownsandv1.ApiKeyState_API_KEY_STATE_ACTIVE}}, NextPageToken: "next"}), nil
}

type fakeDevices struct {
	ownsandv1connect.UnimplementedDeviceServiceHandler
	f *fakeServer
}

func (h *fakeDevices) GetDevice(context.Context, *connect.Request[ownsandv1.GetDeviceRequest]) (*connect.Response[ownsandv1.GetDeviceResponse], error) {
	return connect.NewResponse(&ownsandv1.GetDeviceResponse{Device: &ownsandv1.Device{DeviceId: h.f.dev, Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 1, DiskGib: 1, VcpuOvercommitPercent: 150}}}), nil
}

// ReplaceDeviceLimits is the only limits RPC that the fake implements, so a
// CLI that called the legacy UpdateDeviceLimits would fail with unimplemented.
func (h *fakeDevices) ReplaceDeviceLimits(_ context.Context, r *connect.Request[ownsandv1.ReplaceDeviceLimitsRequest]) (*connect.Response[ownsandv1.ReplaceDeviceLimitsResponse], error) {
	h.f.mu.Lock()
	h.f.limit = r.Msg.GetLimits()
	h.f.mu.Unlock()
	in := r.Msg.GetLimits()
	limits := &ownsandv1.DeviceLimits{VcpuCount: in.GetVcpuCount(), MemoryMib: in.GetMemoryMib(), DiskGib: in.GetDiskGib(), VcpuOvercommitPercent: in.GetVcpuOvercommitPercent()}
	return connect.NewResponse(&ownsandv1.ReplaceDeviceLimitsResponse{Device: &ownsandv1.Device{DeviceId: h.f.dev, Limits: limits}}), nil
}

type fakeSandbox struct {
	ownsandv1connect.UnimplementedSandboxServiceHandler
	f *fakeServer
}

func (h *fakeSandbox) CreateSandbox(_ context.Context, r *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	h.f.mu.Lock()
	h.f.creat = append(h.f.creat, r.Msg)
	reject := h.f.reject
	h.f.mu.Unlock()
	if reject != nil {
		return nil, reject
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: &ownsandv1.Sandbox{SandboxId: h.f.sb, OrganizationId: r.Msg.GetOrganizationId(), State: ownsandv1.SandboxState_SANDBOX_STATE_CREATING}}), nil
}

func (h *fakeSandbox) GetSandbox(_ context.Context, r *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: &ownsandv1.Sandbox{SandboxId: r.Msg.GetSandboxId(), OrganizationId: r.Msg.GetOrganizationId(), State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, CreateTime: timestamppb.Now()}}), nil
}

type fakeDataPath struct {
	ownsandv1connect.UnimplementedDataPathServiceHandler
	f *fakeServer
}

func (h *fakeDataPath) AuthorizeRelay(context.Context, *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	if err := h.f.rejected(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{GrantId: newID(h.f.t), ExpireTime: timestamppb.New(time.Now().Add(15 * time.Minute)), ExpiresIn: durationpb.New(15 * time.Minute)}), nil
}

type fakeRelay struct {
	ownsandv1connect.UnimplementedRelayServiceHandler
	f *fakeServer
}

func ev(e *ownsandv1.ExecEvent) *ownsandv1.ExecResponse { return &ownsandv1.ExecResponse{Event: e} }

func exited(code int64, timedOut bool) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: code}, TimedOut: timedOut}}}
}

func stdout(off int64, s string) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: off, Data: []byte(s)}}}
}

// Exec runs scripted commands: command[0] selects the result.
func (h *fakeRelay) Exec(_ context.Context, r *connect.Request[ownsandv1.ExecRequest], s *connect.ServerStream[ownsandv1.ExecResponse]) error {
	if r.Header().Get("Ownsand-Relay-Grant-Id") == "" {
		return connect.NewError(connect.CodeUnavailable, errors.New("no grant"))
	}
	h.f.mu.Lock()
	h.f.execIDs = append(h.f.execIDs, r.Msg.GetExecId())
	h.f.mu.Unlock()
	cmd := r.Msg.GetCommand()
	if cmd[0] == "denied" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("digest mismatch"))
	}
	events := []*ownsandv1.ExecEvent{
		{Event: &ownsandv1.ExecEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}},
		{Event: &ownsandv1.ExecEvent_Started{Started: &ownsandv1.ExecStarted{ExecId: r.Msg.GetExecId()}}},
	}
	switch cmd[0] {
	case "echo":
		events = append(events, stdout(0, strings.Join(cmd[1:], " ")+"\n"),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stderr{Stderr: &ownsandv1.ExecOutput{Offset: 0, Data: []byte("err\xff\n")}}},
			exited(0, false))
	case "exit3":
		events = append(events, exited(3, false))
	case "exit300":
		events = append(events, exited(300, false))
	case "sig9":
		events = append(events, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_Signal{Signal: 9}}}})
	case "timeout":
		events = append(events, exited(143, true))
	case "fail":
		events = append(events, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed{Failed: &ownsandv1.ExecFailed{Reason: ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND, Message: "not found"}}})
	case "ended":
		events = append(events, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded{SandboxEnded: &ownsandv1.ExecSandboxEnded{Reason: ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED}}})
	case "split":
		// Multibyte characters span chunk boundaries; stderr ends in the
		// middle of a character.
		events = append(events, stdout(0, "h\xc3"), stdout(2, "\xa9 \xe2\x82"), stdout(6, "\xac"),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stderr{Stderr: &ownsandv1.ExecOutput{Offset: 0, Data: []byte("x\xe2")}}},
			exited(0, false))
	case "splitgap":
		// Bytes are lost after the start of a character.
		events = append(events, stdout(0, "a\xe2\x82"),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated{OutputTruncated: &ownsandv1.ExecOutputTruncated{StdoutOffset: 5}}},
			stdout(5, "\xacz"), exited(0, false))
	case "trunc":
		events = append(events, stdout(0, "ab"),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated{OutputTruncated: &ownsandv1.ExecOutputTruncated{StdoutOffset: 5}}},
			stdout(5, "z"), exited(0, false))
	}
	for _, e := range events {
		if err := s.Send(ev(e)); err != nil {
			return err
		}
	}
	return nil
}

func (h *fakeRelay) ReadFile(_ context.Context, r *connect.Request[ownsandv1.ReadFileRequest], s *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	h.f.mu.Lock()
	data, ok := h.f.files[r.Msg.GetPath()]
	h.f.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("no such file"))
	}
	for _, e := range []*ownsandv1.FileReadEvent{
		{Event: &ownsandv1.FileReadEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}},
		{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: 0, Data: data}}},
		{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: int64(len(data))}}},
	} {
		if err := s.Send(&ownsandv1.ReadFileResponse{Event: e}); err != nil {
			return err
		}
	}
	return nil
}

func (h *fakeRelay) WriteFile(_ context.Context, r *connect.Request[ownsandv1.WriteFileRequest]) (*connect.Response[ownsandv1.WriteFileResponse], error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	key := "write:" + r.Msg.GetWriteId()
	if r.Msg.GetPath() != "" {
		h.f.files[key+":path"] = []byte(r.Msg.GetPath())
	}
	h.f.files[key] = append(h.f.files[key], r.Msg.GetData()...)
	res := &ownsandv1.WriteFileResponse{WriteId: r.Msg.GetWriteId(), Offset: r.Msg.GetOffset(), Length: int64(len(r.Msg.GetData()))}
	if fin := r.Msg.GetFinish(); fin != nil {
		h.f.files[string(h.f.files[key+":path"])] = bytes.Clone(h.f.files[key])
		res.Finished, res.SizeBytes = true, fin.GetSizeBytes()
	}
	return connect.NewResponse(res), nil
}
