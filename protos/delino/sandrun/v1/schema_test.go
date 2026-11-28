// SPDX-License-Identifier: Apache-2.0

package sandrunv1_test

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

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

func carriesCredential(field protoreflect.FieldDescriptor) bool {
	if field.Name() == "api_key" {
		return field.Kind() == protoreflect.StringKind
	}
	return secretFieldNames[string(field.Name())]
}

func TestApiKeyFieldCredentialClassification(t *testing.T) {
	for _, test := range []struct {
		message        protoreflect.MessageDescriptor
		wantCredential bool
	}{
		{(&sandrunv1.OpenAiCodexCredentials{}).ProtoReflect().Descriptor(), true},
		{(&sandrunv1.CreateApiKeyResponse{}).ProtoReflect().Descriptor(), false},
		{(&sandrunv1.RevokeApiKeyResponse{}).ProtoReflect().Descriptor(), false},
	} {
		t.Run(string(test.message.Name()), func(t *testing.T) {
			field := test.message.Fields().ByName("api_key")
			if field == nil {
				t.Fatal("missing api_key field")
			}
			if got := carriesCredential(field); got != test.wantCredential {
				t.Errorf("%s (%s) carries a credential = %t, want %t", field.FullName(), field.Kind(), got, test.wantCredential)
			}
		})
	}
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
			if carriesCredential(field) && !allowedResponseSecrets[field.FullName()] {
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

// Every RPC except the three documented ones takes a singular string organization_id.
func TestRPCsTakeOrganizationID(t *testing.T) {
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			if err := validateRPCOrganizationID(service.Methods().Get(i)); err != nil {
				t.Error(err)
			}
		}
	}
}

func validateRPCOrganizationID(method protoreflect.MethodDescriptor) error {
	name := method.FullName()
	field := method.Input().Fields().ByName("organization_id")
	switch name {
	case "delino.sandrun.v1.OrganizationService.CreateOrganization",
		"delino.sandrun.v1.OrganizationService.ListOrganizations",
		"delino.sandrun.v1.MemberService.AcceptInvitation":
		if field != nil {
			return fmt.Errorf("%s: request must omit organization_id", name)
		}
		return nil
	}
	if field == nil {
		return fmt.Errorf("%s: request is missing organization_id", name)
	}
	if field.Kind() != protoreflect.StringKind || field.Cardinality() == protoreflect.Repeated {
		return fmt.Errorf("%s: organization_id must be a singular string (got %s %s)", name, field.Cardinality(), field.Kind())
	}
	return nil
}

func TestValidateRPCOrganizationID(t *testing.T) {
	field := func(kind descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String("organization_id"),
			Number: proto.Int32(1),
			Type:   kind.Enum(),
			Label:  label.Enum(),
		}
	}
	singularString := field(descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	singularInt64 := field(descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	repeatedString := field(descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_LABEL_REPEATED)

	check := func(t *testing.T, service, method string, field *descriptorpb.FieldDescriptorProto, wantError string) {
		t.Helper()
		request := &descriptorpb.DescriptorProto{Name: proto.String("Request")}
		if field != nil {
			request.Field = []*descriptorpb.FieldDescriptorProto{field}
		}
		file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name:        proto.String("organization_id_test.proto"),
			Package:     proto.String(packageName),
			Syntax:      proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{request, {Name: proto.String("Response")}},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String(service),
				Method: []*descriptorpb.MethodDescriptorProto{{
					Name:       proto.String(method),
					InputType:  proto.String("." + packageName + ".Request"),
					OutputType: proto.String("." + packageName + ".Response"),
				}},
			}},
		}, nil)
		if err != nil {
			t.Fatalf("create descriptor for %s.%s: %v", service, method, err)
		}
		var gotError string
		if err := validateRPCOrganizationID(file.Services().Get(0).Methods().Get(0)); err != nil {
			gotError = err.Error()
		}
		if gotError != wantError {
			t.Errorf("validation error = %q, want %q", gotError, wantError)
		}
	}

	for _, test := range []struct {
		name      string
		field     *descriptorpb.FieldDescriptorProto
		wantError string
	}{
		{"singular_string", singularString, ""},
		{"missing", nil, "delino.sandrun.v1.OrganizationService.GetOrganization: request is missing organization_id"},
		{"numeric", singularInt64, "delino.sandrun.v1.OrganizationService.GetOrganization: organization_id must be a singular string (got optional int64)"},
		{"repeated", repeatedString, "delino.sandrun.v1.OrganizationService.GetOrganization: organization_id must be a singular string (got repeated string)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check(t, "OrganizationService", "GetOrganization", test.field, test.wantError)
		})
	}

	for _, test := range []struct {
		service string
		method  string
	}{
		{"OrganizationService", "CreateOrganization"},
		{"OrganizationService", "ListOrganizations"},
		{"MemberService", "AcceptInvitation"},
	} {
		t.Run(test.method+"/absent", func(t *testing.T) {
			check(t, test.service, test.method, nil, "")
		})
		t.Run(test.method+"/present", func(t *testing.T) {
			wantError := packageName + "." + test.service + "." + test.method + ": request must omit organization_id"
			check(t, test.service, test.method, singularString, wantError)
		})
	}
	t.Run("same_method_name_in_another_service", func(t *testing.T) {
		check(t, "OtherService", "CreateOrganization", nil,
			"delino.sandrun.v1.OtherService.CreateOrganization: request is missing organization_id")
	})
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
