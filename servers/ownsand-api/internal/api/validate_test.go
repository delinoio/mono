// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"bytes"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestValidateTTL(t *testing.T) {
	d := durationpb.New
	cases := []struct {
		name      string
		idle, max *durationpb.Duration
		ok        bool
	}{
		{"valid", d(time.Hour), d(2 * time.Hour), true},
		{"equal", d(time.Hour), d(time.Hour), true},
		{"exactly 7 days", d(time.Hour), d(7 * 24 * time.Hour), true},
		{"over 7 days", d(time.Hour), d(7*24*time.Hour + time.Nanosecond), false},
		{"idle above max", d(2 * time.Hour), d(time.Hour), false},
		{"missing idle", nil, d(time.Hour), false},
		{"missing max", d(time.Hour), nil, false},
		{"zero", d(0), d(time.Hour), false},
		{"negative", d(-time.Second), d(time.Hour), false},
		{"invalid duration", &durationpb.Duration{Seconds: 1, Nanos: -1}, d(time.Hour), false},
	}
	for _, tc := range cases {
		_, _, err := ValidateTTL(tc.idle, tc.max)
		if (err == nil) != tc.ok || (err != nil && connect.CodeOf(err) != connect.CodeInvalidArgument) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func validCreate() *ownsandv1.CreateSandboxRequest {
	return &ownsandv1.CreateSandboxRequest{
		OrganizationId: "o", IdempotencyKey: datapath.NewID(),
		Source:    &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 5},
		IdleTtl:   durationpb.New(time.Hour), MaxTtl: durationpb.New(time.Hour),
	}
}

func TestValidateCreate(t *testing.T) {
	p, err := ValidateCreate(validCreate())
	if err != nil || p.NetworkMode != ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		t.Fatalf("valid: %v", err)
	}
	// The idempotency hash ignores the key and covers every parameter.
	other := validCreate()
	other.IdempotencyKey = datapath.NewID()
	p2, _ := ValidateCreate(other)
	other.Resources.VcpuCount = 2
	p3, _ := ValidateCreate(other)
	if !bytes.Equal(p.IdempotencyHash, p2.IdempotencyHash) || bytes.Equal(p.IdempotencyHash, p3.IdempotencyHash) {
		t.Fatal("idempotency hash")
	}
	long := make([]byte, 64)
	for i := range long {
		long[i] = 'k'
	}
	bad := map[string]func(*ownsandv1.CreateSandboxRequest){
		"key not v7":        func(r *ownsandv1.CreateSandboxRequest) { r.IdempotencyKey = "00000000-0000-4000-8000-000000000000" },
		"no source":         func(r *ownsandv1.CreateSandboxRequest) { r.Source = nil },
		"unspecified image": func(r *ownsandv1.CreateSandboxRequest) { r.Source.Source = &ownsandv1.SandboxSource_BaseImage{} },
		"bad snapshot id": func(r *ownsandv1.CreateSandboxRequest) {
			r.Source.Source = &ownsandv1.SandboxSource_SnapshotId{SnapshotId: "x"}
		},
		"zero vcpu":           func(r *ownsandv1.CreateSandboxRequest) { r.Resources.VcpuCount = 0 },
		"missing resources":   func(r *ownsandv1.CreateSandboxRequest) { r.Resources = nil },
		"unspecified network": func(r *ownsandv1.CreateSandboxRequest) { r.Network = &ownsandv1.NetworkPolicy{} },
		"missing ttl":         func(r *ownsandv1.CreateSandboxRequest) { r.MaxTtl = nil },
		"long label key":      func(r *ownsandv1.CreateSandboxRequest) { r.Labels = map[string]string{string(long): "v"} },
		"unspecified host": func(r *ownsandv1.CreateSandboxRequest) {
			h := ownsandv1.HostOs(0)
			r.Placement = &ownsandv1.Placement{HostOs: &h}
		},
		"too many labels": func(r *ownsandv1.CreateSandboxRequest) {
			r.Labels = map[string]string{}
			for i := 0; i < 33; i++ {
				r.Labels[string(rune('a'+i%26))+string(rune('A'+i/26))] = "v"
			}
		},
	}
	for name, mut := range bad {
		r := validCreate()
		mut(r)
		if _, err := ValidateCreate(r); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	none := validCreate()
	none.Network = &ownsandv1.NetworkPolicy{Mode: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
	if p, err := ValidateCreate(none); err != nil || p.NetworkMode != ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		t.Fatalf("network none: %v", err)
	}
}
