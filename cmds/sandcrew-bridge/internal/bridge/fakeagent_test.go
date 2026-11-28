// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/agent"
)

// fakeAgent is an ACP agent that runs in the test process and talks
// JSON-RPC over pipes, like the adapter process. The prompt text selects
// the behavior of a turn:
//
//   - "all-updates": sends one session update of every kind, then ends the
//     turn with end_turn.
//   - "permission": asks for permission and ends the turn after the answer.
//   - "block": ends the turn with cancelled after session/cancel.
//   - "wait": ends the turn with end_turn after release is closed.
//   - "exit": ends the process with exit code 7.
//   - anything else: answers with one agent message.
type fakeAgent struct {
	t *testing.T

	toClient   *io.PipeWriter
	fromClient *io.PipeReader
	clientOut  *io.PipeReader
	clientIn   *io.PipeWriter

	writeMu sync.Mutex

	mu             sync.Mutex
	methods        []string
	authMethodIDs  []string
	setModes       []string
	setOptions     []map[string]any
	permissions    []map[string]any
	modes          []string
	currentMode    string
	modelValue     string
	fast           bool
	nextID         int
	responses      map[string]chan json.RawMessage
	cancelled      chan struct{}
	release        chan struct{}
	exited         chan struct{}
	exitOnce       sync.Once
	exitCode       int
	exitSignal     string
	withoutModes   bool
	failSessionNew bool
}

func newFakeAgent(t *testing.T) *fakeAgent {
	clientOut, toClient := io.Pipe()
	fromClient, clientIn := io.Pipe()
	f := &fakeAgent{
		t:           t,
		toClient:    toClient,
		fromClient:  fromClient,
		clientOut:   clientOut,
		clientIn:    clientIn,
		modes:       []string{"default", "bypassPermissions", "plan"},
		currentMode: "default",
		modelValue:  "sonnet",
		responses:   make(map[string]chan json.RawMessage),
		cancelled:   make(chan struct{}, 8),
		release:     make(chan struct{}),
		exited:      make(chan struct{}),
	}
	go f.serve()
	return f
}

// Process implementation.

func (f *fakeAgent) Stdin() io.WriteCloser { return f.clientIn }
func (f *fakeAgent) Stdout() io.Reader     { return f.clientOut }

func (f *fakeAgent) Wait() agent.Exit {
	<-f.exited
	return agent.Exit{Code: f.exitCode, Signal: f.exitSignal}
}

func (f *fakeAgent) Kill() { f.exit(-1, "terminated") }

func (f *fakeAgent) exit(code int, signal string) {
	f.exitOnce.Do(func() {
		f.exitCode, f.exitSignal = code, signal
		f.toClient.Close()
		f.fromClient.Close()
		close(f.exited)
	})
}

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (f *fakeAgent) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.toClient.Write(append(data, '\n'))
}

func (f *fakeAgent) sendRaw(line string) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.toClient.Write([]byte(line + "\n"))
}

func (f *fakeAgent) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeAgent) replyError(id json.RawMessage, code int, message string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (f *fakeAgent) update(update map[string]any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fake-session", "update": update}})
}

// request sends a request to the client and waits for the result.
func (f *fakeAgent) request(method string, params any) json.RawMessage {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("agent-%d", f.nextID)
	ch := make(chan json.RawMessage, 1)
	f.responses[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case result := <-ch:
		return result
	case <-f.exited:
		return nil
	}
}

func (f *fakeAgent) serve() {
	scanner := bufio.NewScanner(f.fromClient)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		var m rpc
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			f.t.Errorf("fake agent: malformed message from the bridge: %v", err)
			continue
		}
		if m.Method == "" {
			var id string
			_ = json.Unmarshal(m.ID, &id)
			f.mu.Lock()
			ch := f.responses[id]
			delete(f.responses, id)
			f.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		f.mu.Unlock()
		go f.handle(m)
	}
}

func (f *fakeAgent) configOptions() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []map[string]any{
		{
			"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": f.modelValue,
			"options": []map[string]any{
				{"group": "main", "name": "Main", "options": []map[string]any{{"value": "sonnet", "name": "Sonnet"}, {"value": "opus", "name": "Opus"}}},
			},
		},
		{"id": "fast", "name": "Fast", "category": "speed", "type": "boolean", "currentValue": f.fast},
	}
}

func (f *fakeAgent) modeState() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var available []map[string]any
	for _, id := range f.modes {
		available = append(available, map[string]any{"id": id, "name": strings.ToUpper(id[:1]) + id[1:]})
	}
	return map[string]any{"currentModeId": f.currentMode, "availableModes": available}
}

