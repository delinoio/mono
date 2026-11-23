// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// readbackLimitsProblem describes why a GetDevice readback does not show the
// wanted limits, or returns "" when it does. A failed unary call returns a nil
// response, so the error is checked before the response is touched; a test
// that formats got.Msg in its failure message would panic on a nil response
// and hide the original error.
func readbackLimitsProblem(got *connect.Response[ownsandv1.GetDeviceResponse], err error, want *ownsandv1.DeviceLimits) string {
	if err != nil {
		return fmt.Sprintf("GetDevice: %v", err)
	}
	if got == nil || got.Msg == nil {
		return "GetDevice: empty response"
	}
	if actual := got.Msg.GetDevice().GetLimits(); !proto.Equal(actual, want) {
		return fmt.Sprintf("stored limits = %v, want %v", actual, want)
	}
	return ""
}

func TestReadbackLimitsProblem(t *testing.T) {
	want := &ownsandv1.DeviceLimits{VcpuCount: 2, MemoryMib: 2048, DiskGib: 20, VcpuOvercommitPercent: 100}
	response := func(l *ownsandv1.DeviceLimits) *connect.Response[ownsandv1.GetDeviceResponse] {
		return connect.NewResponse(&ownsandv1.GetDeviceResponse{Device: &ownsandv1.Device{Limits: l}})
	}
	other := &ownsandv1.DeviceLimits{VcpuCount: 3, MemoryMib: 2048, DiskGib: 20, VcpuOvercommitPercent: 100}
	injected := errors.New("injected readback failure")

	if p := readbackLimitsProblem(response(want), nil, want); p != "" {
		t.Errorf("matching readback reported %q", p)
	}

	// A failed RPC returns a nil response: the helper must report the error
	// and must not dereference the response.
	failure := connect.NewError(connect.CodeUnavailable, injected)
	p := readbackLimitsProblem(nil, failure, want)
	if p == "" {
		t.Fatal("failed readback reported no problem")
	}
	if want := "GetDevice: " + failure.Error(); p != want {
		t.Errorf("failed readback problem = %q, want %q", p, want)
	}

	p = readbackLimitsProblem(response(other), nil, want)
	if wantProblem := fmt.Sprintf("stored limits = %v, want %v", other, want); p != wantProblem {
		t.Errorf("mismatching readback problem = %q, want %q", p, wantProblem)
	}

	if p := readbackLimitsProblem(nil, nil, want); p == "" {
		t.Error("nil response without an error reported no problem")
	}
}
