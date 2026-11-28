// SPDX-License-Identifier: Apache-2.0

// Package normalize converts ACP session updates and objects into the
// bridge event schema of protos/delino/sandcrew/v1. The mapping follows
// docs/protos-sandcrew-api-contract.md#bridge-schema: an update without a
// mapped case becomes unknown_update with its original JSON, and a mapped
// update that does not parse is an error that the bridge reports as
// ACP_PROTOCOL_ERROR.
package normalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/acp"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// UpdateKind is the ACP `sessionUpdate` value.
type UpdateKind string

// The ACP session update kinds that the bridge maps.
const (
	UpdateUserMessageChunk  UpdateKind = "user_message_chunk"
	UpdateAgentMessageChunk UpdateKind = "agent_message_chunk"
	UpdateAgentThoughtChunk UpdateKind = "agent_thought_chunk"
	UpdateToolCall          UpdateKind = "tool_call"
	UpdateToolCallUpdate    UpdateKind = "tool_call_update"
	UpdatePlan              UpdateKind = "plan"
	UpdateCurrentMode       UpdateKind = "current_mode_update"
	UpdateConfigOptions     UpdateKind = "config_option_update"
)

// Update is one normalized ACP session update. Exactly one of the fields is
// set: Event for updates that become an event directly, CurrentModeID for
// current_mode_update and ConfigOptions for config_option_update (the
// bridge combines both with its session state).
type Update struct {
	Event         *sandcrewv1.BridgeEvent
	CurrentModeID *string
	ConfigOptions []ConfigOption
}

// SessionUpdate normalizes the `update` object of an ACP `session/update`.
func SessionUpdate(raw json.RawMessage) (Update, error) {
	var head struct {
		SessionUpdate UpdateKind `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Update{}, fmt.Errorf("normalize: session update: %w", err)
	}
	if head.SessionUpdate == "" {
		return Update{}, errors.New("normalize: session update without sessionUpdate")
	}
	switch head.SessionUpdate {
	case UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateAgentThoughtChunk:
		var chunk struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return Update{}, fmt.Errorf("normalize: %s: %w", head.SessionUpdate, err)
		}
		block, err := ContentBlock(chunk.Content)
		if err != nil {
			return Update{}, fmt.Errorf("normalize: %s: %w", head.SessionUpdate, err)
		}
		content := &sandcrewv1.ContentChunk{Content: block}
		event := &sandcrewv1.BridgeEvent{}
		switch head.SessionUpdate {
		case UpdateUserMessageChunk:
			event.Event = &sandcrewv1.BridgeEvent_UserMessageChunk{UserMessageChunk: content}
		case UpdateAgentMessageChunk:
			event.Event = &sandcrewv1.BridgeEvent_AgentMessageChunk{AgentMessageChunk: content}
		default:
			event.Event = &sandcrewv1.BridgeEvent_AgentThoughtChunk{AgentThoughtChunk: content}
		}
		return Update{Event: event}, nil
	case UpdateToolCall:
		call, err := ToolCall(raw)
		if err != nil {
			return Update{}, err
		}
		return Update{Event: &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_ToolCall{ToolCall: call}}}, nil
	case UpdateToolCallUpdate:
		update, err := ToolCallUpdate(raw)
		if err != nil {
			return Update{}, err
		}
		return Update{Event: &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_ToolCallUpdate{ToolCallUpdate: update}}}, nil
	case UpdatePlan:
		plan, err := Plan(raw)
		if err != nil {
			return Update{}, err
		}
		return Update{Event: &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_Plan{Plan: plan}}}, nil
	case UpdateCurrentMode:
		var mode struct {
			CurrentModeID *string `json:"currentModeId"`
		}
		if err := json.Unmarshal(raw, &mode); err != nil || mode.CurrentModeID == nil {
			return Update{}, fmt.Errorf("normalize: current_mode_update without currentModeId")
		}
		return Update{CurrentModeID: mode.CurrentModeID}, nil
	case UpdateConfigOptions:
		var update struct {
			ConfigOptions []json.RawMessage `json:"configOptions"`
		}
		if err := json.Unmarshal(raw, &update); err != nil {
			return Update{}, fmt.Errorf("normalize: config_option_update: %w", err)
		}
		options, err := ConfigOptions(update.ConfigOptions)
		if err != nil {
			return Update{}, err
		}
		if options == nil {
			options = []ConfigOption{}
		}
		return Update{ConfigOptions: options}, nil
	default:
		return Update{Event: &sandcrewv1.BridgeEvent{Event: &sandcrewv1.BridgeEvent_UnknownUpdate{UnknownUpdate: &sandcrewv1.UnknownUpdate{
			SessionUpdate: string(head.SessionUpdate),
			RawJson:       compact(raw),
		}}}}, nil
	}
}

// ContentBlock maps an ACP content block. Text and resource links have
// their own cases; every other block keeps its original JSON.
func ContentBlock(raw json.RawMessage) (*sandcrewv1.ContentBlock, error) {
	if !present(raw) {
		return nil, errors.New("content block missing")
	}
	var block struct {
		Type        string  `json:"type"`
		Text        *string `json:"text"`
		URI         string  `json:"uri"`
		Name        string  `json:"name"`
		Title       string  `json:"title"`
		Description string  `json:"description"`
		MimeType    string  `json:"mimeType"`
		Size        *int64  `json:"size"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("content block: %w", err)
	}
	switch block.Type {
	case "text":
		if block.Text == nil {
			return nil, errors.New("text content block without text")
		}
		return &sandcrewv1.ContentBlock{Block: &sandcrewv1.ContentBlock_Text{Text: *block.Text}}, nil
	case "resource_link":
		return &sandcrewv1.ContentBlock{Block: &sandcrewv1.ContentBlock_ResourceLink{ResourceLink: &sandcrewv1.ResourceLink{
			Uri:         block.URI,
			Name:        block.Name,
			Title:       block.Title,
			Description: block.Description,
			MimeType:    block.MimeType,
			Size:        block.Size,
		}}}, nil
	default:
		return &sandcrewv1.ContentBlock{Block: &sandcrewv1.ContentBlock_Unsupported{Unsupported: compact(raw)}}, nil
	}
}

