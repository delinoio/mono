// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }

// fullInput returns a complete valid ReplaceDeviceLimits input.
func fullInput() *ownsandv1.DeviceLimitsInput {
	return &ownsandv1.DeviceLimitsInput{VcpuCount: i32(6), MemoryMib: i64(4096), DiskGib: i64(50), VcpuOvercommitPercent: i32(200)}
}

// deviceWriteCounts returns the number of configuration events and the
// number of DEVICE_LIMITS_CHANGED audit records of one device.
func deviceWriteCounts(t *testing.T, env *testenv.Env, orgID, deviceID string) (events, audits int) {
	t.Helper()
	ctx := context.Background()
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE kind = 4 AND device_id = $1", deviceID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2 AND target_id = $3",
		orgID, int32(ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_LIMITS_CHANGED), deviceID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	return events, audits
}

// TestReplaceDeviceLimitsRequiresEveryField covers DeviceLimitsInput presence
// over the binary protobuf codec and Connect JSON: each field omitted in
// turn is rejected with invalid_argument that names the field, before any
// write.
func TestReplaceDeviceLimitsRequiresEveryField(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	clients := map[string]ownsandv1connect.DeviceServiceClient{
		"binary": org.OwnerClient.Device,
		"json":   env.JSONDeviceClient(testenv.UserToken(org.Owner)),
	}
	// Start from known stored limits so an unwanted write is visible.
	if _, err := org.OwnerClient.Device.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{
		OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimitsInput{VcpuCount: i32(2), MemoryMib: i64(2048), DiskGib: i64(20), VcpuOvercommitPercent: i32(100)},
	})); err != nil {
		t.Fatal(err)
	}
	wantLimits := &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 2048, DiskGib: 20, VcpuOvercommitPercent: 100}
	baseEvents, baseAudits := deviceWriteCounts(t, env, org.ID, dev.ID)

	cases := []struct {
		field string
		clear func(*ownsandv1.DeviceLimitsInput)
	}{
		{"vcpu_count", func(l *ownsandv1.DeviceLimitsInput) { l.VcpuCount = nil }},
		{"memory_mib", func(l *ownsandv1.DeviceLimitsInput) { l.MemoryMib = nil }},
		{"disk_gib", func(l *ownsandv1.DeviceLimitsInput) { l.DiskGib = nil }},
		{"vcpu_overcommit_percent", func(l *ownsandv1.DeviceLimitsInput) { l.VcpuOvercommitPercent = nil }},
	}
	for name, c := range clients {
		for _, tc := range cases {
			limits := fullInput()
			tc.clear(limits)
			_, err := c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: limits}))
			if testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits."+tc.field) {
				t.Errorf("%s: omit %s: %v", name, tc.field, err)
			}
			got, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
			if problem := readbackLimitsProblem(got, err, wantLimits); problem != "" {
				t.Errorf("%s: omit %s: %s", name, tc.field, problem)
			}
			if e, a := deviceWriteCounts(t, env, org.ID, dev.ID); e != baseEvents || a != baseAudits {
				t.Errorf("%s: omit %s: events = %d, audits = %d, want %d, %d", name, tc.field, e, a, baseEvents, baseAudits)
			}
		}
		// A missing limits message is rejected the same way.
		_, err := c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
		if testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits") {
			t.Errorf("%s: no limits: %v", name, err)
		}
		// An empty message names the first missing field.
		_, err = c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: &ownsandv1.DeviceLimitsInput{}}))
		if testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits.vcpu_count") {
			t.Errorf("%s: empty limits: %v", name, err)
		}
		// Numeric rules still apply to a complete request.
		bad := fullInput()
		bad.VcpuOvercommitPercent = i32(110)
		_, err = c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: bad}))
		if testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits.vcpu_overcommit_percent") {
			t.Errorf("%s: bad overcommit: %v", name, err)
		}
		bad = fullInput()
		bad.DiskGib = i64(-1)
		_, err = c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: bad}))
		if testenv.Code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits.disk_gib") {
			t.Errorf("%s: negative disk: %v", name, err)
		}
	}
	if e, a := deviceWriteCounts(t, env, org.ID, dev.ID); e != baseEvents || a != baseAudits {
		t.Fatalf("events = %d, audits = %d after rejections, want %d, %d", e, a, baseEvents, baseAudits)
	}
}