func (f *fakeAgent) handle(m rpc) {
	switch m.Method {
	case "initialize":
		f.reply(m.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":         true,
				"sessionCapabilities": map[string]any{"resume": map[string]any{}},
			},
			"authMethods": []map[string]any{{"id": "api-key", "name": "API key"}},
			"agentInfo":   map[string]any{"name": "fake-agent", "version": "9.9.9"},
		})
	case "authenticate":
		var params struct {
			MethodID string `json:"methodId"`
		}
		_ = json.Unmarshal(m.Params, &params)
		f.mu.Lock()
		f.authMethodIDs = append(f.authMethodIDs, params.MethodID)
		f.mu.Unlock()
		f.reply(m.ID, map[string]any{})
	case "session/new":
		if f.failSessionNew {
			f.replyError(m.ID, -32000, "Authentication required")
			return
		}
		result := map[string]any{"sessionId": "fake-session", "configOptions": f.configOptions()}
		if !f.withoutModes {
			result["modes"] = f.modeState()
		}
		f.reply(m.ID, result)
	case "session/set_mode":
		var params struct {
			ModeID string `json:"modeId"`
		}
		_ = json.Unmarshal(m.Params, &params)
		f.mu.Lock()
		f.setModes = append(f.setModes, params.ModeID)
		known := slices.Contains(f.modes, params.ModeID)
		if known {
			f.currentMode = params.ModeID
		}
		f.mu.Unlock()
		if !known {
			f.replyError(m.ID, -32602, "unknown mode")
			return
		}
		f.reply(m.ID, map[string]any{})
	case "session/set_config_option":
		var params map[string]any
		_ = json.Unmarshal(m.Params, &params)
		f.mu.Lock()
		f.setOptions = append(f.setOptions, params)
		ok := true
		switch params["configId"] {
		case "model":
			value, _ := params["value"].(string)
			if value != "sonnet" && value != "opus" {
				ok = false
			} else {
				f.modelValue = value
			}
		case "fast":
			value, isBool := params["value"].(bool)
			ok = isBool && params["type"] == "boolean"
			f.fast = value
		default:
			ok = false
		}
		f.mu.Unlock()
		if !ok {
			f.replyError(m.ID, -32602, "invalid value")
			return
		}
		f.reply(m.ID, map[string]any{"configOptions": f.configOptions()})
	case "session/cancel":
		f.cancelled <- struct{}{}
	case "session/prompt":
		var params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(m.Params, &params)
		f.prompt(m.ID, params.Prompt[0].Text)
	default:
		f.replyError(m.ID, -32601, "method not found")
	}
}

func (f *fakeAgent) prompt(id json.RawMessage, text string) {
	switch text {
	case "all-updates":
		f.update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "replayed"}})
		f.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "hello"}})
		f.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "resource_link", "uri": "file:///w/a.go", "name": "a.go", "size": 12}})
		f.update(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "Edit a.go", "kind": "edit", "status": "pending",
			"content": []map[string]any{
				{"type": "diff", "path": "/w/a.go", "oldText": "a", "newText": "b"},
				{"type": "diff", "path": "/w/new.go", "newText": "c"},
				{"type": "terminal", "terminalId": "term-1"},
				{"type": "content", "content": map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}},
			},
			"locations": []map[string]any{{"path": "/w/a.go", "line": 3}},
			"rawInput":  map[string]any{"file": "a.go"},
		})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "completed", "rawOutput": map[string]any{"ok": true}, "title": nil})
		f.update(map[string]any{"sessionUpdate": "plan", "entries": []map[string]any{{"content": "Step", "priority": "high", "status": "in_progress"}}})
		f.update(map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "plan"})
		f.update(map[string]any{"sessionUpdate": "config_option_update", "configOptions": f.configOptions()})
		f.update(map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []map[string]any{{"name": "review", "description": "Review"}}})
		f.update(map[string]any{"sessionUpdate": "tool_call", "title": "no id"})
		f.sendRaw("this is not JSON")
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	case "permission":
		result := f.request("session/request_permission", map[string]any{
			"sessionId": "fake-session",
			"toolCall":  map[string]any{"toolCallId": "call-2", "title": "Run ls", "kind": "execute"},
			"options": []map[string]any{
				{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
				{"optionId": "always", "name": "Always", "kind": "allow_always"},
				{"optionId": "once", "name": "Once", "kind": "allow_once"},
			},
		})
		var answer map[string]any
		_ = json.Unmarshal(result, &answer)
		f.mu.Lock()
		f.permissions = append(f.permissions, answer)
		f.mu.Unlock()
		reason := "end_turn"
		if outcome, _ := answer["outcome"].(map[string]any); outcome["outcome"] == "cancelled" {
			reason = "cancelled"
		}
		f.reply(id, map[string]any{"stopReason": reason})
	case "block":
		select {
		case <-f.cancelled:
			f.reply(id, map[string]any{"stopReason": "cancelled"})
		case <-f.exited:
		}
	case "wait":
		select {
		case <-f.release:
			f.reply(id, map[string]any{"stopReason": "end_turn"})
		case <-f.exited:
		}
	case "exit":
		f.exit(7, "")
	default:
		f.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "echo: " + text}})
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	}
}

func (f *fakeAgent) snapshot() (methods, authIDs, setModes []string, setOptions, permissions []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.methods), slices.Clone(f.authMethodIDs), slices.Clone(f.setModes), slices.Clone(f.setOptions), slices.Clone(f.permissions)
}

func (f *fakeAgent) waitCancelled(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		methods, _, _, _, _ := f.snapshot()
		if slices.Contains(methods, "session/cancel") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the agent received no session/cancel")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
