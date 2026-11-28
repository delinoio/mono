// SPDX-License-Identifier: Apache-2.0

package acp

import "encoding/json"

// ProtocolVersion is the ACP major version that the bridge speaks.
const ProtocolVersion = 1

// ACP method names.
const (
	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionSetMode    = "session/set_mode"
	MethodSessionSetConfig  = "session/set_config_option"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// InitializeRequest is the params of `initialize`.
type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

// ClientCapabilities: the bridge offers no file system or terminal
// capability, so the agent uses its own tools in the sandbox.
type ClientCapabilities struct {
	FS       FileSystemCapability `json:"fs"`
	Terminal bool                 `json:"terminal"`
}

// FileSystemCapability is the client file system capability.
type FileSystemCapability struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// Implementation names a client or an agent.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// InitializeResponse is the result of `initialize`.
type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	AgentInfo         *Implementation   `json:"agentInfo"`
}

// AgentCapabilities are the optional capabilities of the agent.
type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

// SessionCapabilities: a present object marks a supported method.
type SessionCapabilities struct {
	Resume json.RawMessage `json:"resume,omitempty"`
}

// SupportsResume reports whether the agent advertises `session/resume`.
func (c SessionCapabilities) SupportsResume() bool {
	return len(c.Resume) > 0 && string(c.Resume) != "null"
}

// AuthMethod is an authentication method of the agent.
type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AuthenticateRequest is the params of `authenticate`.
type AuthenticateRequest struct {
	MethodID string `json:"methodId"`
}

// NewSessionRequest is the params of `session/new`.
type NewSessionRequest struct {
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

// NewSessionResponse is the result of `session/new`.
type NewSessionResponse struct {
	SessionID     string            `json:"sessionId"`
	Modes         *SessionModeState `json:"modes"`
	ConfigOptions []json.RawMessage `json:"configOptions"`
}

// SessionModeState is the mode state of a session.
type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// SessionMode is one mode that the agent offers.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ContentBlock is an ACP content block that the bridge sends.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptRequest is the params of `session/prompt`.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResponse is the result of `session/prompt`.
type PromptResponse struct {
	StopReason string `json:"stopReason"`
}

// CancelNotification is the params of `session/cancel`.
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// SetModeRequest is the params of `session/set_mode`.
type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// SetConfigOptionRequest is the params of `session/set_config_option`.
// Value is a value ID for a select option. For a boolean option, Type is
// "boolean" and Value a JSON boolean.
type SetConfigOptionRequest struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Type      string `json:"type,omitempty"`
	Value     any    `json:"value"`
}

// SetConfigOptionResponse is the result of `session/set_config_option`.
type SetConfigOptionResponse struct {
	ConfigOptions []json.RawMessage `json:"configOptions"`
}

// SessionNotification is the params of `session/update`.
type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// RequestPermissionRequest is the params of `session/request_permission`.
type RequestPermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionOption is one answer of a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Permission option kinds.
const (
	PermissionAllowOnce    = "allow_once"
	PermissionAllowAlways  = "allow_always"
	PermissionRejectOnce   = "reject_once"
	PermissionRejectAlways = "reject_always"
)

// RequestPermissionResponse is the result of `session/request_permission`.
type RequestPermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is "selected" with an option ID or "cancelled".
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// Permission outcomes.
const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)
