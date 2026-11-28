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

// bridgeEventCases are the documented BridgeEvent cases in order.
var bridgeEventCases = []string{
	"hello", "input_accepted", "input_rejected", "user_message_chunk", "agent_message_chunk",
	"agent_thought_chunk", "tool_call", "tool_call_update", "plan", "permission_request",
	"permission_response", "turn_ended", "mode_changed", "config_options", "session_state",
	"bridge_error", "agent_exited", "credential_update", "unknown_update",
}

func TestBridgeEventCases(t *testing.T) {
	oneof := (&sandcrewv1.BridgeEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	if oneof.Fields().Len() != len(bridgeEventCases) {
		t.Fatalf("event has %d cases, want %d", oneof.Fields().Len(), len(bridgeEventCases))
	}
	for i, name := range bridgeEventCases {
		if got := oneof.Fields().Get(i).Name(); string(got) != name {
			t.Errorf("case %d is %q, want %q", i, got, name)
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

// One event of each BridgeEvent case.
func sampleBridgeEvents() []*sandcrewv1.BridgeEvent {
	text := &sandcrewv1.ContentBlock{Block: &sandcrewv1.ContentBlock_Text{Text: "hello"}}
	events := []*sandcrewv1.BridgeEvent{
		{Event: &sandcrewv1.BridgeEvent_Hello{Hello: &sandcrewv1.BridgeHello{
			BridgeVersion:     "1.0.0",
			ProtocolVersion:   &sandcrewv1.ProtocolVersion{Major: 1},
			Vendor:            sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE,
			GuestOs:           sandcrewv1.GuestOs_GUEST_OS_LINUX,
			AgentCapabilities: &sandcrewv1.AgentCapabilities{LoadSession: true},
		}}},
		{Event: &sandcrewv1.BridgeEvent_InputAccepted{InputAccepted: &sandcrewv1.InputAccepted{InputId: "i1"}}},
		{Event: &sandcrewv1.BridgeEvent_InputRejected{InputRejected: &sandcrewv1.InputRejected{
			InputId: "i2", Reason: sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNKNOWN_OPTION,
		}}},
		{Event: &sandcrewv1.BridgeEvent_UserMessageChunk{UserMessageChunk: &sandcrewv1.ContentChunk{Content: text}}},
		{Event: &sandcrewv1.BridgeEvent_AgentMessageChunk{AgentMessageChunk: &sandcrewv1.ContentChunk{Content: text}}},
		{Event: &sandcrewv1.BridgeEvent_AgentThoughtChunk{AgentThoughtChunk: &sandcrewv1.ContentChunk{Content: text}}},
		{Event: &sandcrewv1.BridgeEvent_ToolCall{ToolCall: &sandcrewv1.ToolCall{
			ToolCallId: "t1",
			Title:      "Edit main.go",
			Kind:       sandcrewv1.ToolKind_TOOL_KIND_EDIT,
			Status:     sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_PENDING,
			Content: []*sandcrewv1.ToolCallContent{{Content: &sandcrewv1.ToolCallContent_Diff{Diff: &sandcrewv1.Diff{
				Path: "main.go", OldText: proto.String("a"), NewText: "b",
			}}}},
			Locations: []*sandcrewv1.ToolCallLocation{{Path: "main.go", Line: proto.Uint32(3)}},
			RawInput:  `{"path":"main.go"}`,
		}}},
		{Event: &sandcrewv1.BridgeEvent_ToolCallUpdate{ToolCallUpdate: &sandcrewv1.ToolCallUpdate{
			ToolCallId: "t1",
			Status:     sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_COMPLETED.Enum(),
			Content:    &sandcrewv1.ToolCallContentList{},
			RawOutput:  proto.String(`{"ok":true}`),
		}}},
		{Event: &sandcrewv1.BridgeEvent_Plan{Plan: &sandcrewv1.Plan{Entries: []*sandcrewv1.PlanEntry{{
			Content:  "Write tests",
			Priority: sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_HIGH,
			Status:   sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_IN_PROGRESS,
		}}}}},
		{Event: &sandcrewv1.BridgeEvent_PermissionRequest{PermissionRequest: &sandcrewv1.PermissionRequest{
			PermissionRequestId: "p1",
			ToolCallId:          "t1",
			Options: []*sandcrewv1.PermissionOption{{
				OptionId: "allow", Name: "Allow", Kind: sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_ALLOW_ONCE,
			}},
		}}},
		{Event: &sandcrewv1.BridgeEvent_PermissionResponse{PermissionResponse: &sandcrewv1.PermissionResponse{
			PermissionRequestId: "p1",
			Outcome:             sandcrewv1.PermissionOutcome_PERMISSION_OUTCOME_SELECTED,
			OptionId:            "allow",
			AutoApproved:        true,
		}}},
		{Event: &sandcrewv1.BridgeEvent_TurnEnded{TurnEnded: &sandcrewv1.TurnEnded{
			StopReason: sandcrewv1.StopReason_STOP_REASON_END_TURN,
		}}},
		{Event: &sandcrewv1.BridgeEvent_ModeChanged{ModeChanged: &sandcrewv1.ModeChanged{
			CurrentModeId:  "code",
			AvailableModes: []*sandcrewv1.SessionMode{{ModeId: "code", Name: "Code"}},
			PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_ASK,
		}}},
		{Event: &sandcrewv1.BridgeEvent_ConfigOptions{ConfigOptions: &sandcrewv1.ConfigOptions{Options: []*sandcrewv1.ConfigOption{{
			ConfigOptionId: "model",
			Name:           "Model",
			Category:       sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_MODEL,
			CurrentValueId: "m1",
			Values:         []*sandcrewv1.ConfigOptionValue{{ValueId: "m1", Name: "Model 1"}},
		}}}}},
		{Event: &sandcrewv1.BridgeEvent_SessionState{SessionState: &sandcrewv1.AgentSessionState{
			State: sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING,
		}}},
		{Event: &sandcrewv1.BridgeEvent_BridgeError{BridgeError: &sandcrewv1.BridgeError{
			Kind: sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED, Message: "rejected",
		}}},
		{Event: &sandcrewv1.BridgeEvent_AgentExited{AgentExited: &sandcrewv1.AgentExited{
			Status: &sandcrewv1.AgentExited_ExitCode{ExitCode: 0},
		}}},
		{Event: &sandcrewv1.BridgeEvent_CredentialUpdate{CredentialUpdate: &sandcrewv1.CredentialUpdate{
			Kind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON, Revision: 2,
		}}},
		{Event: &sandcrewv1.BridgeEvent_UnknownUpdate{UnknownUpdate: &sandcrewv1.UnknownUpdate{
			SessionUpdate: "usage_update", RawJson: `{"sessionUpdate":"usage_update"}`,
		}}},
	}
	for i, event := range events {
		event.Sequence = uint64(i + 1)
		event.LogEndOffset = uint64(100 * (i + 1))
		event.Time = timestamppb.Now()
	}
	return events
}

// Every BridgeEvent case round-trips through protobuf JSON, the encoding of
// the event lines.
func TestBridgeEventsRoundTripThroughJSON(t *testing.T) {
	events := sampleBridgeEvents()
	cases := map[string]bool{}
	for _, event := range events {
		name := string(event.ProtoReflect().WhichOneof(event.ProtoReflect().Descriptor().Oneofs().ByName("event")).Name())
		cases[name] = true
		line, err := protojson.Marshal(event)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(line), "\n") {
			t.Errorf("%s: the JSON line contains a line break", name)
		}
		var got sandcrewv1.BridgeEvent
		if err := protojson.Unmarshal(line, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if !proto.Equal(&got, event) {
			t.Errorf("%s: round trip changed the event: %s", name, line)
		}
	}
	for _, name := range bridgeEventCases {
		if !cases[name] {
			t.Errorf("no sample for case %q", name)
		}
	}
}

// Every BridgeEvent case except credential_update has a TranscriptEvent case
// with the same wire encoding.
func TestBridgeEventsConvertToTranscriptEvents(t *testing.T) {
	for _, event := range sampleBridgeEvents() {
		if event.GetCredentialUpdate() != nil {
			continue
		}
		wire, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var transcript sandcrewv1.TranscriptEvent
		// Unknown fields keep the log_end_offset and time numbers of
		// BridgeEvent, so compare only the event case.
		if err := proto.Unmarshal(wire, &transcript); err != nil {
			t.Fatal(err)
		}
		bridgeCase := event.ProtoReflect().WhichOneof(event.ProtoReflect().Descriptor().Oneofs().ByName("event"))
		transcriptCase := transcript.ProtoReflect().WhichOneof(transcript.ProtoReflect().Descriptor().Oneofs().ByName("event"))
		if transcriptCase == nil || transcriptCase.Name() != bridgeCase.Name() {
			t.Errorf("case %s did not convert", bridgeCase.Name())
			continue
		}
		if !proto.Equal(
			event.ProtoReflect().Get(bridgeCase).Message().Interface(),
			transcript.ProtoReflect().Get(transcriptCase).Message().Interface(),
		) {
			t.Errorf("case %s changed during conversion", bridgeCase.Name())
		}
	}
}

// Every BridgeInput case round-trips through protobuf JSON.
func TestBridgeInputsRoundTripThroughJSON(t *testing.T) {
	inputs := []*sandcrewv1.BridgeInput{
		{Input: &sandcrewv1.BridgeInput_Prompt{Prompt: &sandcrewv1.PromptInput{Text: "hi", AttachmentPath: proto.String(".sandcrew/previous-session.md")}}},
		{Input: &sandcrewv1.BridgeInput_PermissionAnswer{PermissionAnswer: &sandcrewv1.PermissionAnswerInput{PermissionRequestId: "p1", OptionId: "allow"}}},
		{Input: &sandcrewv1.BridgeInput_CancelTurn{CancelTurn: &sandcrewv1.CancelTurnInput{}}},
		{Input: &sandcrewv1.BridgeInput_SetMode{SetMode: &sandcrewv1.SetModeInput{Mode: &sandcrewv1.SetModeInput_PermissionMode{PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_ASK}}}},
		{Input: &sandcrewv1.BridgeInput_SetConfigOption{SetConfigOption: &sandcrewv1.SetConfigOptionInput{ConfigOptionId: "model", ValueId: "m1"}}},
	}
	oneof := (&sandcrewv1.BridgeInput{}).ProtoReflect().Descriptor().Oneofs().ByName("input")
	if oneof.Fields().Len() != len(inputs) {
		t.Fatalf("input has %d cases, samples cover %d", oneof.Fields().Len(), len(inputs))
	}
	for _, input := range inputs {
		input.InputId = "0190a8f4-0000-7000-8000-000000000000"
		input.CreateTime = timestamppb.Now()
		line, err := protojson.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var got sandcrewv1.BridgeInput
		if err := protojson.Unmarshal(line, &got); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(&got, input) {
			t.Errorf("round trip changed the input: %s", line)
		}
	}
}
