// SPDX-License-Identifier: Apache-2.0

package sandrunv1_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

const packageName = "delino.sandrun.v1"

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

// services returns every service of the package.
func services(t *testing.T) []protoreflect.ServiceDescriptor {
	t.Helper()
	var out []protoreflect.ServiceDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage(packageName, func(file protoreflect.FileDescriptor) bool {
		for i := 0; i < file.Services().Len(); i++ {
			out = append(out, file.Services().Get(i))
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("no services registered for the package")
	}
	return out
}

// Every public RPC is unary or server-streaming, so browser clients can call
// it.
func TestRPCsAreUnaryOrServerStreaming(t *testing.T) {
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			if method.IsStreamingClient() {
				t.Errorf("%s is client-streaming", method.FullName())
			}
		}
	}
}

// secretFieldNames are the request field names that carry credentials or
// one-time secrets.
var secretFieldNames = map[string]bool{
	"ownsand_api_key":         true,
	"environment_key":         true,
	"service_account_api_key": true,
	"webhook_secret":          true,
	"api_token":               true,
	"outpost_token":           true,
	"secret":                  true,
	"invitation_secret":       true,
	"code":                    true,
}

// allowedResponseSecrets are the only response fields that carry a secret:
// the personal API key secret and the invitation secret, returned once.
var allowedResponseSecrets = map[protoreflect.FullName]bool{
	"delino.sandrun.v1.CreateApiKeyResponse.secret":                true,
	"delino.sandrun.v1.CreateInvitationResponse.invitation_secret": true,
}

// No response message, or message reachable from one, carries a credential
// field, except the documented one-time secrets.
func TestResponsesCarryNoCredentials(t *testing.T) {
	seen := map[protoreflect.FullName]bool{}
	var walk func(protoreflect.MessageDescriptor)
	walk = func(message protoreflect.MessageDescriptor) {
		if seen[message.FullName()] || message.ParentFile().Package() != packageName {
			return
		}
		seen[message.FullName()] = true
		for i := 0; i < message.Fields().Len(); i++ {
			field := message.Fields().Get(i)
			if secretFieldNames[string(field.Name())] && !allowedResponseSecrets[field.FullName()] {
				t.Errorf("response field %s carries a credential", field.FullName())
			}
			if field.Message() != nil {
				walk(field.Message())
			}
		}
	}
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			walk(service.Methods().Get(i).Output())
		}
	}
}

// Every RPC except the three documented ones takes an organization_id.
func TestRPCsTakeOrganizationID(t *testing.T) {
	without := map[protoreflect.FullName]bool{
		"delino.sandrun.v1.OrganizationService.CreateOrganization": true,
		"delino.sandrun.v1.OrganizationService.ListOrganizations":  true,
		"delino.sandrun.v1.MemberService.AcceptInvitation":         true,
	}
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			has := method.Input().Fields().ByName("organization_id") != nil
			if has == without[method.FullName()] {
				t.Errorf("%s: organization_id present = %v", method.FullName(), has)
			}
		}
	}
}

// The DevSessionExecEvent oneof has the documented events.
func TestDevSessionExecEventCases(t *testing.T) {
	oneof := (&sandrunv1.DevSessionExecEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	want := []string{"keepalive", "started", "output_truncated", "stdout", "stderr", "exited", "failed", "sandbox_ended"}
	if oneof.Fields().Len() != len(want) {
		t.Fatalf("event has %d cases, want %d", oneof.Fields().Len(), len(want))
	}
	for _, name := range want {
		if oneof.Fields().ByName(protoreflect.Name(name)) == nil {
			t.Errorf("event has no case %q", name)
		}
	}
}
