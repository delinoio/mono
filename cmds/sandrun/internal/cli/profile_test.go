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

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// updateRecorder is a runner profile service that keeps the update
// requests it receives.
type updateRecorder struct {
	sandrunv1connect.UnimplementedRunnerProfileServiceHandler
	current *sandrunv1.RunnerProfile
	mu      sync.Mutex
	gets    int
	updates []*sandrunv1.UpdateRunnerProfileRequest
}

func (u *updateRecorder) GetRunnerProfile(context.Context, *connect.Request[sandrunv1.GetRunnerProfileRequest]) (*connect.Response[sandrunv1.GetRunnerProfileResponse], error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.gets++
	return connect.NewResponse(&sandrunv1.GetRunnerProfileResponse{Profile: proto.CloneOf(u.current)}), nil
}

func (u *updateRecorder) UpdateRunnerProfile(_ context.Context, req *connect.Request[sandrunv1.UpdateRunnerProfileRequest]) (*connect.Response[sandrunv1.UpdateRunnerProfileResponse], error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.updates = append(u.updates, proto.CloneOf(req.Msg))
	return connect.NewResponse(&sandrunv1.UpdateRunnerProfileResponse{Profile: proto.CloneOf(u.current)}), nil
}

// TestProfileUpdateMask checks that each update sends only the fields named
// by flags and makes no GetRunnerProfile call.
func TestProfileUpdateMask(t *testing.T) {
	f := newFixture(t)
	rec := &updateRecorder{current: &sandrunv1.RunnerProfile{
		RunnerProfileId: "p1", Resources: &sandrunv1.Resources{VcpuCount: 4, MemoryMib: 8192, DiskGib: 64},
	}}
	path, h := sandrunv1connect.NewRunnerProfileServiceHandler(rec)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	// The SDK speaks HTTP/2 without TLS, like the sandruntest server.
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f.env[EnvAPIURL] = srv.URL

	for _, tc := range []struct {
		name    string
		flags   []string
		paths   []string
		profile *sandrunv1.RunnerProfile
	}{
		{
			name:  "vCPU and disk with image",
			flags: []string{"--vcpu", "8", "--disk-gib", "96", "--image", "linux"},
			paths: []string{"source", "resources.vcpu_count", "resources.disk_gib"},
			profile: &sandrunv1.RunnerProfile{
				Resources: &sandrunv1.Resources{VcpuCount: 8, DiskGib: 96},
				Source:    &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
			},
		},
		{
			name:    "memory only",
			flags:   []string{"--memory-mib", "16384"},
			paths:   []string{"resources.memory_mib"},
			profile: &sandrunv1.RunnerProfile{Resources: &sandrunv1.Resources{MemoryMib: 16384}},
		},
		{
			name:  "all resources with template",
			flags: []string{"--template", "tpl", "--vcpu", "2", "--memory-mib", "4096", "--disk-gib", "32"},
			paths: []string{"source", "resources"},
			profile: &sandrunv1.RunnerProfile{
				Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 32},
				Source:    &sandrunv1.RunnerProfile_OwnsandTemplateId{OwnsandTemplateId: "tpl"},
			},
		},
		{
			name:    "name only",
			flags:   []string{"--name", "renamed"},
			paths:   []string{"name"},
			profile: &sandrunv1.RunnerProfile{Name: "renamed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.run("", append([]string{"--org", f.org, "profile", "update", "p1"}, tc.flags...)...)
			if r.code != 0 {
				t.Fatalf("SandRun exited %d: %s%s", r.code, r.stdout, r.stderr)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.gets != 0 || len(rec.updates) != 1 {
				t.Fatalf("GetRunnerProfile calls = %d, UpdateRunnerProfile calls = %d, want 0 and 1", rec.gets, len(rec.updates))
			}
			got := rec.updates[0]
			rec.updates = nil
			if !slices.Equal(got.GetUpdateMask().GetPaths(), tc.paths) {
				t.Errorf("update paths = %v, want %v", got.GetUpdateMask().GetPaths(), tc.paths)
			}
			if !proto.Equal(got.GetProfile(), tc.profile) {
				t.Errorf("update profile = %v, want %v", got.GetProfile(), tc.profile)
			}
		})
	}
	if r := f.run("", "--org", f.org, "profile", "update", "p1"); r.code != ExitUsage {
		t.Fatalf("update without flags = %d", r.code)
	}
}
