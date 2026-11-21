// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	orgID     = "0192f4b6-7c3a-7def-8abc-000000000001"
	sandboxID = "0192f4b6-7c3a-7def-8abc-000000000002"
	opID      = "0192f4b6-7c3a-7def-8abc-000000000003"
)

func wantInvalid(t *testing.T, name string, err error, field string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want a validation error", name, err)
		return
	}
	if field != "" && e.Field != field {
		t.Errorf("%s: field = %s, want %s", name, e.Field, field)
	}
	if e.Code() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || e.Detail().GetCode() != e.Code() {
		t.Errorf("%s: code %s", name, e.Code())
	}
}

func validExec() *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{
		OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID,
		Command: []string{"/bin/sh", "-c", "true"}, Env: map[string]string{"A": "B"},
		WorkingDirectory: "/", Timeout: durationpb.New(time.Minute),
		BindingSalt: make([]byte, 32),
	}
}

func TestExecRequest(t *testing.T) {
	if err := ExecRequest(validExec()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*ownsandv1.ExecRequest)
		field  string
	}{
		{"missing exec_id", func(r *ownsandv1.ExecRequest) { r.ExecId = "" }, "exec_id"},
		{"v4 exec_id", func(r *ownsandv1.ExecRequest) { r.ExecId = "0192f4b6-7c3a-4def-8abc-000000000003" }, "exec_id"},
		{"uppercase exec_id", func(r *ownsandv1.ExecRequest) { r.ExecId = strings.ToUpper(opID) }, "exec_id"},
		{"mixed-case exec_id", func(r *ownsandv1.ExecRequest) { r.ExecId = "0192f4b6-7c3a-7DEF-8abc-000000000003" }, "exec_id"},
		{"bad sandbox", func(r *ownsandv1.ExecRequest) { r.SandboxId = "x" }, "sandbox_id"},
		{"bad organization", func(r *ownsandv1.ExecRequest) { r.OrganizationId = "" }, "organization_id"},
		{"no command", func(r *ownsandv1.ExecRequest) { r.Command = nil }, "command"},
		{"empty program", func(r *ownsandv1.ExecRequest) { r.Command = []string{""} }, "command"},
		{"negative timeout", func(r *ownsandv1.ExecRequest) { r.Timeout = durationpb.New(-time.Second) }, "timeout"},
		{"invalid timeout", func(r *ownsandv1.ExecRequest) { r.Timeout = &durationpb.Duration{Seconds: 1, Nanos: -1} }, "timeout"},
		{"short salt", func(r *ownsandv1.ExecRequest) { r.BindingSalt = make([]byte, 31) }, "binding_salt"},
	}
	for _, tc := range cases {
		r := validExec()
		tc.change(r)
		wantInvalid(t, tc.name, ExecRequest(r), tc.field)
	}
}

func TestExecRequestSizeLimit(t *testing.T) {
	r := validExec()
	r.Env = nil
	r.WorkingDirectory = ""
	r.Command = []string{"echo", ""}
	// Grow the argument until the encoded fields are exactly at the limit.
	r.Command[1] = strings.Repeat("x", MaxExecFieldsSize-ExecFieldsSize(r))
	for ExecFieldsSize(r) > MaxExecFieldsSize {
		r.Command[1] = r.Command[1][1:]
	}
	for ExecFieldsSize(r) < MaxExecFieldsSize {
		r.Command[1] += "x"
	}
	if ExecFieldsSize(r) != MaxExecFieldsSize {
		t.Fatalf("size %d", ExecFieldsSize(r))
	}
	if err := ExecRequest(r); err != nil {
		t.Fatalf("exactly 256 KiB rejected: %v", err)
	}
	r.Command[1] += "x"
	wantInvalid(t, "over limit", ExecRequest(r), "command")
	// The limit counts env and working_directory too.
	r = validExec()
	r.Env = map[string]string{"BIG": strings.Repeat("v", MaxExecFieldsSize)}
	wantInvalid(t, "env over limit", ExecRequest(r), "command")
	// Fields outside the three do not count.
	r = validExec()
	before := ExecFieldsSize(r)
	r.BindingSalt = make([]byte, 32)
	r.OrganizationId = orgID
	if ExecFieldsSize(r) != before {
		t.Fatal("other fields counted")
	}
}