// TestReplaceDeviceLimitsAcceptsExplicitZero checks that an explicit zero of
// each of vcpu_count, memory_mib and disk_gib is stored, over both codecs,
// and that the device then cannot accept a sandbox that needs the resource.
func TestReplaceDeviceLimitsAcceptsExplicitZero(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	_ = agent
	clients := map[string]ownsandv1connect.DeviceServiceClient{
		"binary": org.OwnerClient.Device,
		"json":   env.JSONDeviceClient(testenv.UserToken(org.Owner)),
	}
	zero := map[string]func(*ownsandv1.DeviceLimitsInput){
		"vcpu_count": func(l *ownsandv1.DeviceLimitsInput) { l.VcpuCount = i32(0) },
		"memory_mib": func(l *ownsandv1.DeviceLimitsInput) { l.MemoryMib = i64(0) },
		"disk_gib":   func(l *ownsandv1.DeviceLimitsInput) { l.DiskGib = i64(0) },
	}
	// The scheduler rejects the first limit the request exceeds, checked in
	// the order vCPU, memory, disk. With one limit zeroed and the others
	// large enough, exactly one check fires and its message is pinned here.
	wantMessage := map[string]string{
		"vcpu_count": "no device can run the sandbox: the request has more vCPUs than the device vCPU limit",
		"memory_mib": "no device can run the sandbox: the request needs more memory than the device limit",
		"disk_gib":   "no device can run the sandbox: the request needs more disk than the device limit",
	}
	for name, c := range clients {
		for field, set := range zero {
			baseEvents, baseAudits := deviceWriteCounts(t, env, org.ID, dev.ID)
			limits := &ownsandv1.DeviceLimitsInput{VcpuCount: i32(4), MemoryMib: i64(8192), DiskGib: i64(100), VcpuOvercommitPercent: i32(100)}
			set(limits)
			resp, err := c.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: limits}))
			if err != nil {
				t.Fatalf("%s: zero %s: %v", name, field, err)
			}
			want := &ownsandv1.DeviceLimits{VcpuCount: limits.GetVcpuCount(), MemoryMib: limits.GetMemoryMib(), DiskGib: limits.GetDiskGib(), VcpuOvercommitPercent: 100}
			if !proto.Equal(resp.Msg.GetDevice().GetLimits(), want) {
				t.Fatalf("%s: zero %s: response limits = %v, want %v", name, field, resp.Msg.GetDevice().GetLimits(), want)
			}
			got, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
			if problem := readbackLimitsProblem(got, err, want); problem != "" {
				t.Fatalf("%s: zero %s: %s", name, field, problem)
			}
			if e, a := deviceWriteCounts(t, env, org.ID, dev.ID); e != baseEvents+1 || a != baseAudits+1 {
				t.Fatalf("%s: zero %s: events = %d, audits = %d, want %d, %d", name, field, e, a, baseEvents+1, baseAudits+1)
			}
			// The device holds the stored zero, so it cannot take a sandbox that
			// needs the resource (createReq needs 2 vCPUs, 1024 MiB and 10 GiB).
			_, err = org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err == nil {
				t.Fatalf("%s: zero %s: a sandbox was placed on the device", name, field)
			}
			var ce *connect.Error
			if code := testenv.Code(err); code != connect.CodeFailedPrecondition || !errors.As(err, &ce) || ce.Message() != wantMessage[field] {
				t.Fatalf("%s: zero %s: create sandbox = %v (code %v), want %v: %q", name, field, err, code, connect.CodeFailedPrecondition, wantMessage[field])
			}
		}
	}
}

// TestReplaceDeviceLimitsUpdatesAllFour checks a complete positive request
// and its response, configuration event, audit record and agent
// configuration.
func TestReplaceDeviceLimitsUpdatesAllFour(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev, agent := setupAgent(t, env, org, nil)
	baseEvents, baseAudits := deviceWriteCounts(t, env, org.ID, dev.ID)
	resp, err := org.OwnerClient.Device.ReplaceDeviceLimits(ctx, connect.NewRequest(&ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: fullInput()}))
	if err != nil {
		t.Fatal(err)
	}
	want := &ownsandv1.DeviceLimits{VcpuCount: 6, MemoryMib: 4096, DiskGib: 50, VcpuOvercommitPercent: 200}
	d := resp.Msg.GetDevice()
	if d.GetDeviceId() != dev.ID || !proto.Equal(d.GetLimits(), want) || d.GetEffectiveVcpuLimitCount() != 12 || d.GetEffectiveVcpuLimit() != 12 {
		t.Fatalf("device = %v", d)
	}
	got, err := org.OwnerClient.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}))
	if problem := readbackLimitsProblem(got, err, want); problem != "" {
		t.Fatal(problem)
	}
	if e, a := deviceWriteCounts(t, env, org.ID, dev.ID); e != baseEvents+1 || a != baseAudits+1 {
		t.Fatalf("events = %d, audits = %d, want %d, %d", e, a, baseEvents+1, baseAudits+1)
	}
	// The change reaches the connected agent as a new configuration.
	agent.WaitFor("new configuration", func(e *ownsandv1.AgentEnvelope) bool {
		l := e.GetConfiguration().GetLimits()
		return l.GetVcpuCount() == 6 && l.GetMemoryMib() == 4096 && l.GetDiskGib() == 50 && l.GetVcpuOvercommitPercent() == 200
	})
}

// TestLegacyUpdateDeviceLimitsKeepsImplicitPresence checks that v1 clients of
// UpdateDeviceLimits still work, including an intentional zero and an
// omitted value that reads as zero.
func TestLegacyUpdateDeviceLimitsKeepsImplicitPresence(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	legacy := func(l *ownsandv1.DeviceLimits) (*ownsandv1.Device, error) {
		resp, err := org.OwnerClient.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{OrganizationId: org.ID, DeviceId: dev.ID, Limits: l}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetDevice(), nil
	}
	if _, err := legacy(&ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 150}); err != nil {
		t.Fatal(err)
	}
	// An intentional zero (the binary codec omits it on the wire) still works.
	d, err := legacy(&ownsandv1.DeviceLimits{VcpuCount: 0, MemoryMib: 8192, DiskGib: 100, VcpuOvercommitPercent: 150})
	if err != nil || d.GetLimits().GetVcpuCount() != 0 || d.GetLimits().GetMemoryMib() != 8192 {
		t.Fatalf("zero vcpu: %v, %v", d, err)
	}
	// An omitted value is stored as zero: the legacy default.
	d, err = legacy(&ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 1024, VcpuOvercommitPercent: 100})
	if err != nil || d.GetLimits().GetDiskGib() != 0 {
		t.Fatalf("omitted disk: %v, %v", d, err)
	}
	if _, err := legacy(nil); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("nil limits: %v", err)
	}
	if _, err := legacy(&ownsandv1.DeviceLimits{VcpuCount: 1, VcpuOvercommitPercent: 0}); testenv.Code(err) != connect.CodeInvalidArgument {
		t.Fatalf("zero overcommit: %v", err)
	}
}
