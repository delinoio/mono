// SPDX-License-Identifier: Apache-2.0

// Package validate holds the request rules that the OwnSand control plane,
// the device agent and the SDKs check the same way: exec request size,
// labels, TTLs, resources, device limits, network mode, sandbox source and
// page size. Every failure is an *Error with the INVALID_ARGUMENT code; its
// message names the field and the rule and never contains the field value,
// because values can be sandbox content or labels, which components never
// log. The owning contracts are docs/protos-ownsand-api-contract.md and
// docs/servers-ownsand-api-foundation.md.
package validate

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Limits of the public API.
const (
	// MaxExecFieldsSize bounds the encoded command, env and
	// working_directory of one ExecRequest together.
	MaxExecFieldsSize = 256 << 10
	// MaxLabels is the largest number of labels of a sandbox.
	MaxLabels = 32
	// MaxLabelKeySize and MaxLabelValueSize are byte limits.
	MaxLabelKeySize   = 63
	MaxLabelValueSize = 255
	// MaxTTL is the largest max_ttl (exactly 7 days is valid).
	MaxTTL = 7 * 24 * time.Hour
	// DefaultPageSize and MaxPageSize apply to every list RPC.
	DefaultPageSize int32 = 50
	MaxPageSize     int32 = 200
	// vCPU overcommit percentage range and step.
	MinVCPUOvercommitPercent  int32 = 100
	MaxVCPUOvercommitPercent  int32 = 400
	VCPUOvercommitPercentStep int32 = 25
	// MaxEnrollmentTokenTTL is the longest enrollment token lifetime.
	MaxEnrollmentTokenTTL = 24 * time.Hour
)

// Error is a validation failure. Code is always INVALID_ARGUMENT.
type Error struct {
	// Field is the request field path, for example `resources.vcpu_count`.
	Field string
	// Rule describes the broken rule without the field value.
	Rule string
}

func (e *Error) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Rule)
}

// Code returns INVALID_ARGUMENT.
func (e *Error) Code() ownsandv1.ErrorCode {
	return ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
}

// Detail returns the ErrorDetail of an error frame.
func (e *Error) Detail() *ownsandv1.ErrorDetail {
	return &ownsandv1.ErrorDetail{Code: e.Code(), Message: e.Error()}
}

func fail(field, rule string, args ...any) error {
	return &Error{Field: field, Rule: fmt.Sprintf(rule, args...)}
}

// IsValidationError reports whether err is an *Error.
func IsValidationError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// UUIDv7 checks that value is a canonical UUID v7.
func UUIDv7(field, value string) error {
	if value == "" {
		return fail(field, "is required")
	}
	if !uuidv7.Valid(value) {
		return fail(field, "must be a canonical UUID v7")
	}
	return nil
}

// ExecFieldsSize returns the encoded size of the command, env and
// working_directory fields of req.
func ExecFieldsSize(req *ownsandv1.ExecRequest) int {
	return proto.Size(&ownsandv1.ExecRequest{
		Command:          req.GetCommand(),
		Env:              req.GetEnv(),
		WorkingDirectory: req.GetWorkingDirectory(),
	})
}

// ExecRequest checks an exec request before it is framed (SDK, control
// plane) and again after it is decoded (agent): the IDs, a non-empty command
// whose first element is not empty, the 256 KiB limit of the encoded
// command, env and working_directory, a non-negative valid timeout and a
// 32-byte binding salt. It does not check the admission window of exec_id;
// the agent does that with datapath.CheckOperationID.
func ExecRequest(req *ownsandv1.ExecRequest) error {
	if err := UUIDv7("organization_id", req.GetOrganizationId()); err != nil {
		return err
	}
	if err := UUIDv7("sandbox_id", req.GetSandboxId()); err != nil {
		return err
	}
	if err := UUIDv7("exec_id", req.GetExecId()); err != nil {
		return err
	}
	if len(req.GetCommand()) == 0 || req.GetCommand()[0] == "" {
		return fail("command", "needs a program as its first element")
	}
	if size := ExecFieldsSize(req); size > MaxExecFieldsSize {
		return fail("command", "command, env and working_directory encode to %d bytes, more than %d", size, MaxExecFieldsSize)
	}
	if timeout := req.GetTimeout(); timeout != nil {
		if err := timeout.CheckValid(); err != nil {
			return fail("timeout", "is not a valid duration")
		}
		if timeout.AsDuration() < 0 {
			return fail("timeout", "must not be negative")
		}
	}
	return BindingSalt(req.GetBindingSalt())
}

