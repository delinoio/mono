// SPDX-License-Identifier: Apache-2.0

package sandcrewv1_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

const packageName = "delino.sandcrew.v1"

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

// Every BridgeEvent case round-trips through protobuf JSON as one line.
func TestBridgeEventCasesRoundTripThroughJSON(t *testing.T) {
	oneof := (&sandcrewv1.BridgeEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	fields := oneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			event := &sandcrewv1.BridgeEvent{Sequence: 7, LogEndOffset: 99, Time: timestamppb.Now()}
			message := event.ProtoReflect()
			value := message.NewField(field)
			populate(value.Message())
			message.Set(field, value)

			line, err := protojson.Marshal(event)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(line), "\n") {
				t.Fatalf("protobuf JSON is not one line: %s", line)
			}
			var decoded sandcrewv1.BridgeEvent
			if err := protojson.Unmarshal(line, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !proto.Equal(event, &decoded) {
				t.Fatalf("round trip changed the event:\n got %v\nwant %v", &decoded, event)
			}
		})
	}
}

// Every BridgeInput case round-trips through protobuf JSON.
func TestBridgeInputCasesRoundTripThroughJSON(t *testing.T) {
	oneof := (&sandcrewv1.BridgeInput{}).ProtoReflect().Descriptor().Oneofs().ByName("input")
	fields := oneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			input := &sandcrewv1.BridgeInput{InputId: "0190f3c4-0000-7000-8000-000000000000", CreateTime: timestamppb.Now()}
			message := input.ProtoReflect()
			value := message.NewField(field)
			populate(value.Message())
			message.Set(field, value)
			data, err := protojson.Marshal(input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded sandcrewv1.BridgeInput
			if err := protojson.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !proto.Equal(input, &decoded) {
				t.Fatalf("round trip changed the input:\n got %v\nwant %v", &decoded, input)
			}
		})
	}
}

// populate sets the first case of each oneof and every other scalar field of
// message to a nonzero value, so a round trip covers every field.
func populate(message protoreflect.Message) {
	fields := message.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if oneof := field.ContainingOneof(); oneof != nil && !oneof.IsSynthetic() && oneof.Fields().Get(0) != field {
			continue
		}
		switch {
		case field.IsList():
			list := message.Mutable(field).List()
			list.Append(sample(list.NewElement(), field))
		case field.Message() != nil:
			if field.Message().FullName() == "google.protobuf.Timestamp" {
				message.Set(field, protoreflect.ValueOfMessage(timestamppb.Now().ProtoReflect()))
				continue
			}
			populate(message.Mutable(field).Message())
		default:
			message.Set(field, sample(message.NewField(field), field))
		}
	}
}

func sample(value protoreflect.Value, field protoreflect.FieldDescriptor) protoreflect.Value {
	switch field.Kind() {
	case protoreflect.MessageKind:
		populate(value.Message())
		return value
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("value-" + string(field.Name()))
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.EnumKind:
		return protoreflect.ValueOfEnum(1)
	case protoreflect.Int32Kind:
		return protoreflect.ValueOfInt32(3)
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(5)
	case protoreflect.Uint32Kind:
		return protoreflect.ValueOfUint32(11)
	case protoreflect.Uint64Kind:
		return protoreflect.ValueOfUint64(13)
	default:
		panic("unsupported kind " + field.Kind().String())
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
// it. A server-streaming RPC returns a <Rpc>Response message.
func TestRPCsAreUnaryOrServerStreaming(t *testing.T) {
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			if method.IsStreamingClient() {
				t.Errorf("%s is client-streaming", method.FullName())
			}
			if want := method.Name() + "Response"; method.Output().Name() != want {
				t.Errorf("%s returns %s, want %s", method.FullName(), method.Output().Name(), want)
			}
		}
	}
}

// Every RPC except ListOrganizations and the NotificationService RPCs takes
// an organization_id.
func TestRPCsTakeOrganizationID(t *testing.T) {
	for _, service := range services(t) {
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			without := method.FullName() == "delino.sandcrew.v1.OrganizationService.ListOrganizations" ||
				service.FullName() == "delino.sandcrew.v1.NotificationService"
			has := method.Input().Fields().ByName("organization_id") != nil
			if has == without {
				t.Errorf("%s: organization_id present = %v", method.FullName(), has)
			}
		}
	}
}

