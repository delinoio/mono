// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

// updateRecorder is a runner profile service that keeps the update
// requests it receives.
type updateRecorder struct {
	tidev1connect.UnimplementedRunnerProfileServiceHandler
	current *tidev1.RunnerProfile
	mu      sync.Mutex
	gets    int
	updates []*tidev1.UpdateRunnerProfileRequest
}

func (u *updateRecorder) GetRunnerProfile(context.Context, *connect.Request[tidev1.GetRunnerProfileRequest]) (*connect.Response[tidev1.GetRunnerProfileResponse], error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gets++
	return connect.NewResponse(&tidev1.GetRunnerProfileResponse{Profile: proto.CloneOf(u.current)}), nil
}

func (u *updateRecorder) UpdateRunnerProfile(_ context.Context, req *connect.Request[tidev1.UpdateRunnerProfileRequest]) (*connect.Response[tidev1.UpdateRunnerProfileResponse], error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.updates = append(u.updates, proto.CloneOf(req.Msg))
	return connect.NewResponse(&tidev1.UpdateRunnerProfileResponse{Profile: proto.CloneOf(u.current)}), nil
}

// TestProfileUpdateMask checks that profile update sends only the mask
// paths that UpdateRunnerProfile accepts, and keeps the resource values
// without a flag.
func TestProfileUpdateMask(t *testing.T) {
	f := newFixture(t)
	rec := &updateRecorder{current: &tidev1.RunnerProfile{
		RunnerProfileId: "p1", Resources: &tidev1.Resources{VcpuCount: 4, MemoryMib: 8192, DiskGib: 64},
	}}
	path, h := tidev1connect.NewRunnerProfileServiceHandler(rec)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	// The SDK speaks HTTP/2 without TLS, like the tidetest server.
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f.env[EnvAPIURL] = srv.URL

	f.ok("", "--org", f.org, "profile", "update", "p1", "--vcpu", "8", "--image", "linux")
	f.ok("", "--org", f.org, "profile", "update", "p1", "--template", "tpl", "--vcpu", "2", "--memory-mib", "4096", "--disk-gib", "32")
	f.ok("", "--org", f.org, "profile", "update", "p1", "--name", "renamed")
	if len(rec.updates) != 3 {
		t.Fatalf("updates = %d", len(rec.updates))
	}
	// Only the partial resource update reads the current profile.
	if rec.gets != 1 {
		t.Fatalf("GetRunnerProfile calls = %d, want 1", rec.gets)
	}
	for i, tc := range []struct {
		paths     []string
		resources *tidev1.Resources
	}{
		{[]string{"source", "resources"}, &tidev1.Resources{VcpuCount: 8, MemoryMib: 8192, DiskGib: 64}},
		{[]string{"source", "resources"}, &tidev1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32}},
		{[]string{"name"}, nil},
	} {
		got := rec.updates[i]
		if !slices.Equal(got.GetUpdateMask().GetPaths(), tc.paths) {
			t.Errorf("update %d paths = %v, want %v", i, got.GetUpdateMask().GetPaths(), tc.paths)
		}
		if !proto.Equal(got.GetProfile().GetResources(), tc.resources) {
			t.Errorf("update %d resources = %v, want %v", i, got.GetProfile().GetResources(), tc.resources)
		}
	}
	if rec.updates[0].GetProfile().GetBaseImage() != tidev1.GuestOs_GUEST_OS_LINUX || rec.updates[1].GetProfile().GetOwnsandTemplateId() != "tpl" {
		t.Errorf("sources = %v, %v", rec.updates[0].GetProfile().GetSource(), rec.updates[1].GetProfile().GetSource())
	}
	if r := f.run("", "--org", f.org, "profile", "update", "p1"); r.code != ExitUsage {
		t.Fatalf("update without flags = %d", r.code)
	}
}
