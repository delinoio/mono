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