type toolCallJSON struct {
	ToolCallID *string         `json:"toolCallId"`
	Title      json.RawMessage `json:"title"`
	Kind       json.RawMessage `json:"kind"`
	Status     json.RawMessage `json:"status"`
	Content    json.RawMessage `json:"content"`
	Locations  json.RawMessage `json:"locations"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}

func parseToolCall(raw json.RawMessage) (toolCallJSON, error) {
	var call toolCallJSON
	if err := json.Unmarshal(raw, &call); err != nil {
		return call, fmt.Errorf("normalize: tool call: %w", err)
	}
	if call.ToolCallID == nil || *call.ToolCallID == "" {
		return call, errors.New("normalize: tool call without toolCallId")
	}
	return call, nil
}

// ToolCall maps an ACP `tool_call` update.
func ToolCall(raw json.RawMessage) (*sandcrewv1.ToolCall, error) {
	call, err := parseToolCall(raw)
	if err != nil {
		return nil, err
	}
	out := &sandcrewv1.ToolCall{ToolCallId: *call.ToolCallID}
	if out.Title, err = optionalString(call.Title); err != nil {
		return nil, fmt.Errorf("normalize: tool call title: %w", err)
	}
	if kind, err := optionalString(call.Kind); err != nil {
		return nil, fmt.Errorf("normalize: tool call kind: %w", err)
	} else if present(call.Kind) {
		out.Kind = toolKind(kind)
	}
	if status, err := optionalString(call.Status); err != nil {
		return nil, fmt.Errorf("normalize: tool call status: %w", err)
	} else if present(call.Status) {
		out.Status = toolCallStatus(status)
	} else {
		// ACP: a tool call without status is pending.
		out.Status = sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_PENDING
	}
	if out.Content, err = toolCallContents(call.Content); err != nil {
		return nil, err
	}
	if out.Locations, err = toolCallLocations(call.Locations); err != nil {
		return nil, err
	}
	if present(call.RawInput) {
		out.RawInput = compact(call.RawInput)
	}
	if present(call.RawOutput) {
		out.RawOutput = compact(call.RawOutput)
	}
	return out, nil
}

// ToolCallUpdate maps an ACP `tool_call_update` update or the `toolCall`
// of a permission request. Fields that are missing or null stay unset.
func ToolCallUpdate(raw json.RawMessage) (*sandcrewv1.ToolCallUpdate, error) {
	call, err := parseToolCall(raw)
	if err != nil {
		return nil, err
	}
	out := &sandcrewv1.ToolCallUpdate{ToolCallId: *call.ToolCallID}
	if present(call.Title) {
		title, err := optionalString(call.Title)
		if err != nil {
			return nil, fmt.Errorf("normalize: tool call title: %w", err)
		}
		out.Title = &title
	}
	if present(call.Kind) {
		kind, err := optionalString(call.Kind)
		if err != nil {
			return nil, fmt.Errorf("normalize: tool call kind: %w", err)
		}
		value := toolKind(kind)
		out.Kind = &value
	}
	if present(call.Status) {
		status, err := optionalString(call.Status)
		if err != nil {
			return nil, fmt.Errorf("normalize: tool call status: %w", err)
		}
		value := toolCallStatus(status)
		out.Status = &value
	}
	if present(call.Content) {
		items, err := toolCallContents(call.Content)
		if err != nil {
			return nil, err
		}
		out.Content = &sandcrewv1.ToolCallContentList{Items: items}
	}
	if present(call.Locations) {
		items, err := toolCallLocations(call.Locations)
		if err != nil {
			return nil, err
		}
		out.Locations = &sandcrewv1.ToolCallLocationList{Items: items}
	}
	if present(call.RawInput) {
		value := compact(call.RawInput)
		out.RawInput = &value
	}
	if present(call.RawOutput) {
		value := compact(call.RawOutput)
		out.RawOutput = &value
	}
	return out, nil
}

func toolCallContents(raw json.RawMessage) ([]*sandcrewv1.ToolCallContent, error) {
	if !present(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("normalize: tool call content: %w", err)
	}
	out := make([]*sandcrewv1.ToolCallContent, 0, len(items))
	for _, item := range items {
		var content struct {
			Type       string          `json:"type"`
			Content    json.RawMessage `json:"content"`
			Path       string          `json:"path"`
			OldText    *string         `json:"oldText"`
			NewText    string          `json:"newText"`
			TerminalID string          `json:"terminalId"`
		}
		if err := json.Unmarshal(item, &content); err != nil {
			return nil, fmt.Errorf("normalize: tool call content: %w", err)
		}
		switch content.Type {
		case "content":
			block, err := ContentBlock(content.Content)
			if err != nil {
				return nil, fmt.Errorf("normalize: tool call content: %w", err)
			}
			out = append(out, &sandcrewv1.ToolCallContent{Content: &sandcrewv1.ToolCallContent_Block{Block: block}})
		case "diff":
			out = append(out, &sandcrewv1.ToolCallContent{Content: &sandcrewv1.ToolCallContent_Diff{Diff: &sandcrewv1.Diff{
				Path:    content.Path,
				OldText: content.OldText,
				NewText: content.NewText,
			}}})
		case "terminal":
			out = append(out, &sandcrewv1.ToolCallContent{Content: &sandcrewv1.ToolCallContent_Terminal{Terminal: &sandcrewv1.Terminal{TerminalId: content.TerminalID}}})
		default:
			// A content type of a later ACP version keeps its JSON as an
			// unsupported content block.
			out = append(out, &sandcrewv1.ToolCallContent{Content: &sandcrewv1.ToolCallContent_Block{Block: &sandcrewv1.ContentBlock{
				Block: &sandcrewv1.ContentBlock_Unsupported{Unsupported: compact(item)},
			}}})
		}
	}
	return out, nil
}

func toolCallLocations(raw json.RawMessage) ([]*sandcrewv1.ToolCallLocation, error) {
	if !present(raw) {
		return nil, nil
	}
	var items []struct {
		Path string  `json:"path"`
		Line *uint32 `json:"line"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("normalize: tool call locations: %w", err)
	}
	out := make([]*sandcrewv1.ToolCallLocation, 0, len(items))
	for _, item := range items {
		out = append(out, &sandcrewv1.ToolCallLocation{Path: item.Path, Line: item.Line})
	}
	return out, nil
}