// BindingSalt checks that salt has relaybinding.SaltSize bytes.
func BindingSalt(salt []byte) error {
	if len(salt) != relaybinding.SaltSize {
		return fail("binding_salt", "must be %d bytes", relaybinding.SaltSize)
	}
	return nil
}

// AttachExecRequest checks the IDs and offsets of an exec attach request.
func AttachExecRequest(req *ownsandv1.AttachExecRequest) error {
	if err := UUIDv7("organization_id", req.GetOrganizationId()); err != nil {
		return err
	}
	if err := UUIDv7("sandbox_id", req.GetSandboxId()); err != nil {
		return err
	}
	if err := UUIDv7("exec_id", req.GetExecId()); err != nil {
		return err
	}
	if req.GetStdoutOffset() < 0 || req.GetStderrOffset() < 0 {
		return fail("stdout_offset", "offsets must not be negative")
	}
	return nil
}

// ReadFileRequest checks a file read request. Every call carries the path
// and the salt.
func ReadFileRequest(req *ownsandv1.ReadFileRequest) error {
	if err := UUIDv7("organization_id", req.GetOrganizationId()); err != nil {
		return err
	}
	if err := UUIDv7("sandbox_id", req.GetSandboxId()); err != nil {
		return err
	}
	if err := UUIDv7("read_id", req.GetReadId()); err != nil {
		return err
	}
	if err := guestPath("path", req.GetPath()); err != nil {
		return err
	}
	if req.GetOffset() < 0 {
		return fail("offset", "must not be negative")
	}
	return BindingSalt(req.GetBindingSalt())
}

// WriteFileRequest checks one WriteFile call. first is true for the first
// call of a write_id, which must carry the path and the salt; later calls
// may omit them, but a call that carries them is checked.
func WriteFileRequest(req *ownsandv1.WriteFileRequest, first bool) error {
	if err := UUIDv7("organization_id", req.GetOrganizationId()); err != nil {
		return err
	}
	if err := UUIDv7("sandbox_id", req.GetSandboxId()); err != nil {
		return err
	}
	if err := UUIDv7("write_id", req.GetWriteId()); err != nil {
		return err
	}
	if first || req.GetPath() != "" || len(req.GetBindingSalt()) > 0 {
		if err := guestPath("path", req.GetPath()); err != nil {
			return err
		}
		if err := BindingSalt(req.GetBindingSalt()); err != nil {
			return err
		}
	}
	if req.GetOffset() < 0 {
		return fail("offset", "must not be negative")
	}
	if len(req.GetData()) > datapath.MaxFileChunkSize {
		return fail("data", "must not exceed %d bytes", datapath.MaxFileChunkSize)
	}
	if finish := req.GetFinish(); finish != nil {
		if finish.GetSizeBytes() < 0 {
			return fail("finish.size_bytes", "must not be negative")
		}
		if finish.GetMode() > 0o7777 {
			return fail("finish.mode", "must be at most 07777")
		}
	}
	return nil
}

// guestPath checks that a guest path is present. The guest helper decides
// whether it is absolute for its guest OS (for example `/etc/hosts` or
// `C:\Users`), so this check accepts both forms.
func guestPath(field, path string) error {
	if path == "" {
		return fail(field, "is required")
	}
	if !utf8.ValidString(path) {
		return fail(field, "must be valid UTF-8")
	}
	if !isAbsoluteGuestPath(path) {
		return fail(field, "must be an absolute guest path")
	}
	return nil
}

