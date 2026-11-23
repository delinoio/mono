// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// workWorld is an organization with an ACTIVE OwnSand link.
type workWorld struct {
	*world
	osOrg string
}

func devProfileProto(name string, warm int32, priority tidev1.Priority) *tidev1.RunnerProfile {
	return &tidev1.RunnerProfile{
		Name: name, WorkloadKind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/home/dev/work",
		GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, Source: &tidev1.RunnerProfile_BaseImage{BaseImage: tidev1.GuestOs_GUEST_OS_LINUX},
		Resources: &tidev1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(4 * time.Hour), MaxQueueWait: durationpb.New(time.Hour),
		WarmCount: warm, Priority: priority,
	}
}

func (w *workWorld) createProfile(t *testing.T, p *tidev1.RunnerProfile) string {
	t.Helper()
	resp, err := w.env.User(w.org.Admin).Profile.CreateRunnerProfile(context.Background(), connect.NewRequest(&tidev1.CreateRunnerProfileRequest{OrganizationId: w.org.ID, Profile: p}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetProfile().GetRunnerProfileId()
}

func (w *workWorld) runner(t *testing.T, id string) *tidev1.Runner {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).Work.GetRunner(context.Background(), connect.NewRequest(&tidev1.GetRunnerRequest{OrganizationId: w.org.ID, RunnerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRunner()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stays checks that cond holds for a while (several dispatcher passes).
func stays(t *testing.T, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(until) {
		if !cond() {
			t.Fatalf("%s changed", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