// valueFields are the request fields that carry credential, secret or push
// subscription key values.
var valueFields = map[protoreflect.FullName]bool{
	"delino.sandcrew.v1.SetVendorCredentialRequest.value":           true,
	"delino.sandcrew.v1.SetSessionSecretRequest.value":              true,
	"delino.sandcrew.v1.RegisterPushSubscriptionRequest.p256dh_key": true,
	"delino.sandcrew.v1.RegisterPushSubscriptionRequest.auth_key":   true,
}

// No response message, or message reachable from one, has a field named like
// a credential or secret value.
func TestResponsesCarryNoValues(t *testing.T) {
	for name := range valueFields {
		if _, err := protoregistry.GlobalFiles.FindDescriptorByName(name); err != nil {
			t.Errorf("value field %s is not registered: %v", name, err)
		}
	}
	seen := map[protoreflect.FullName]bool{}
	var walk func(protoreflect.MessageDescriptor)
	walk = func(message protoreflect.MessageDescriptor) {
		if seen[message.FullName()] || message.ParentFile().Package() != packageName {
			return
		}
		seen[message.FullName()] = true
		for i := 0; i < message.Fields().Len(); i++ {
			field := message.Fields().Get(i)
			switch field.Name() {
			case "value", "p256dh_key", "auth_key", "endpoint":
				t.Errorf("response field %s carries a value", field.FullName())
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

// TranscriptEvent has every BridgeEvent case except credential_update, with
// the same field numbers and messages.
func TestTranscriptEventHasNoCredentialUpdate(t *testing.T) {
	transcript := (&sandcrewv1.TranscriptEvent{}).ProtoReflect().Descriptor()
	if transcript.Fields().ByName("credential_update") != nil {
		t.Fatal("TranscriptEvent has a credential_update case")
	}
	if !transcript.ReservedNames().Has("credential_update") {
		t.Error("TranscriptEvent does not reserve the name credential_update")
	}
	bridge := (&sandcrewv1.BridgeEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	oneof := transcript.Oneofs().ByName("event")
	if oneof.Fields().Len() != bridge.Fields().Len()-1 {
		t.Fatalf("TranscriptEvent has %d cases, want %d", oneof.Fields().Len(), bridge.Fields().Len()-1)
	}
	for i := 0; i < bridge.Fields().Len(); i++ {
		want := bridge.Fields().Get(i)
		if want.Name() == "credential_update" {
			continue
		}
		got := oneof.Fields().ByName(want.Name())
		if got == nil {
			t.Errorf("TranscriptEvent has no case %q", want.Name())
			continue
		}
		if got.Number() != want.Number() || got.Message().FullName() != want.Message().FullName() {
			t.Errorf("case %q is %d %s, want %d %s", want.Name(), got.Number(), got.Message().FullName(), want.Number(), want.Message().FullName())
		}
	}
}

// Every BridgeEvent case except credential_update has a TranscriptEvent case
// with the same wire encoding.
func TestBridgeEventCasesConvertToTranscriptEvents(t *testing.T) {
	oneof := (&sandcrewv1.BridgeEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	fields := oneof.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Name() == "credential_update" {
			continue
		}
		t.Run(string(field.Name()), func(t *testing.T) {
			event := &sandcrewv1.BridgeEvent{Sequence: 7, Time: timestamppb.Now()}
			message := event.ProtoReflect()
			value := message.NewField(field)
			populate(value.Message())
			message.Set(field, value)
			wire, err := proto.Marshal(event)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var transcript sandcrewv1.TranscriptEvent
			if err := proto.Unmarshal(wire, &transcript); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			transcriptMessage := transcript.ProtoReflect()
			got := transcriptMessage.WhichOneof(transcriptMessage.Descriptor().Oneofs().ByName("event"))
			if got == nil || got.Name() != field.Name() {
				t.Fatalf("case %s did not convert", field.Name())
			}
			if !proto.Equal(message.Get(field).Message().Interface(), transcriptMessage.Get(got).Message().Interface()) {
				t.Fatalf("case %s changed during conversion", field.Name())
			}
		})
	}
}