// Plan maps an ACP `plan` update.
func Plan(raw json.RawMessage) (*sandcrewv1.Plan, error) {
	var plan struct {
		Entries []struct {
			Content  string `json:"content"`
			Priority string `json:"priority"`
			Status   string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("normalize: plan: %w", err)
	}
	out := &sandcrewv1.Plan{}
	for _, entry := range plan.Entries {
		out.Entries = append(out.Entries, &sandcrewv1.PlanEntry{
			Content:  entry.Content,
			Priority: planPriority(entry.Priority),
			Status:   planStatus(entry.Status),
		})
	}
	return out, nil
}

// Modes maps the ACP session modes.
func Modes(modes []acp.SessionMode) []*sandcrewv1.SessionMode {
	out := make([]*sandcrewv1.SessionMode, 0, len(modes))
	for _, mode := range modes {
		out = append(out, &sandcrewv1.SessionMode{ModeId: mode.ID, Name: mode.Name, Description: mode.Description})
	}
	return out
}

// PermissionOptions maps the options of a permission request.
func PermissionOptions(options []acp.PermissionOption) []*sandcrewv1.PermissionOption {
	out := make([]*sandcrewv1.PermissionOption, 0, len(options))
	for _, option := range options {
		out = append(out, &sandcrewv1.PermissionOption{OptionId: option.OptionID, Name: option.Name, Kind: PermissionOptionKind(option.Kind)})
	}
	return out
}

// ConfigOption is one agent configuration option with the type that the
// bridge needs to set it.
type ConfigOption struct {
	Option *sandcrewv1.ConfigOption
	// Boolean is true for an ACP boolean option, whose values the bridge
	// shows as the value IDs "true" and "false".
	Boolean bool
}

// Boolean option value IDs.
const (
	BooleanTrue  = "true"
	BooleanFalse = "false"
)

// ConfigOptions maps ACP session configuration options. Grouped select
// values are flattened in order.
func ConfigOptions(raws []json.RawMessage) ([]ConfigOption, error) {
	var out []ConfigOption
	for _, raw := range raws {
		var option struct {
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			Category     string          `json:"category"`
			Type         string          `json:"type"`
			CurrentValue json.RawMessage `json:"currentValue"`
			Options      json.RawMessage `json:"options"`
		}
		if err := json.Unmarshal(raw, &option); err != nil {
			return nil, fmt.Errorf("normalize: config option: %w", err)
		}
		if option.ID == "" {
			return nil, errors.New("normalize: config option without id")
		}
		mapped := &sandcrewv1.ConfigOption{
			ConfigOptionId: option.ID,
			Name:           option.Name,
			Description:    option.Description,
			Category:       configCategory(option.Category),
		}
		switch option.Type {
		case "boolean":
			var current bool
			if present(option.CurrentValue) {
				if err := json.Unmarshal(option.CurrentValue, &current); err != nil {
					return nil, fmt.Errorf("normalize: config option %s value: %w", option.ID, err)
				}
			}
			mapped.CurrentValueId = BooleanFalse
			if current {
				mapped.CurrentValueId = BooleanTrue
			}
			mapped.Values = []*sandcrewv1.ConfigOptionValue{{ValueId: BooleanTrue, Name: "On"}, {ValueId: BooleanFalse, Name: "Off"}}
			out = append(out, ConfigOption{Option: mapped, Boolean: true})
			continue
		default:
			// "select" and options without a type: a value ID with values.
			if present(option.CurrentValue) {
				if err := json.Unmarshal(option.CurrentValue, &mapped.CurrentValueId); err != nil {
					return nil, fmt.Errorf("normalize: config option %s value: %w", option.ID, err)
				}
			}
			values, err := selectValues(option.Options)
			if err != nil {
				return nil, fmt.Errorf("normalize: config option %s values: %w", option.ID, err)
			}
			mapped.Values = values
		}
		out = append(out, ConfigOption{Option: mapped})
	}
	return out, nil
}

func selectValues(raw json.RawMessage) ([]*sandcrewv1.ConfigOptionValue, error) {
	if !present(raw) {
		return nil, nil
	}
	var items []struct {
		Value       string            `json:"value"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Group       string            `json:"group"`
		Options     []json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	var out []*sandcrewv1.ConfigOptionValue
	for _, item := range items {
		if item.Group != "" || item.Options != nil {
			nested, err := selectValues(mustMarshal(item.Options))
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}
		out = append(out, &sandcrewv1.ConfigOptionValue{ValueId: item.Value, Name: item.Name, Description: item.Description})
	}
	return out, nil
}

// StopReason maps an ACP stop reason.
func StopReason(reason string) sandcrewv1.StopReason {
	switch reason {
	case "end_turn":
		return sandcrewv1.StopReason_STOP_REASON_END_TURN
	case "max_tokens":
		return sandcrewv1.StopReason_STOP_REASON_MAX_TOKENS
	case "max_turn_requests":
		return sandcrewv1.StopReason_STOP_REASON_MAX_TURN_REQUESTS
	case "refusal":
		return sandcrewv1.StopReason_STOP_REASON_REFUSAL
	case "cancelled":
		return sandcrewv1.StopReason_STOP_REASON_CANCELLED
	default:
		return sandcrewv1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// PermissionOptionKind maps an ACP permission option kind.
func PermissionOptionKind(kind string) sandcrewv1.PermissionOptionKind {
	switch kind {
	case acp.PermissionAllowOnce:
		return sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_ALLOW_ONCE
	case acp.PermissionAllowAlways:
		return sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_ALLOW_ALWAYS
	case acp.PermissionRejectOnce:
		return sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_REJECT_ONCE
	case acp.PermissionRejectAlways:
		return sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_REJECT_ALWAYS
	default:
		return sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_UNSPECIFIED
	}
}

func toolKind(kind string) sandcrewv1.ToolKind {
	switch kind {
	case "read":
		return sandcrewv1.ToolKind_TOOL_KIND_READ
	case "edit":
		return sandcrewv1.ToolKind_TOOL_KIND_EDIT
	case "delete":
		return sandcrewv1.ToolKind_TOOL_KIND_DELETE
	case "move":
		return sandcrewv1.ToolKind_TOOL_KIND_MOVE
	case "search":
		return sandcrewv1.ToolKind_TOOL_KIND_SEARCH
	case "execute":
		return sandcrewv1.ToolKind_TOOL_KIND_EXECUTE
	case "think":
		return sandcrewv1.ToolKind_TOOL_KIND_THINK
	case "fetch":
		return sandcrewv1.ToolKind_TOOL_KIND_FETCH
	case "switch_mode":
		return sandcrewv1.ToolKind_TOOL_KIND_SWITCH_MODE
	default:
		// ACP: "other" and kinds of later versions.
		return sandcrewv1.ToolKind_TOOL_KIND_OTHER
	}
}

func toolCallStatus(status string) sandcrewv1.ToolCallStatus {
	switch status {
	case "pending":
		return sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_PENDING
	case "in_progress":
		return sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_IN_PROGRESS
	case "completed":
		return sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_COMPLETED
	case "failed":
		return sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_FAILED
	default:
		return sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_UNSPECIFIED
	}
}

func planPriority(priority string) sandcrewv1.PlanEntryPriority {
	switch priority {
	case "high":
		return sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_HIGH
	case "medium":
		return sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_MEDIUM
	case "low":
		return sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_LOW
	default:
		return sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_UNSPECIFIED
	}
}

func planStatus(status string) sandcrewv1.PlanEntryStatus {
	switch status {
	case "pending":
		return sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_PENDING
	case "in_progress":
		return sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_IN_PROGRESS
	case "completed":
		return sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_COMPLETED
	default:
		return sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_UNSPECIFIED
	}
}

func configCategory(category string) sandcrewv1.ConfigOptionCategory {
	switch category {
	case "mode":
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_MODE
	case "model":
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_MODEL
	case "model_config":
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_MODEL_CONFIG
	case "speed":
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_SPEED
	case "thought_level":
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_THOUGHT_LEVEL
	default:
		// Missing and custom categories.
		return sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_OTHER
	}
}

func optionalString(raw json.RawMessage) (string, error) {
	if !present(raw) {
		return "", nil
	}
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}

func present(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// compact returns raw as compact JSON text, or raw unchanged when it does
// not parse (the caller already parsed it).
func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
