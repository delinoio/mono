// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestReplaceDeviceLimitsAgainstTheRealControlPlane drives ReplaceDeviceLimits
// on the real control plane process over the SDK (binary protobuf) and over
// Connect JSON, and checks that a missing value is rejected without a write,
// that an explicit zero is stored and that an API key is denied.
func TestReplaceDeviceLimitsAgainstTheRealControlPlane(t *testing.T) {
	s := newStack(t)
	ctx := s.ctx(2 * time.Minute)
	user := s.userClient(ownsand.TransportAuto)
	stored := func() *ownsandv1.DeviceLimits {
		t.Helper()
		res, err := user.Devices().Get(ctx, &ownsandv1.GetDeviceRequest{OrganizationId: s.orgID, DeviceId: s.deviceID})
		if err != nil {
			t.Fatal(err)
		}
		return res.GetDevice().GetLimits()
	}
	want := &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 100, VcpuOvercommitPercent: 100} // set by the harness
	if got := stored(); !proto.Equal(got, want) {
		t.Fatalf("initial limits = %v", got)
	}
	full := func() *ownsandv1.DeviceLimitsInput {
		return &ownsandv1.DeviceLimitsInput{VcpuCount: proto.Int32(2), MemoryMib: proto.Int64(2048), DiskGib: proto.Int64(20), VcpuOvercommitPercent: proto.Int32(150)}
	}
	omitters := map[string]func(*ownsandv1.DeviceLimitsInput){
		"vcpu_count":              func(l *ownsandv1.DeviceLimitsInput) { l.VcpuCount = nil },
		"memory_mib":              func(l *ownsandv1.DeviceLimitsInput) { l.MemoryMib = nil },
		"disk_gib":                func(l *ownsandv1.DeviceLimitsInput) { l.DiskGib = nil },
		"vcpu_overcommit_percent": func(l *ownsandv1.DeviceLimitsInput) { l.VcpuOvercommitPercent = nil },
	}
	// Binary protobuf through the SDK.
	for field, omit := range omitters {
		l := full()
		omit(l)
		_, err := user.Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: s.orgID, DeviceId: s.deviceID, Limits: l})
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limits."+field) {
			t.Errorf("binary: omit %s: %v", field, err)
		}
	}
	// Connect JSON on the wire: an omitted field is absent from the object.
	post := func(token string, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+"/delino.ownsand.v1.DeviceService/ReplaceDeviceLimits", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}
	jsonLimits := func(omit string) map[string]any {
		l := map[string]any{"vcpuCount": 2, "memoryMib": "2048", "diskGib": "20", "vcpuOvercommitPercent": 150}
		keys := map[string]string{"vcpu_count": "vcpuCount", "memory_mib": "memoryMib", "disk_gib": "diskGib", "vcpu_overcommit_percent": "vcpuOvercommitPercent"}
		delete(l, keys[omit])
		return map[string]any{"organizationId": s.orgID, "deviceId": s.deviceID, "limits": l}
	}
	for field := range omitters {
		status, body := post(s.userToken(), jsonLimits(field))
		if status != http.StatusBadRequest || !strings.Contains(body, "invalid_argument") || !strings.Contains(body, "limits."+field) {
			t.Errorf("json: omit %s: %d %s", field, status, body)
		}
	}
	if got := stored(); !proto.Equal(got, want) {
		t.Fatalf("limits changed after rejected requests: %v", got)
	}
	// Explicit zeros on the wire as JSON (a present 0) are stored.
	status, body := post(s.userToken(), map[string]any{"organizationId": s.orgID, "deviceId": s.deviceID,
		"limits": map[string]any{"vcpuCount": 0, "memoryMib": "0", "diskGib": "0", "vcpuOvercommitPercent": 100}})
	if status != http.StatusOK {
		t.Fatalf("json explicit zeros: %d %s", status, body)
	}
	if got := stored(); got.GetVcpuCount() != 0 || got.GetMemoryMib() != 0 || got.GetDiskGib() != 0 || got.GetVcpuOvercommitPercent() != 100 {
		t.Fatalf("limits after explicit zeros = %v", got)
	}
	// A complete request restores positive limits, over binary protobuf.
	if _, err := user.Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: s.orgID, DeviceId: s.deviceID, Limits: full()}); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got.GetVcpuCount() != 2 || got.GetMemoryMib() != 2048 || got.GetDiskGib() != 20 || got.GetVcpuOvercommitPercent() != 150 {
		t.Fatalf("limits after a complete request = %v", got)
	}
	// An API key credential is not accepted on this RPC.
	_, err := s.apiKeyClient(ownsand.TransportAuto).Devices().ReplaceLimits(ctx, &ownsandv1.ReplaceDeviceLimitsRequest{OrganizationId: s.orgID, DeviceId: s.deviceID, Limits: full()})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("api key: %v", err)
	}
}
