// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestIntegrationUpdateVendorSettingsFlags(t *testing.T) {
	cursor := sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR
	devin := sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN
	claude := sandrunv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE
	for _, tc := range []struct {
		name    string
		vendor  sandrunv1.AgentVendor
		flags   []string
		wantErr string
		want    *sandrunv1.UpdateAgentIntegrationRequest
	}{
		{name: "cursor rejects outpost", vendor: cursor, flags: []string{"--outpost-name", "outpost", "--credentials-stdin"}, wantErr: "--outpost-name applies only to devin"},
		{name: "devin rejects pool", vendor: devin, flags: []string{"--pool-name", "pool", "--credentials-stdin"}, wantErr: "--pool-name applies only to cursor"},
		{name: "claude rejects pool", vendor: claude, flags: []string{"--pool-name="}, wantErr: "--pool-name applies only to cursor"},
		{name: "nothing to update", vendor: cursor, wantErr: "nothing to update"},
		{name: "cursor pool", vendor: cursor, flags: []string{"--pool-name", "pool"}, want: &sandrunv1.UpdateAgentIntegrationRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"settings"}},
			Settings:   &sandrunv1.UpdateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool"}},
		}},
		{name: "devin outpost", vendor: devin, flags: []string{"--outpost-name", "outpost"}, want: &sandrunv1.UpdateAgentIntegrationRequest{
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"settings"}},
			Settings:   &sandrunv1.UpdateAgentIntegrationRequest_Devin{Devin: &sandrunv1.DevinSettings{OutpostName: "outpost"}},
		}},
		{name: "name only", vendor: cursor, flags: []string{"--name", "new"}, want: &sandrunv1.UpdateAgentIntegrationRequest{
			Name: "new", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
		}},
		{name: "name and pool", vendor: cursor, flags: []string{"--name", "new", "--pool-name", "pool"}, want: &sandrunv1.UpdateAgentIntegrationRequest{
			Name: "new", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name", "settings"}},
			Settings: &sandrunv1.UpdateAgentIntegrationRequest_Cursor{Cursor: &sandrunv1.CursorSettings{PoolName: "pool"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, tc.vendor)
			args := append([]string{"integration", "update", integrationTestID}, tc.flags...)
			r := f.run(t, args, unreadIntegrationCredentials{t})
			wantRequests := []proto.Message{&sandrunv1.GetAgentIntegrationRequest{
				OrganizationId: integrationTestOrg, AgentIntegrationId: integrationTestID,
			}}
			if tc.want != nil {
				tc.want.OrganizationId, tc.want.AgentIntegrationId = integrationTestOrg, integrationTestID
				wantRequests = append(wantRequests, tc.want)
			}
			checkIntegrationResult(t, r, tc.wantErr)
			f.checkRequests(t, wantRequests...)
		})
	}
}

func TestIntegrationCreateVendorSettingsFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vendor  string
		flags   []string
		wantErr string
	}{
		{name: "cursor rejects outpost", vendor: "cursor", flags: []string{"--outpost-name", "outpost"}, wantErr: "--outpost-name applies only to devin"},
		{name: "devin rejects pool", vendor: "devin", flags: []string{"--pool-name", "pool"}, wantErr: "--pool-name applies only to cursor"},
		{name: "codex rejects outpost", vendor: "openai-codex", flags: []string{"--outpost-name="}, wantErr: "--outpost-name applies only to devin"},
		{name: "cursor rejects both flags", vendor: "cursor", flags: []string{"--pool-name", "pool", "--outpost-name", "outpost"}, wantErr: "--outpost-name applies only to devin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegrationFixture(t, sandrunv1.AgentVendor_AGENT_VENDOR_UNSPECIFIED)
			args := append([]string{"integration", "create", "--vendor", tc.vendor, "--name", "new"}, tc.flags...)
			r := f.run(t, args, unreadIntegrationCredentials{t})
			checkIntegrationResult(t, r, tc.wantErr)
			f.checkRequests(t)
		})
	}
}

const (
	integrationTestOrg = "test-org"
	integrationTestID  = "test-integration"
)

type integrationFixture struct {
	url string
	mu  sync.Mutex
	rpc []proto.Message
}

func newIntegrationFixture(t *testing.T, vendor sandrunv1.AgentVendor) *integrationFixture {
	t.Helper()
	f := &integrationFixture{}
	integration := &sandrunv1.AgentIntegration{AgentIntegrationId: integrationTestID, Vendor: vendor}
	handler := connect.UnaryInterceptorFunc(func(_ connect.UnaryFunc) connect.UnaryFunc {
		return func(_ context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			msg := req.Any().(proto.Message)
			f.rpc = append(f.rpc, msg)
			switch msg.(type) {
			case *sandrunv1.GetAgentIntegrationRequest:
				return connect.NewResponse(&sandrunv1.GetAgentIntegrationResponse{Integration: integration}), nil
			case *sandrunv1.CreateAgentIntegrationRequest:
				return connect.NewResponse(&sandrunv1.CreateAgentIntegrationResponse{Integration: integration}), nil
			case *sandrunv1.UpdateAgentIntegrationRequest:
				return connect.NewResponse(&sandrunv1.UpdateAgentIntegrationResponse{Integration: integration}), nil
			default:
				return nil, connect.NewError(connect.CodeUnimplemented, nil)
			}
		}
	})
	_, service := sandrunv1connect.NewAgentIntegrationServiceHandler(sandrunv1connect.UnimplementedAgentIntegrationServiceHandler{}, connect.WithInterceptors(handler))
	srv := httptest.NewUnstartedServer(service)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *integrationFixture) run(t *testing.T, args []string, stdin io.Reader) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := map[string]string{
		EnvAPIURL: f.url, EnvOrganization: integrationTestOrg, EnvAPIKey: "fake-sandrun-key",
		EnvConfigDir: filepath.Join(t.TempDir(), "config"), EnvNoColor: "1",
	}
	code := Run(t.Context(), Env{
		Args: args, Stdin: stdin, Stdout: &stdout, Stderr: &stderr,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (f *integrationFixture) checkRequests(t *testing.T, want ...proto.Message) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rpc) != len(want) {
		t.Fatalf("got %d RPCs, want %d: %v", len(f.rpc), len(want), f.rpc)
	}
	for i, request := range f.rpc {
		if !proto.Equal(request, want[i]) {
			t.Errorf("request %d = %v, want %v", i, request, want[i])
		}
	}
}

func checkIntegrationResult(t *testing.T, r result, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if r.code != 0 {
			t.Fatalf("exit = %d, want 0: %s", r.code, r.stderr)
		}
		return
	}
	if r.code != ExitUsage || !strings.Contains(r.stderr, wantErr) {
		t.Fatalf("exit = %d, stderr = %q, want %d and %q", r.code, r.stderr, ExitUsage, wantErr)
	}
}

type unreadIntegrationCredentials struct{ t *testing.T }

func (r unreadIntegrationCredentials) Read([]byte) (int, error) {
	r.t.Error("the CLI read credentials from standard input")
	return 0, io.EOF
}
