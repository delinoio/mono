// SPDX-License-Identifier: FSL-1.1-ALv2

package link_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

type factoryFunc func(orgID, key string) (ownsandclient.Client, error)

func (f factoryFunc) New(orgID, key string) (ownsandclient.Client, error) { return f(orgID, key) }

func TestOrganizationDeletionEvictsOwnSandClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	seed := testenv.New(t)
	// Keep background work from building clients or processing the fixture
	// runner. Send all link and delete RPCs to the replica.
	for !seed.App.Lease.Held() {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("core instance did not acquire the lease")
		}
	}
	fake := ownsandfake.New()
	type pausedLookup struct {
		orgID   string
		entered chan struct{}
		resume  chan struct{}
	}
	var pause atomic.Pointer[pausedLookup]
	factory := factoryFunc(func(orgID, key string) (ownsandclient.Client, error) {
		if p := pause.Load(); p != nil && p.orgID == orgID && pause.CompareAndSwap(p, nil) {
			close(p.entered)
			select {
			case <-p.resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return fake.New(orgID, key)
	})
	var wired layers.Layers
	env := seed.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: factory}, &wired))
	newLink := func(t *testing.T, name string) (testenv.Org, string) {
		t.Helper()
		org, osOrg := env.NewOrg(name), api.NewID()
		key := "osk_" + strings.Repeat(name[:1], 43)
		fake.AddOrg(osOrg, key)
		_, err := env.User(org.Admin).OwnSandLink.SetOwnSandLink(ctx, connect.NewRequest(&sandrunv1.SetOwnSandLinkRequest{
			OrganizationId: org.ID, OwnsandOrganizationId: osOrg, OwnsandApiKey: key,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return org, osOrg
	}
	deleteOrg := func(org testenv.Org) error {
		_, err := env.User(org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		return err
	}
	client := func(t *testing.T, org testenv.Org) ownsandclient.Client {
		t.Helper()
		got, _, err := wired.Links.Client(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	unrelated, _ := newLink(t, "unrelated")
	keep := client(t, unrelated)
	assertDeleted := func(t *testing.T, org testenv.Org) {
		t.Helper()
		if wired.Links.CachedClientForTest(org.ID) != nil {
			t.Error("the deleted organization's OwnSand client is still cached")
		}
		if wired.Links.CachedClientForTest(unrelated.ID) != keep {
			t.Error("deletion changed the unrelated organization's cached client")
		}
		if _, ok, err := wired.Links.Get(ctx, env.DB.Pool, org.ID); err != nil || ok {
			t.Errorf("look up the deleted link: found = %v, error = %v", ok, err)
		}
		if _, _, err := wired.Links.Client(ctx, org.ID); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("look up the deleted organization's client: %v, want failed_precondition", err)
		}
	}

	t.Run("no runners", func(t *testing.T) {
		org, _ := newLink(t, "empty")
		if err := deleteOrg(org); err != nil {
			t.Fatal(err)
		}
		assertDeleted(t, org)
	})

	t.Run("lookup during deletion", func(t *testing.T) {
		org, osOrg := newLink(t, "concurrent")
		p := &pausedLookup{orgID: osOrg, entered: make(chan struct{}), resume: make(chan struct{})}
		pause.Store(p)
		resume := sync.OnceFunc(func() { close(p.resume) })
		defer resume()
		done := make(chan error, 1)
		go func() {
			_, _, err := wired.Links.Client(ctx, org.ID)
			done <- err
		}()
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("lookup did not reach the factory")
		}
		if err := deleteOrg(org); err != nil {
			t.Fatal(err)
		}
		assertDeleted(t, org)
		resume()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("finish the in-flight lookup: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("in-flight lookup did not finish")
		}
		assertDeleted(t, org)
	})

	t.Run("failed cleanup and retry", func(t *testing.T) {
		org, osOrg := newLink(t, "retry")
		profile, err := env.User(org.Admin).Profile.CreateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.CreateRunnerProfileRequest{
			OrganizationId: org.ID,
			Profile: &sandrunv1.RunnerProfile{
				Name: "dev", WorkloadKind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/home/dev/work",
				GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, Source: &sandrunv1.RunnerProfile_BaseImage{BaseImage: sandrunv1.GuestOs_GUEST_OS_LINUX},
				Resources: &sandrunv1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10}, NetworkMode: sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND,
				IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(4 * time.Hour), MaxQueueWait: durationpb.New(time.Hour),
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		primed := client(t, org)
		runnerID := api.NewID()
		sandbox, err := primed.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: runnerID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.DB.Pool.Exec(ctx, `INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
			idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $1, '', $7, now(), now())`,
			runnerID, org.ID, profile.Msg.GetProfile().GetRunnerProfileId(), int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION),
			int16(sandrunv1.GuestOs_GUEST_OS_LINUX), int16(sandrunv1.RunnerState_RUNNER_STATE_BUSY), sandbox.GetSandboxId()); err != nil {
			t.Fatal(err)
		}
		fake.Update(osOrg, func(o *ownsandfake.Org) {
			o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
		})
		if err := deleteOrg(org); err == nil {
			t.Fatal("deletion succeeded despite the sandbox cleanup failure")
		}
		if wired.Links.CachedClientForTest(org.ID) != primed || client(t, org) != primed {
			t.Fatal("failed cleanup changed the cached client")
		}
		fake.Update(osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
		if err := deleteOrg(org); err != nil {
			t.Fatal(err)
		}
		if fake.Active(osOrg) != 0 {
			t.Error("a sandbox survived the retry")
		}
		assertDeleted(t, org)
	})
}
