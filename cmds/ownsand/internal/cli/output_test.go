// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestPrintFieldsIntegersUseDecimalForm checks that text mode prints every
// integer in full decimal form. A float64 would print 1e+06 from one million
// upward.
func TestPrintFieldsIntegersUseDecimalForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  proto.Message
		want []string
	}{
		{"below one million", &ownsandv1.Resources{VcpuCount: 999999}, []string{"vcpu_count: 999999"}},
		{"one million", &ownsandv1.Resources{VcpuCount: 1000000}, []string{"vcpu_count: 1000000"}},
		{"negative million", &ownsandv1.Resources{VcpuCount: -1000000}, []string{"vcpu_count: -1000000"}},
		{"int32 minimum", &ownsandv1.Resources{VcpuCount: -2147483648}, []string{"vcpu_count: -2147483648"}},
		{"int32 maximum", &ownsandv1.Resources{VcpuCount: 2147483647}, []string{"vcpu_count: 2147483647"}},
		{
			"int64 stays exact",
			&ownsandv1.Resources{VcpuCount: 3, MemoryMib: 9007199254740993, DiskGib: 12},
			[]string{"vcpu_count: 3", "memory_mib: 9007199254740993", "disk_gib: 12"},
		},
		{
			"nested fields",
			&ownsandv1.Device{Limits: &ownsandv1.DeviceLimits{VcpuCount: 2147483647, VcpuOvercommitPercent: 100}, EffectiveVcpuLimit: 2147483647, EffectiveVcpuLimitCount: 2147483648},
			[]string{"effective_vcpu_limit: 2147483647", "effective_vcpu_limit_count: 2147483648", "limits.vcpu_count: 2147483647", "limits.vcpu_overcommit_percent: 100"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			a := &app{}
			if err := a.printFields(&out, tc.msg); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "e+") {
				t.Fatalf("exponent form in:\n%s", out.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want+"\n") {
					t.Errorf("output misses the line %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// TestFlattenKeepsTextFormats checks the text layout that the number fix
// must not change: strings, bools, nested keys, key order and simple
// repeated fields, which join their elements with a comma.
func TestFlattenKeepsTextFormats(t *testing.T) {
	v := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(`{"b":"text","a":{"y":true,"x":1000000},"list":[1000000,2,"s"],"objs":[{"k":1},{"k":2}]}`))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var lines [][2]string
	flatten("", v, &lines)
	got := map[string]string{}
	for _, l := range lines {
		got[l[0]] = l[1]
	}
	for k, want := range map[string]string{
		"b": "text", "a.y": "true", "a.x": "1000000", "list": "1000000, 2, s", "objs[0].k": "1", "objs[1].k": "2",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

// TestProtoValueJSONIsByteIdentical checks that the generic value still
// marshals to the bytes that the former float64 decoding produced.
func TestProtoValueJSONIsByteIdentical(t *testing.T) {
	m := &ownsandv1.Device{
		DeviceId:                "d",
		Name:                    "mac",
		Limits:                  &ownsandv1.DeviceLimits{VcpuCount: 2147483647, MemoryMib: 1, DiskGib: 1, VcpuOvercommitPercent: 100},
		EffectiveVcpuLimit:      2147483647,
		EffectiveVcpuLimitCount: 9007199254740993,
		Allocation:              &ownsandv1.DeviceAllocation{VcpuCount: -1000000},
	}
	raw, err := jsonOpts.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := writeJSON(&want, legacy); err != nil {
		t.Fatal(err)
	}
	v, err := protoValue(m)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeJSON(&got, v); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("JSON changed:\n got %s\nwant %s", got.String(), want.String())
	}
}
