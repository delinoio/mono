// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
)

func TestGetCapacityRunnerCounts(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := t.Context()
	w.fake.SetDevices(w.osOrg,
		&ownsandv1.Device{DeviceId: "device-0", Name: "linux-box", State: ownsandv1.DeviceState_DEVICE_STATE_ONLINE,
			HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, EffectiveVcpuLimitCount: 16,
			Limits:     &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 500, VcpuOvercommitPercent: 100},
			Allocation: &ownsandv1.DeviceAllocation{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100}},
		&ownsandv1.Device{DeviceId: "device-1", Name: "mac-box", State: ownsandv1.DeviceState_DEVICE_STATE_OFFLINE,
			HostOs: ownsandv1.HostOs_HOST_OS_MACOS, Arch: ownsandv1.Arch_ARCH_ARM64})
	client, _, err := w.l.Links.Client(ctx, w.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: api.NewID()})
	if err != nil {
		t.Fatal(err)
	}
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	busy := sandrunv1.RunnerState_RUNNER_STATE_BUSY
	waiting := sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	pool := w.env.DB.Pool
	for _, runner := range []struct {
		device string
		state  sandrunv1.RunnerState
	}{
		{"device-0", busy},
		{"device-0", busy},
		{"device-1", waiting},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
			idempotency_key, create_request, ownsand_sandbox_id, ownsand_device_id, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $1, '', $7, $8, now(), now())`,
			api.NewID(), w.org.ID, profileID, int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION),
			int16(sandrunv1.GuestOs_GUEST_OS_LINUX), int16(runner.state), sandbox.GetSandboxId(), runner.device); err != nil {
			t.Fatal(err)
		}
	}

	work := w.env.User(w.org.Member).Work
	want := &sandrunv1.GetCapacityResponse{Devices: []*sandrunv1.DeviceCapacity{
		{OwnsandDeviceId: "device-0", Name: "linux-box", State: sandrunv1.DeviceState_DEVICE_STATE_ONLINE,
			HostOs: "LINUX", Arch: "AMD64", EffectiveVcpuLimit: 16,
			Limits:     &sandrunv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 500, VcpuOvercommitPercent: 100},
			Allocation: &sandrunv1.DeviceAllocation{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100},
			Runners:    []*sandrunv1.RunnerStateCount{{State: busy, Count: 2}}},
		{OwnsandDeviceId: "device-1", Name: "mac-box", State: sandrunv1.DeviceState_DEVICE_STATE_OFFLINE,
			HostOs: "MACOS", Arch: "ARM64", Limits: &sandrunv1.DeviceLimits{}, Allocation: &sandrunv1.DeviceAllocation{},
			Runners: []*sandrunv1.RunnerStateCount{{State: waiting, Count: 1}}},
	}}
	checkCapacity := func(t *testing.T) {
		t.Helper()
		resp, err := work.GetCapacity(t.Context(), connect.NewRequest(&sandrunv1.GetCapacityRequest{OrganizationId: w.org.ID}))
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(resp.Msg, want) {
			t.Fatalf("GetCapacity returned %v, want %v", resp.Msg, want)
		}
	}
	t.Run("healthy", checkCapacity)

	// Run the real query with a test aggregate that fails on a selected group.
	// Sorted aggregation can send the first group's row before processing the second.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE capacity_count_failure (group_size bigint);
		INSERT INTO capacity_count_failure VALUES (0);
		CREATE FUNCTION capacity_count_result(total bigint) RETURNS bigint LANGUAGE plpgsql STABLE AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM capacity_count_failure WHERE group_size = total) THEN
				RAISE EXCEPTION 'injected capacity count failure';
			END IF;
			RETURN total;
		END;
		$$;
		CREATE AGGREGATE public.count(*) (
			SFUNC = pg_catalog.int8inc, STYPE = bigint, INITCOND = '0', FINALFUNC = capacity_count_result
		);`); err != nil {
		t.Fatal(err)
	}
	database := pgx.Identifier{pool.Config().ConnConfig.Database}.Sanitize()
	if _, err := pool.Exec(ctx, "ALTER DATABASE "+database+" SET search_path = public, pg_catalog; "+
		"ALTER DATABASE "+database+" SET enable_hashagg = off"); err != nil {
		t.Fatal(err)
	}
	pool.Reset()

	for _, tc := range []struct {
		name      string
		groupSize int64
	}{
		{"error before first row", 2},
		{"error after first row", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), "UPDATE capacity_count_failure SET group_size = $1", tc.groupSize); err != nil {
				t.Fatal(err)
			}
			resp, err := work.GetCapacity(t.Context(), connect.NewRequest(&sandrunv1.GetCapacityRequest{OrganizationId: w.org.ID}))
			if resp != nil || connect.CodeOf(err) != connect.CodeInternal {
				t.Fatalf("GetCapacity returned %v, %v, want nil response and internal error", resp, err)
			}
		})
	}
	if _, err := pool.Exec(ctx, "UPDATE capacity_count_failure SET group_size = 0"); err != nil {
		t.Fatal(err)
	}
	t.Run("recovered", checkCapacity)
}