func isAbsoluteGuestPath(path string) bool {
	if path[0] == '/' || path[0] == '\\' {
		return true
	}
	// A Windows drive path such as `C:\` or `C:/`.
	return len(path) >= 3 && isLetter(path[0]) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Labels checks the label limits: at most 32 entries, keys of 1 to 63
// bytes and values of at most 255 bytes.
func Labels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fail("labels", "at most %d entries", MaxLabels)
	}
	for key, value := range labels {
		if key == "" {
			return fail("labels", "keys must not be empty")
		}
		if len(key) > MaxLabelKeySize {
			return fail("labels", "keys must be at most %d bytes", MaxLabelKeySize)
		}
		if len(value) > MaxLabelValueSize {
			return fail("labels", "values must be at most %d bytes", MaxLabelValueSize)
		}
	}
	return nil
}

// TTL checks idle_ttl and max_ttl: both required and greater than zero,
// idle_ttl not greater than max_ttl and max_ttl at most 7 days.
func TTL(idle, maxTTL *durationpb.Duration) error {
	if idle == nil {
		return fail("idle_ttl", "is required")
	}
	if maxTTL == nil {
		return fail("max_ttl", "is required")
	}
	if idle.CheckValid() != nil || idle.AsDuration() <= 0 {
		return fail("idle_ttl", "must be greater than zero")
	}
	if maxTTL.CheckValid() != nil || maxTTL.AsDuration() <= 0 {
		return fail("max_ttl", "must be greater than zero")
	}
	if maxTTL.AsDuration() > MaxTTL {
		return fail("max_ttl", "must be at most 7 days")
	}
	if idle.AsDuration() > maxTTL.AsDuration() {
		return fail("idle_ttl", "must not be greater than max_ttl")
	}
	return nil
}

// Resources checks that vcpu_count, memory_mib and disk_gib are present and
// greater than zero.
func Resources(r *ownsandv1.Resources) error {
	if r == nil {
		return fail("resources", "is required")
	}
	if r.GetVcpuCount() <= 0 {
		return fail("resources.vcpu_count", "must be greater than zero")
	}
	if r.GetMemoryMib() <= 0 {
		return fail("resources.memory_mib", "must be greater than zero")
	}
	if r.GetDiskGib() <= 0 {
		return fail("resources.disk_gib", "must be greater than zero")
	}
	return nil
}

// SandboxSource checks that exactly one source is set and that it is valid.
func SandboxSource(s *ownsandv1.SandboxSource) error {
	const sourceRequired = "exactly one of base_image, snapshot_id or template_id is required"
	// A type switch matches a typed nil wrapper pointer, so each case rejects
	// nil before it dereferences the wrapper.
	switch src := s.GetSource().(type) {
	case *ownsandv1.SandboxSource_BaseImage:
		if src == nil {
			return fail("source", sourceRequired)
		}
		if src.BaseImage == ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED {
			return fail("source.base_image", "must not be unspecified")
		}
		if _, ok := ownsandv1.BaseImage_name[int32(src.BaseImage)]; !ok {
			return fail("source.base_image", "is unknown")
		}
		return nil
	case *ownsandv1.SandboxSource_SnapshotId:
		if src == nil {
			return fail("source", sourceRequired)
		}
		return UUIDv7("source.snapshot_id", src.SnapshotId)
	case *ownsandv1.SandboxSource_TemplateId:
		if src == nil {
			return fail("source", sourceRequired)
		}
		return UUIDv7("source.template_id", src.TemplateId)
	}
	return fail("source", sourceRequired)
}

// NetworkMode returns the network mode of a create request: OUTBOUND when
// network is absent, and an error when a present message has an
// unspecified or unknown mode.
func NetworkMode(network *ownsandv1.Network) (ownsandv1.NetworkMode, error) {
	if network == nil {
		return ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, nil
	}
	switch mode := network.GetMode(); mode {
	case ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, ownsandv1.NetworkMode_NETWORK_MODE_NONE:
		return mode, nil
	}
	return 0, fail("network.mode", "must be NETWORK_MODE_OUTBOUND or NETWORK_MODE_NONE")
}