func TestExecIDRequiresLowercase(t *testing.T) {
	if err := ExecID("exec_id", opID); err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(opID)
	err := ExecID("exec_id", upper)
	wantInvalid(t, "uppercase", err, "exec_id")
	if strings.Contains(err.Error(), upper) {
		t.Errorf("error must omit the value: %v", err)
	}
	wantInvalid(t, "empty", ExecID("exec_id", ""), "exec_id")
	wantInvalid(t, "v4", ExecID("exec_id", "0192f4b6-7c3a-4def-8abc-000000000003"), "exec_id")
	// Other UUID fields stay case tolerant.
	if err := UUIDv7("sandbox_id", upper); err != nil {
		t.Errorf("UUIDv7 must accept uppercase: %v", err)
	}
	if err := ExecRequest(func() *ownsandv1.ExecRequest { r := validExec(); r.SandboxId = upper; return r }()); err != nil {
		t.Errorf("uppercase sandbox_id: %v", err)
	}
}

func TestAttachReadWrite(t *testing.T) {
	if err := AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID}); err != nil {
		t.Fatal(err)
	}
	if err := AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID, StdoutOffset: 1 << 40, StderrOffset: 7}); err != nil {
		t.Fatal(err)
	}
	wantInvalid(t, "attach uppercase exec_id", AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: strings.ToUpper(opID)}), "exec_id")
	wantInvalid(t, "negative stderr offset", AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID, StderrOffset: -1}), "stderr_offset")
	wantInvalid(t, "negative stdout offset", AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID, StdoutOffset: -1}), "stdout_offset")
	wantInvalid(t, "both offsets negative", AttachExecRequest(&ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: sandboxID, ExecId: opID, StdoutOffset: -1, StderrOffset: -1}), "stdout_offset")

	read := &ownsandv1.ReadFileRequest{OrganizationId: orgID, SandboxId: sandboxID, ReadId: opID, Path: "/etc/hosts", BindingSalt: make([]byte, 32)}
	if err := ReadFileRequest(read); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{`C:\Users\x`, "C:/x", `\\server\share`, `\/server/share`, `\??\C:\file.txt`} {
		r := proto.Clone(read).(*ownsandv1.ReadFileRequest)
		r.Path = path
		if err := ReadFileRequest(r); err != nil {
			t.Errorf("path %q: %v", path, err)
		}
	}
	for _, path := range []string{"", "relative/file", "C:", "\xff", `\file.txt`, `\`, `\?x`} {
		r := proto.Clone(read).(*ownsandv1.ReadFileRequest)
		r.Path = path
		wantInvalid(t, "path "+path, ReadFileRequest(r), "path")
	}
	if err := ReadFileRequest(proto.Clone(read).(*ownsandv1.ReadFileRequest)); err != nil {
		t.Fatal(err)
	}
	r := proto.Clone(read).(*ownsandv1.ReadFileRequest)
	r.Path = `\file.txt`
	if err := ReadFileRequest(r); err == nil || strings.Contains(err.Error(), "file.txt") {
		t.Errorf("root-relative path error must exist and omit the value: %v", err)
	}
	r = proto.Clone(read).(*ownsandv1.ReadFileRequest)
	r.BindingSalt = nil
	wantInvalid(t, "read without salt", ReadFileRequest(r), "binding_salt")

	first := &ownsandv1.WriteFileRequest{OrganizationId: orgID, SandboxId: sandboxID, WriteId: opID, Path: "/tmp/f", BindingSalt: make([]byte, 32), Data: []byte("x")}
	if err := WriteFileRequest(first, true); err != nil {
		t.Fatal(err)
	}
	rootRel := proto.Clone(first).(*ownsandv1.WriteFileRequest)
	rootRel.Path = `\file.txt`
	wantInvalid(t, "write first root-relative", WriteFileRequest(rootRel, true), "path")
	wantInvalid(t, "write later root-relative", WriteFileRequest(rootRel, false), "path")
	later := &ownsandv1.WriteFileRequest{OrganizationId: orgID, SandboxId: sandboxID, WriteId: opID, Offset: 1, Data: []byte("y"), Finish: &ownsandv1.WriteFileFinish{SizeBytes: 2, Mode: 0o644}}
	if err := WriteFileRequest(later, false); err != nil {
		t.Fatal(err)
	}
	wantInvalid(t, "first without path", WriteFileRequest(later, true), "path")
	big := proto.Clone(first).(*ownsandv1.WriteFileRequest)
	big.Data = make([]byte, 1<<20+1)
	wantInvalid(t, "chunk over 1 MiB", WriteFileRequest(big, true), "data")
	badMode := proto.Clone(later).(*ownsandv1.WriteFileRequest)
	badMode.Finish.Mode = 0o10000
	wantInvalid(t, "mode", WriteFileRequest(badMode, false), "finish.mode")
}

func TestLabels(t *testing.T) {
	labels := map[string]string{}
	for i := 0; i < MaxLabels; i++ {
		labels[strings.Repeat("k", 1+i)] = strings.Repeat("v", MaxLabelValueSize)
	}
	labels[strings.Repeat("k", MaxLabelKeySize)] = ""
	delete(labels, strings.Repeat("k", 1))
	if err := Labels(labels); err != nil {
		t.Fatalf("limits rejected: %v", err)
	}
	labels["extra"] = "x"
	wantInvalid(t, "33 labels", Labels(labels), "labels")
	wantInvalid(t, "long key", Labels(map[string]string{strings.Repeat("k", 64): ""}), "labels")
	wantInvalid(t, "long value", Labels(map[string]string{"k": strings.Repeat("v", 256)}), "labels")
	wantInvalid(t, "empty key", Labels(map[string]string{"": "v"}), "labels")
	// The error never contains the label value.
	err := Labels(map[string]string{"k": strings.Repeat("secret", 50)})
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("error message contains the value")
	}
}

func TestTTL(t *testing.T) {
	d := durationpb.New
	if err := TTL(d(time.Hour), d(7*24*time.Hour)); err != nil {
		t.Fatalf("7 days rejected: %v", err)
	}
	if err := TTL(d(time.Hour), d(time.Hour)); err != nil {
		t.Fatalf("equal TTLs rejected: %v", err)
	}
	wantInvalid(t, "missing idle", TTL(nil, d(time.Hour)), "idle_ttl")
	wantInvalid(t, "missing max", TTL(d(time.Hour), nil), "max_ttl")
	wantInvalid(t, "zero idle", TTL(d(0), d(time.Hour)), "idle_ttl")
	wantInvalid(t, "negative max", TTL(d(time.Hour), d(-time.Hour)), "max_ttl")
	wantInvalid(t, "over 7 days", TTL(d(time.Hour), d(7*24*time.Hour+time.Nanosecond)), "max_ttl")
	wantInvalid(t, "idle over max", TTL(d(2*time.Hour), d(time.Hour)), "idle_ttl")
}

func TestResourcesSourceNetwork(t *testing.T) {
	if err := Resources(&ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 1}); err != nil {
		t.Fatal(err)
	}
	wantInvalid(t, "nil", Resources(nil), "resources")
	wantInvalid(t, "zero vcpu", Resources(&ownsandv1.Resources{MemoryMib: 1, DiskGib: 1}), "resources.vcpu_count")
	wantInvalid(t, "zero memory", Resources(&ownsandv1.Resources{VcpuCount: 1, DiskGib: 1}), "resources.memory_mib")
	wantInvalid(t, "zero disk", Resources(&ownsandv1.Resources{VcpuCount: 1, MemoryMib: 1}), "resources.disk_gib")

	if err := SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}}); err != nil {
		t.Fatal(err)
	}
	if err := SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: opID}}); err != nil {
		t.Fatal(err)
	}
	wantInvalid(t, "no source", SandboxSource(nil), "source")
	wantInvalid(t, "unspecified image", SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{}}), "source.base_image")
	wantInvalid(t, "unknown image", SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: 42}}), "source.base_image")
	wantInvalid(t, "bad template", SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: "t"}}), "source.template_id")
	// A typed nil wrapper inside the oneof must be reported, not dereferenced.
	wantInvalid(t, "typed nil base image", SandboxSource(&ownsandv1.SandboxSource{Source: (*ownsandv1.SandboxSource_BaseImage)(nil)}), "source")
	wantInvalid(t, "typed nil snapshot id", SandboxSource(&ownsandv1.SandboxSource{Source: (*ownsandv1.SandboxSource_SnapshotId)(nil)}), "source")
	wantInvalid(t, "typed nil template id", SandboxSource(&ownsandv1.SandboxSource{Source: (*ownsandv1.SandboxSource_TemplateId)(nil)}), "source")
	if err := SandboxSource(&ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: opID}}); err != nil {
		t.Fatal(err)
	}

	if mode, err := NetworkMode(nil); err != nil || mode != ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		t.Fatalf("absent network: %v %v", mode, err)
	}
	if mode, err := NetworkMode(&ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_NONE}); err != nil || mode != ownsandv1.NetworkMode_NETWORK_MODE_NONE {
		t.Fatalf("none: %v %v", mode, err)
	}
	_, err := NetworkMode(&ownsandv1.Network{})
	wantInvalid(t, "present unspecified network", err, "network.mode")
}

func TestCreateSandboxRequest(t *testing.T) {
	valid := func() *ownsandv1.CreateSandboxRequest {
		host := ownsandv1.HostOs_HOST_OS_LINUX
		return &ownsandv1.CreateSandboxRequest{
			OrganizationId: orgID, IdempotencyKey: opID,
			Source:    &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX}},
			Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 2048, DiskGib: 10},
			IdleTtl:   durationpb.New(10 * time.Minute), MaxTtl: durationpb.New(time.Hour),
			Placement: &ownsandv1.Placement{HostOs: &host},
			Labels:    map[string]string{"tide.runner-id": "x"},
		}
	}
	if err := CreateSandboxRequest(valid()); err != nil {
		t.Fatal(err)
	}
	r := valid()
	r.IdempotencyKey = ""
	wantInvalid(t, "idempotency key", CreateSandboxRequest(r), "idempotency_key")
	r = valid()
	r.MaxTtl = nil
	wantInvalid(t, "no TTL", CreateSandboxRequest(r), "max_ttl")
	r = valid()
	unspecified := ownsandv1.Arch_ARCH_UNSPECIFIED
	r.Placement = &ownsandv1.Placement{Arch: &unspecified}
	wantInvalid(t, "unspecified arch filter", CreateSandboxRequest(r), "placement.arch")
	r = valid()
	bad := "nope"
	r.Placement = &ownsandv1.Placement{DeviceId: &bad}
	wantInvalid(t, "device id", CreateSandboxRequest(r), "placement.device_id")
	r = valid()
	r.Network = &ownsandv1.Network{}
	wantInvalid(t, "network", CreateSandboxRequest(r), "network.mode")
	r = valid()
	r.Source = &ownsandv1.SandboxSource{Source: (*ownsandv1.SandboxSource_SnapshotId)(nil)}
	wantInvalid(t, "typed nil source", CreateSandboxRequest(r), "source")
}

func TestDeviceLimits(t *testing.T) {
	for p := int32(100); p <= 400; p += 25 {
		if err := DeviceLimits(&ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 1, DiskGib: 1, VcpuOvercommitPercent: p}); err != nil {
			t.Errorf("percent %d rejected: %v", p, err)
		}
	}
	for _, p := range []int32{0, 99, 110, 425, 1000, -100} {
		wantInvalid(t, "percent", DeviceLimits(&ownsandv1.DeviceLimits{VcpuOvercommitPercent: p}), "limits.vcpu_overcommit_percent")
	}
	if err := DeviceLimits(&ownsandv1.DeviceLimits{VcpuOvercommitPercent: 100}); err != nil {
		t.Fatalf("zero limits rejected: %v", err)
	}
	wantInvalid(t, "negative vcpu", DeviceLimits(&ownsandv1.DeviceLimits{VcpuCount: -1, VcpuOvercommitPercent: 100}), "limits.vcpu_count")
	wantInvalid(t, "nil", DeviceLimits(nil), "limits")

	cases := []struct {
		vcpu, percent int32
		want          int64
	}{
		{8, 100, 8}, {8, 125, 10}, {3, 125, 3}, {5, 150, 7}, {7, 400, 28}, {0, 400, 0},
		{1 << 30, 400, 1 << 32},
	}
	for _, tc := range cases {
		got := EffectiveVCPULimit(&ownsandv1.DeviceLimits{VcpuCount: tc.vcpu, VcpuOvercommitPercent: tc.percent})
		if got != tc.want {
			t.Errorf("EffectiveVCPULimit(%d, %d) = %d, want %d", tc.vcpu, tc.percent, got, tc.want)
		}
	}
}

func TestEnrollmentTokenTTLAndPageSize(t *testing.T) {
	if err := EnrollmentTokenTTL(durationpb.New(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantInvalid(t, "over 24h", EnrollmentTokenTTL(durationpb.New(24*time.Hour+time.Second)), "ttl")
	wantInvalid(t, "zero", EnrollmentTokenTTL(durationpb.New(0)), "ttl")
	wantInvalid(t, "nil", EnrollmentTokenTTL(nil), "ttl")

	for in, want := range map[int32]int32{0: 50, 1: 1, 200: 200, 201: 200, 10000: 200} {
		got, err := PageSize(in)
		if err != nil || got != want {
			t.Errorf("PageSize(%d) = %d, %v", in, got, err)
		}
	}
	_, err := PageSize(-1)
	wantInvalid(t, "negative page", err, "page_size")
	if !IsValidationError(err) || IsValidationError(errors.New("x")) {
		t.Fatal("IsValidationError")
	}
}
