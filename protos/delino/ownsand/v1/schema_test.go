// SPDX-License-Identifier: Apache-2.0

package ownsandv1_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const packageName = "delino.ownsand.v1"

// Every DataFrameKind value except UNSPECIFIED names exactly one payload
// field of DataFrame, and every payload field has a kind. Components map
// kinds to payloads by this naming rule.
func TestDataFrameKindsMatchPayloadFields(t *testing.T) {
	frame := (&ownsandv1.DataFrame{}).ProtoReflect().Descriptor()
	payload := frame.Oneofs().ByName("payload")
	if payload == nil {
		t.Fatal("DataFrame has no payload oneof")
	}
	fields := map[string]bool{}
	for i := 0; i < payload.Fields().Len(); i++ {
		fields[string(payload.Fields().Get(i).Name())] = true
	}
	kinds := ownsandv1.DataFrameKind(0).Descriptor().Values()
	for i := 0; i < kinds.Len(); i++ {
		value := kinds.Get(i)
		if value.Number() == 0 {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(string(value.Name()), "DATA_FRAME_KIND_"))
		if !fields[name] {
			t.Errorf("DataFrameKind %s has no payload field %q", value.Name(), name)
		}
		delete(fields, name)
	}
	for name := range fields {
		t.Errorf("payload field %q has no DataFrameKind", name)
	}
}

// Every enum of the package has a zero value named <ENUM>_UNSPECIFIED.
func TestEnumsHaveUnspecifiedZeroValue(t *testing.T) {
	count := 0
	protoregistry.GlobalTypes.RangeEnums(func(enum protoreflect.EnumType) bool {
		descriptor := enum.Descriptor()
		if descriptor.ParentFile().Package() != packageName {
			return true
		}
		count++
		zero := descriptor.Values().ByNumber(0)
		if zero == nil || !strings.HasSuffix(string(zero.Name()), "_UNSPECIFIED") {
			t.Errorf("enum %s has no _UNSPECIFIED zero value", descriptor.FullName())
		}
		return true
	})
	if count == 0 {
		t.Fatal("no enums registered for the package")
	}
}

// ErrorCode values equal the Connect error codes, so components can convert
// without a table.
func TestErrorCodeNumbersMatchConnect(t *testing.T) {
	cases := map[ownsandv1.ErrorCode]int32{
		ownsandv1.ErrorCode_ERROR_CODE_CANCELED:            1,
		ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:    3,
		ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND:           5,
		ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:   7,
		ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:  8,
		ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION: 9,
		ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE:         14,
		ownsandv1.ErrorCode_ERROR_CODE_UNAUTHENTICATED:     16,
	}
	for code, want := range cases {
		if int32(code) != want {
			t.Errorf("%s = %d, want %d", code, int32(code), want)
		}
	}
}

// The contract lists the audit actions of the control plane audit table.
func TestAuditActionsMatchContract(t *testing.T) {
	want := []string{
		"ORGANIZATION_CREATED", "ORGANIZATION_DELETED", "OWNERSHIP_TRANSFERRED",
		"INVITATION_CREATED", "INVITATION_REVOKED", "MEMBER_JOINED",
		"MEMBER_ROLE_CHANGED", "MEMBER_REMOVED", "ENROLLMENT_TOKEN_CREATED",
		"ENROLLMENT_TOKEN_REVOKED", "DEVICE_ENROLLED", "DEVICE_LIMITS_CHANGED",
		"DEVICE_REMOVED", "AGENT_VERSION_POLICY_CHANGED", "API_KEY_CREATED",
		"API_KEY_REVOKED", "SANDBOX_CREATED", "SANDBOX_DELETED",
	}
	values := ownsandv1.AuditAction(0).Descriptor().Values()
	if values.Len() != len(want)+1 {
		t.Fatalf("AuditAction has %d values, want %d", values.Len(), len(want)+1)
	}
	for _, name := range want {
		if values.ByName(protoreflect.Name("AUDIT_ACTION_"+name)) == nil {
			t.Errorf("AuditAction has no value %s", name)
		}
	}
}