// CreateSandboxRequest checks the fields of a create request that do not
// need storage: organization_id, idempotency_key, source, resources, TTLs,
// network and labels. Placement filters and the snapshot disk size need the
// control plane's records.
func CreateSandboxRequest(req *ownsandv1.CreateSandboxRequest) error {
	if err := UUIDv7("organization_id", req.GetOrganizationId()); err != nil {
		return err
	}
	if err := UUIDv7("idempotency_key", req.GetIdempotencyKey()); err != nil {
		return err
	}
	if err := SandboxSource(req.GetSource()); err != nil {
		return err
	}
	if err := Resources(req.GetResources()); err != nil {
		return err
	}
	if err := TTL(req.GetIdleTtl(), req.GetMaxTtl()); err != nil {
		return err
	}
	if _, err := NetworkMode(req.GetNetwork()); err != nil {
		return err
	}
	if p := req.GetPlacement(); p != nil {
		if p.DeviceId != nil {
			if err := UUIDv7("placement.device_id", p.GetDeviceId()); err != nil {
				return err
			}
		}
		if p.HostOs != nil && p.GetHostOs() == ownsandv1.HostOs_HOST_OS_UNSPECIFIED {
			return fail("placement.host_os", "must not be unspecified when present")
		}
		if p.Arch != nil && p.GetArch() == ownsandv1.Arch_ARCH_UNSPECIFIED {
			return fail("placement.arch", "must not be unspecified when present")
		}
	}
	return Labels(req.GetLabels())
}

// DeviceLimits checks the four device limits of UpdateDeviceLimits:
// vcpu_count, memory_mib and disk_gib are present and not negative (zero
// makes the device ineligible for new sandboxes, like a new device), and
// vcpu_overcommit_percent is 100 to 400 in steps of 25.
func DeviceLimits(l *ownsandv1.DeviceLimits) error {
	if l == nil {
		return fail("limits", "is required")
	}
	if l.GetVcpuCount() < 0 {
		return fail("limits.vcpu_count", "must not be negative")
	}
	if l.GetMemoryMib() < 0 {
		return fail("limits.memory_mib", "must not be negative")
	}
	if l.GetDiskGib() < 0 {
		return fail("limits.disk_gib", "must not be negative")
	}
	p := l.GetVcpuOvercommitPercent()
	if p < MinVCPUOvercommitPercent || p > MaxVCPUOvercommitPercent || p%VCPUOvercommitPercentStep != 0 {
		return fail("limits.vcpu_overcommit_percent", "must be %d to %d in steps of %d", MinVCPUOvercommitPercent, MaxVCPUOvercommitPercent, VCPUOvercommitPercentStep)
	}
	return nil
}

// EffectiveVCPULimit returns floor(vcpu_count * vcpu_overcommit_percent /
// 100), computed in 64-bit integers.
func EffectiveVCPULimit(l *ownsandv1.DeviceLimits) int64 {
	return int64(l.GetVcpuCount()) * int64(l.GetVcpuOvercommitPercent()) / 100
}

// EnrollmentTokenTTL checks that ttl is present, greater than zero and at
// most 24 hours.
func EnrollmentTokenTTL(ttl *durationpb.Duration) error {
	if ttl == nil {
		return fail("ttl", "is required")
	}
	if ttl.CheckValid() != nil || ttl.AsDuration() <= 0 {
		return fail("ttl", "must be greater than zero")
	}
	if ttl.AsDuration() > MaxEnrollmentTokenTTL {
		return fail("ttl", "must be at most 24 hours")
	}
	return nil
}

// PageSize returns the effective page size of a list request: 50 for 0,
// the value itself up to 200, and 200 for larger values. A negative value
// is invalid.
func PageSize(size int32) (int32, error) {
	switch {
	case size < 0:
		return 0, fail("page_size", "must not be negative")
	case size == 0:
		return DefaultPageSize, nil
	case size > MaxPageSize:
		return MaxPageSize, nil
	}
	return size, nil
}
