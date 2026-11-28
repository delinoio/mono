// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/agent"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/eventlog"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/install"
	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/pins"
	"github.com/delinoio/mono/go/uuidv7"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

const testCredential = "test-credential-value"

// syncBuffer is a goroutine-safe stdout.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t        *testing.T
	stateDir string
	stdout   *syncBuffer
	agent    *fakeAgent
	launches []Launch
	cfg      Config
	cancel   context.CancelFunc
	done     chan ExitCode
}

type harnessOption func(*harness, *sandcrewv1.BridgeStartConfig)

func newHarness(t *testing.T, options ...harnessOption) *harness {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), ".sandcrew")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, stateDir: stateDir, stdout: &syncBuffer{}}
	start := &sandcrewv1.BridgeStartConfig{
		SessionId:      "0190f3c4-0000-7000-8000-000000000001",
		Vendor:         sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE,
		CredentialKind: sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CLAUDE_OAUTH_TOKEN,
		PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE,
	}
	p, err := pins.Load()
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = Config{
		StateDir:      stateDir,
		Environ:       []string{"PATH=/usr/bin", "SANDCREW_SESSION_ID=" + start.SessionId, "SANDCREW_VENDOR_CREDENTIAL=" + testCredential, "MY_SECRET=kept"},
		Stdout:        h.stdout,
		BridgeVersion: "1.2.3+abc",
		GuestOS:       sandcrewv1.GuestOs_GUEST_OS_LINUX,
		Target:        pins.TargetLinuxAMD64,
		Pins:          p,
		Install: func(context.Context, sandcrewv1.AgentVendor) (install.Result, error) {
			return install.Result{RuntimePath: "/agents/node/bin/node", AdapterEntry: "/agents/adapter/dist/index.js"}, nil
		},
		StartAgent: func(_ context.Context, launch Launch) (agent.Agent, error) {
			h.launches = append(h.launches, launch)
			return agent.NewACPAgent(h.agent, agent.ACPOptions{Handler: launch.Handler, ClientInfo: launch.ClientInfo, AuthMethodID: launch.AuthMethodID}), nil
		},
		PollInterval:         5 * time.Millisecond,
		FallbackPollInterval: 5 * time.Millisecond,
		CredentialInterval:   5 * time.Millisecond,
	}
	h.agent = newFakeAgent(t)
	for _, option := range options {
		option(h, start)
	}
	h.writeStart(start)
	return h
}

func (h *harness) writeStart(start *sandcrewv1.BridgeStartConfig) {
	data, err := protojson.Marshal(start)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.stateDir, StartFileName), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan ExitCode, 1)
	go func() { h.done <- Run(ctx, h.cfg) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Error("the bridge did not stop")
		}
	})
}

func (h *harness) wait() ExitCode {
	h.t.Helper()
	select {
	case code := <-h.done:
		h.done <- code
		return code
	case <-time.After(10 * time.Second):
		h.t.Fatal("the bridge did not exit")
		return 0
	}
}

func (h *harness) stop() ExitCode {
	h.cancel()
	return h.wait()
}

// events parses every stdout line.
func (h *harness) events() []*sandcrewv1.BridgeEvent {
	h.t.Helper()
	var out []*sandcrewv1.BridgeEvent
	for _, line := range strings.Split(strings.TrimSuffix(h.stdout.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		event, err := eventlog.Decode([]byte(line))
		if err != nil {
			h.t.Fatalf("stdout line does not parse: %v: %s", err, line)
		}
		out = append(out, event)
	}
	return out
}

func kind(event *sandcrewv1.BridgeEvent) string { return eventKind(event) }

func kinds(events []*sandcrewv1.BridgeEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, kind(event))
	}
	return out
}

// waitFor waits until an event after index from matches match and returns
// its index.
func (h *harness) waitFor(from int, what string, match func(*sandcrewv1.BridgeEvent) bool) int {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events := h.events()
		for i := from; i < len(events); i++ {
			if match(events[i]) {
				return i
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("no %s event; events: %v", what, kinds(h.events()))
	return -1
}

func (h *harness) waitKind(from int, k string) int {
	h.t.Helper()
	return h.waitFor(from, k, func(e *sandcrewv1.BridgeEvent) bool { return kind(e) == k })
}

func (h *harness) waitState(from int, state sandcrewv1.AgentActivity) int {
	h.t.Helper()
	return h.waitFor(from, state.String(), func(e *sandcrewv1.BridgeEvent) bool { return e.GetSessionState().GetState() == state })
}

func (h *harness) waitIdle() int {
	h.t.Helper()
	return h.waitState(0, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
}

// write writes an input file the way WriteDevSessionFile does: a temporary
// name and a rename.
func (h *harness) write(input *sandcrewv1.BridgeInput) string {
	h.t.Helper()
	if input.InputId == "" {
		id, err := uuidv7.NewString()
		if err != nil {
			h.t.Fatal(err)
		}
		input.InputId = id
	}
	data, err := protojson.Marshal(input)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeRaw(input.InputId+".json", data)
	return input.InputId
}

func (h *harness) writeRaw(name string, data []byte) {
	h.t.Helper()
	dir := filepath.Join(h.stateDir, InboxDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	tmp := filepath.Join(h.stateDir, "tmp-"+name)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		h.t.Fatal(err)
	}
}

func prompt(text string) *sandcrewv1.BridgeInput {
	return &sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_Prompt{Prompt: &sandcrewv1.PromptInput{Text: text}}}
}

func (h *harness) waitAccepted(from int, id string) int {
	h.t.Helper()
	return h.waitFor(from, "input_accepted "+id, func(e *sandcrewv1.BridgeEvent) bool { return e.GetInputAccepted().GetInputId() == id })
}

func (h *harness) waitRejected(from int, id string, reason sandcrewv1.InputRejectionReason) int {
	h.t.Helper()
	i := h.waitFor(from, "input_rejected "+id, func(e *sandcrewv1.BridgeEvent) bool { return e.GetInputRejected().GetInputId() == id })
	if got := h.events()[i].GetInputRejected().GetReason(); got != reason {
		h.t.Fatalf("input %s rejected with %s, want %s", id, got, reason)
	}
	return i
}

// checkLog compares the event log with stdout: the same lines in the same
// order, without credential_update, with consecutive sequence numbers and
// log end offsets that match the byte offsets.
func (h *harness) checkLog() {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.stateDir, EventsFileName))
	if err != nil {
		h.t.Fatal(err)
	}
	var stdoutLogged []string
	for _, line := range strings.SplitAfter(h.stdout.String(), "\n") {
		if line == "" || strings.Contains(line, `"credentialUpdate"`) {
			continue
		}
		stdoutLogged = append(stdoutLogged, line)
	}
	logLines := strings.SplitAfter(string(data), "\n")
	logLines = slices.DeleteFunc(logLines, func(s string) bool { return s == "" })
	if !slices.Equal(stdoutLogged, logLines) {
		h.t.Fatalf("stdout and event log differ:\nstdout: %q\nlog:    %q", stdoutLogged, logLines)
	}
	offset := 0
	for i, line := range logLines {
		offset += len(line)
		event, err := eventlog.Decode([]byte(line))
		if err != nil {
			h.t.Fatal(err)
		}
		if event.GetSequence() != uint64(i+1) {
			h.t.Fatalf("line %d has sequence %d", i, event.GetSequence())
		}
		if event.GetLogEndOffset() != uint64(offset) {
			h.t.Fatalf("line %d has log end offset %d, want %d", i, event.GetLogEndOffset(), offset)
		}
		if event.GetTime() == nil {
			h.t.Fatalf("line %d has no time", i)
		}
	}
}

func inboxFiles(t *testing.T, stateDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, InboxDirName))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestStartWritesHelloFirstAndAppliesAutoApprove(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	events := h.events()

	hello := events[0].GetHello()
	if hello == nil {
		t.Fatalf("first event is %s, want hello", kind(events[0]))
	}
	pinned, _ := h.cfg.Pins.Vendor(sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE)
	want := &sandcrewv1.BridgeHello{
		BridgeVersion:   "1.2.3+abc",
		ProtocolVersion: &sandcrewv1.ProtocolVersion{Major: 1, Minor: 0},
		Vendor:          sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE,
		AgentVersion:    pinned.Agent.Version,
		AdapterVersion:  pinned.Adapter.Version,
		GuestOs:         sandcrewv1.GuestOs_GUEST_OS_LINUX,
	}
	if !proto.Equal(hello, want) {
		t.Fatalf("hello = %v, want %v", hello, want)
	}
	wantKinds := []string{"hello", "session_state", "agent_ready", "config_options", "mode_changed", "session_state"}
	if got := kinds(events[:idle+1]); !slices.Equal(got, wantKinds) {
		t.Fatalf("start events %v, want %v", got, wantKinds)
	}
	if events[1].GetSessionState().GetState() != sandcrewv1.AgentActivity_AGENT_ACTIVITY_STARTING {
		t.Fatalf("second event %v, want STARTING", events[1])
	}
	ready := events[2].GetAgentReady()
	if !ready.GetAgentCapabilities().GetLoadSession() || !ready.GetAgentCapabilities().GetResumeSession() || ready.GetAgentSessionId() != "fake-session" || ready.GetAgentReportedVersion() != "9.9.9" {
		t.Fatalf("agent_ready = %v", ready)
	}
	options := events[3].GetConfigOptions().GetOptions()
	if len(options) != 2 || options[0].GetCurrentValueId() != "sonnet" || len(options[0].GetValues()) != 2 ||
		options[1].GetCategory() != sandcrewv1.ConfigOptionCategory_CONFIG_OPTION_CATEGORY_SPEED || options[1].GetCurrentValueId() != "false" {
		t.Fatalf("config_options = %v", options)
	}
	mode := events[4].GetModeChanged()
	if mode.GetCurrentModeId() != "bypassPermissions" || mode.GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE || len(mode.GetAvailableModes()) != 3 {
		t.Fatalf("mode_changed = %v", mode)
	}
	methods, authIDs, setModes, _, _ := h.agent.snapshot()
	if !slices.Equal(methods[:2], []string{"initialize", "session/new"}) || len(authIDs) != 0 || !slices.Equal(setModes, []string{"bypassPermissions"}) {
		t.Fatalf("agent calls %v, auth %v, modes %v", methods, authIDs, setModes)
	}

	// The agent environment: no SANDCREW_* names, the credential under the
	// vendor name, session secrets unchanged, IS_SANDBOX for the
	// bypassPermissions mode of root guests.
	launch := h.launches[0]
	if launch.Name != "/agents/node/bin/node" || !slices.Equal(launch.Args, []string{"/agents/adapter/dist/index.js"}) || launch.Dir != filepath.Dir(h.stateDir) {
		t.Fatalf("launch = %+v", launch)
	}
	for _, entry := range launch.Env {
		if strings.HasPrefix(entry, "SANDCREW_") {
			t.Fatalf("agent environment contains %s", entry)
		}
	}
	for _, entry := range []string{"CLAUDE_CODE_OAUTH_TOKEN=" + testCredential, "MY_SECRET=kept", "IS_SANDBOX=1"} {
		if !slices.Contains(launch.Env, entry) {
			t.Fatalf("agent environment lacks %s: %v", entry, launch.Env)
		}
	}
	if strings.Contains(h.stdout.String(), testCredential) {
		t.Fatal("stdout contains the credential")
	}
	h.checkLog()
}

func TestNormalizesEveryUpdateKind(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	id := h.write(prompt("all-updates"))
	end := h.waitKind(idle, "turn_ended")
	h.waitState(end, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	events := h.events()[idle+1:]
	want := []string{
		"input_accepted", "user_message_chunk", "session_state",
		"user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan",
		"mode_changed", "config_options", "unknown_update", "bridge_error", "bridge_error", "turn_ended", "session_state",
	}
	if got := kinds(events); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if events[0].GetInputAccepted().GetInputId() != id || events[1].GetUserMessageChunk().GetContent().GetText() != "all-updates" ||
		events[2].GetSessionState().GetState() != sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING {
		t.Fatalf("turn start %v", events[:3])
	}
	if events[3].GetUserMessageChunk().GetContent().GetText() != "replayed" || events[4].GetAgentMessageChunk().GetContent().GetText() != "hello" {
		t.Fatalf("chunks %v %v", events[3], events[4])
	}
	link := events[5].GetAgentThoughtChunk().GetContent().GetResourceLink()
	if link.GetUri() != "file:///w/a.go" || link.GetName() != "a.go" || link.GetSize() != 12 {
		t.Fatalf("resource link %v", link)
	}
	call := events[6].GetToolCall()
	if call.GetToolCallId() != "call-1" || call.GetKind() != sandcrewv1.ToolKind_TOOL_KIND_EDIT || call.GetStatus() != sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_PENDING ||
		call.GetRawInput() != `{"file":"a.go"}` || len(call.GetLocations()) != 1 || call.GetLocations()[0].GetLine() != 3 || len(call.GetContent()) != 4 {
		t.Fatalf("tool call %v", call)
	}
	diff := call.GetContent()[0].GetDiff()
	if diff.GetPath() != "/w/a.go" || diff.GetOldText() != "a" || diff.GetNewText() != "b" || call.GetContent()[1].GetDiff().OldText != nil {
		t.Fatalf("diffs %v", call.GetContent())
	}
	if call.GetContent()[2].GetTerminal().GetTerminalId() != "term-1" || !strings.Contains(call.GetContent()[3].GetBlock().GetUnsupported(), `"type":"image"`) {
		t.Fatalf("terminal and image content %v", call.GetContent())
	}
	update := events[7].GetToolCallUpdate()
	if update.GetToolCallId() != "call-1" || update.GetStatus() != sandcrewv1.ToolCallStatus_TOOL_CALL_STATUS_COMPLETED || update.RawOutput == nil ||
		update.GetRawOutput() != `{"ok":true}` || update.Title != nil || update.Kind != nil || update.Content != nil {
		t.Fatalf("tool call update %v", update)
	}
	entry := events[8].GetPlan().GetEntries()[0]
	if entry.GetContent() != "Step" || entry.GetPriority() != sandcrewv1.PlanEntryPriority_PLAN_ENTRY_PRIORITY_HIGH || entry.GetStatus() != sandcrewv1.PlanEntryStatus_PLAN_ENTRY_STATUS_IN_PROGRESS {
		t.Fatalf("plan %v", entry)
	}
	if events[9].GetModeChanged().GetCurrentModeId() != "plan" || events[9].GetModeChanged().GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		t.Fatalf("mode_changed %v", events[9])
	}
	unknown := events[11].GetUnknownUpdate()
	if unknown.GetSessionUpdate() != "available_commands_update" || !strings.Contains(unknown.GetRawJson(), `"availableCommands"`) {
		t.Fatalf("unknown_update %v", unknown)
	}
	for _, e := range events[12:14] {
		if e.GetBridgeError().GetKind() != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_ACP_PROTOCOL_ERROR {
			t.Fatalf("bridge_error %v, want ACP_PROTOCOL_ERROR", e)
		}
	}
	if events[14].GetTurnEnded().GetStopReason() != sandcrewv1.StopReason_STOP_REASON_END_TURN {
		t.Fatalf("turn_ended %v", events[14])
	}
	if files := inboxFiles(t, h.stateDir); len(files) != 0 {
		t.Fatalf("inbox files remain: %v", files)
	}
	h.checkLog()
}

func TestPromptDuringTurnIsQueued(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	first := h.write(prompt("wait"))
	h.waitState(idle, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING)
	second := h.write(prompt("second"))
	accepted := h.waitAccepted(idle, second)
	// The queued prompt is not sent before the first turn ends.
	time.Sleep(30 * time.Millisecond)
	for _, e := range h.events()[accepted:] {
		if e.GetUserMessageChunk().GetContent().GetText() == "second" {
			t.Fatal("the queued prompt was sent during the turn")
		}
	}
	close(h.agent.release)
	firstEnd := h.waitKind(accepted, "turn_ended")
	secondChunk := h.waitFor(firstEnd, "second prompt", func(e *sandcrewv1.BridgeEvent) bool {
		return e.GetUserMessageChunk().GetContent().GetText() == "second"
	})
	h.waitFor(secondChunk, "second answer", func(e *sandcrewv1.BridgeEvent) bool {
		return e.GetAgentMessageChunk().GetContent().GetText() == "echo: second"
	})
	secondEnd := h.waitKind(secondChunk, "turn_ended")
	h.waitState(secondEnd, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	// Between the turns the bridge stays WORKING: no IDLE before the
	// second turn starts.
	for _, e := range h.events()[firstEnd:secondChunk] {
		if e.GetSessionState().GetState() == sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE {
			t.Fatal("IDLE between a turn and a queued prompt")
		}
	}
	h.waitAccepted(0, first)
	h.checkLog()
}

func TestAutoApproveAnswersAndRecordsPermissionRequests(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	h.write(prompt("permission"))
	request := h.waitKind(idle, "permission_request")
	response := h.waitKind(request, "permission_response")
	h.waitKind(response, "turn_ended")
	events := h.events()
	req := events[request].GetPermissionRequest()
	if req.GetToolCallId() != "call-2" || len(req.GetOptions()) != 3 || req.GetToolCall().GetTitle() != "Run ls" ||
		req.GetOptions()[0].GetKind() != sandcrewv1.PermissionOptionKind_PERMISSION_OPTION_KIND_REJECT_ONCE {
		t.Fatalf("permission_request %v", req)
	}
	if !uuidv7.Valid(req.GetPermissionRequestId()) {
		t.Fatalf("permission request ID %q is no UUID v7", req.GetPermissionRequestId())
	}
	resp := events[response].GetPermissionResponse()
	want := &sandcrewv1.PermissionResponse{
		PermissionRequestId: req.GetPermissionRequestId(),
		Outcome:             sandcrewv1.PermissionOutcome_PERMISSION_OUTCOME_SELECTED,
		OptionId:            "once",
		AutoApproved:        true,
	}
	if !proto.Equal(resp, want) {
		t.Fatalf("permission_response %v, want %v", resp, want)
	}
	for _, e := range events[request:response] {
		if e.GetSessionState().GetState() == sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION {
			t.Fatal("an auto-approved request waited for permission")
		}
	}
	_, _, _, _, permissions := h.agent.snapshot()
	if fmt.Sprint(permissions) != "[map[outcome:map[optionId:once outcome:selected]]]" {
		t.Fatalf("agent received %v", permissions)
	}
	h.checkLog()
}

func askMode(_ *harness, start *sandcrewv1.BridgeStartConfig) {
	start.PermissionMode = sandcrewv1.PermissionMode_PERMISSION_MODE_ASK
}

func TestAskModeForwardsPermissionRequests(t *testing.T) {
	h := newHarness(t, askMode)
	h.run()
	idle := h.waitIdle()
	if mode := h.events()[idle-1].GetModeChanged(); mode.GetCurrentModeId() != "default" || mode.GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_ASK {
		t.Fatalf("start mode_changed %v", mode)
	}
	if _, _, setModes, _, _ := h.agent.snapshot(); len(setModes) != 0 {
		t.Fatalf("the agent already was in its asking mode, but got set_mode %v", setModes)
	}
	h.write(prompt("permission"))
	request := h.waitKind(idle, "permission_request")
	waiting := h.waitState(request, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION)
	requestID := h.events()[request].GetPermissionRequest().GetPermissionRequestId()

	answer := func(requestID, optionID string) string {
		return h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_PermissionAnswer{PermissionAnswer: &sandcrewv1.PermissionAnswerInput{
			PermissionRequestId: requestID, OptionId: optionID,
		}}})
	}
	h.waitRejected(waiting, answer("0190f3c4-0000-7000-8000-00000000dead", "once"), sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNKNOWN_PERMISSION_REQUEST)
	h.waitRejected(waiting, answer(requestID, "missing"), sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNKNOWN_OPTION)
	accepted := h.waitAccepted(waiting, answer(requestID, "reject"))
	response := h.waitKind(accepted, "permission_response")
	if resp := h.events()[response].GetPermissionResponse(); resp.GetAutoApproved() || resp.GetOptionId() != "reject" || resp.GetPermissionRequestId() != requestID {
		t.Fatalf("permission_response %v", resp)
	}
	working := h.waitState(response, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING)
	end := h.waitKind(working, "turn_ended")
	h.waitState(end, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	_, _, _, _, permissions := h.agent.snapshot()
	if fmt.Sprint(permissions) != "[map[outcome:map[optionId:reject outcome:selected]]]" {
		t.Fatalf("agent received %v", permissions)
	}

	// Switching to AUTO_APPROVE answers a pending request.
	h.write(prompt("permission"))
	request = h.waitKind(end+1, "permission_request")
	h.waitState(request, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION)
	h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_SetMode{SetMode: &sandcrewv1.SetModeInput{
		Target: &sandcrewv1.SetModeInput_PermissionMode{PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE},
	}}})
	response = h.waitKind(request, "permission_response")
	if resp := h.events()[response].GetPermissionResponse(); !resp.GetAutoApproved() || resp.GetOptionId() != "once" {
		t.Fatalf("permission_response after the switch %v", resp)
	}
	h.waitFor(response, "AUTO_APPROVE mode_changed", func(e *sandcrewv1.BridgeEvent) bool {
		return e.GetModeChanged().GetPermissionMode() == sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE && e.GetModeChanged().GetCurrentModeId() == "bypassPermissions"
	})
	h.waitKind(response, "turn_ended")
	h.checkLog()
}

func TestCancelTurn(t *testing.T) {
	h := newHarness(t, askMode)
	h.run()
	idle := h.waitIdle()
	cancelInput := func() string {
		return h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_CancelTurn{CancelTurn: &sandcrewv1.CancelTurnInput{}}})
	}
	h.waitRejected(idle, cancelInput(), sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_NO_ACTIVE_TURN)

	// A blocked turn with a queued prompt.
	h.write(prompt("block"))
	working := h.waitState(idle, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WORKING)
	queued := h.write(prompt("dropped"))
	h.waitAccepted(working, queued)
	accepted := h.waitAccepted(working, cancelInput())
	h.agent.waitCancelled(t)
	end := h.waitKind(accepted, "turn_ended")
	if reason := h.events()[end].GetTurnEnded().GetStopReason(); reason != sandcrewv1.StopReason_STOP_REASON_CANCELLED {
		t.Fatalf("stop reason %s", reason)
	}
	idle = h.waitState(end, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	time.Sleep(30 * time.Millisecond)
	for _, e := range h.events()[accepted:] {
		if e.GetUserMessageChunk().GetContent().GetText() == "dropped" {
			t.Fatal("a queued prompt ran after cancel_turn")
		}
	}

	// A pending ASK-mode permission request is answered with cancelled.
	h.write(prompt("permission"))
	request := h.waitKind(idle, "permission_request")
	h.waitState(request, sandcrewv1.AgentActivity_AGENT_ACTIVITY_WAITING_FOR_PERMISSION)
	accepted = h.waitAccepted(request, cancelInput())
	response := h.waitKind(accepted, "permission_response")
	if resp := h.events()[response].GetPermissionResponse(); resp.GetOutcome() != sandcrewv1.PermissionOutcome_PERMISSION_OUTCOME_CANCELLED || resp.GetOptionId() != "" {
		t.Fatalf("permission_response %v", resp)
	}
	end = h.waitKind(response, "turn_ended")
	if reason := h.events()[end].GetTurnEnded().GetStopReason(); reason != sandcrewv1.StopReason_STOP_REASON_CANCELLED {
		t.Fatalf("stop reason %s", reason)
	}
	_, _, _, _, permissions := h.agent.snapshot()
	if fmt.Sprint(permissions) != "[map[outcome:map[outcome:cancelled]]]" {
		t.Fatalf("agent received %v", permissions)
	}
	h.checkLog()
}

func TestModeAndConfigOptionChanges(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	setMode := func(modeID string) string {
		return h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_SetMode{SetMode: &sandcrewv1.SetModeInput{
			Target: &sandcrewv1.SetModeInput_ModeId{ModeId: modeID},
		}}})
	}
	setOption := func(configID, valueID string) string {
		return h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_SetConfigOption{SetConfigOption: &sandcrewv1.SetConfigOptionInput{
			ConfigOptionId: configID, ValueId: valueID,
		}}})
	}

	accepted := h.waitAccepted(idle, setMode("plan"))
	changed := h.waitKind(accepted, "mode_changed")
	if mode := h.events()[changed].GetModeChanged(); mode.GetCurrentModeId() != "plan" || mode.GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		t.Fatalf("mode_changed %v", mode)
	}

	accepted = h.waitAccepted(changed, setMode("nope"))
	rejected := h.waitKind(accepted, "bridge_error")
	if e := h.events()[rejected].GetBridgeError(); e.GetKind() != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED || !strings.Contains(e.GetMessage(), `"nope"`) {
		t.Fatalf("bridge_error %v", e)
	}

	accepted = h.waitAccepted(rejected, h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_SetMode{SetMode: &sandcrewv1.SetModeInput{
		Target: &sandcrewv1.SetModeInput_PermissionMode{PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_ASK},
	}}}))
	changed = h.waitKind(accepted, "mode_changed")
	if mode := h.events()[changed].GetModeChanged(); mode.GetCurrentModeId() != "default" || mode.GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_ASK {
		t.Fatalf("mode_changed after ASK %v", mode)
	}

	accepted = h.waitAccepted(changed, setOption("model", "opus"))
	options := h.waitKind(accepted, "config_options")
	if got := h.events()[options].GetConfigOptions().GetOptions()[0].GetCurrentValueId(); got != "opus" {
		t.Fatalf("model after set %q", got)
	}
	accepted = h.waitAccepted(options, setOption("model", "gpt"))
	rejected = h.waitKind(accepted, "bridge_error")
	if kind := h.events()[rejected].GetBridgeError().GetKind(); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED {
		t.Fatalf("bridge_error %s", kind)
	}
	accepted = h.waitAccepted(rejected, setOption("fast", "true"))
	options = h.waitKind(accepted, "config_options")
	if got := h.events()[options].GetConfigOptions().GetOptions()[1].GetCurrentValueId(); got != "true" {
		t.Fatalf("fast after set %q", got)
	}
	_, _, setModes, setOptions, _ := h.agent.snapshot()
	if !slices.Equal(setModes, []string{"bypassPermissions", "plan", "nope", "default"}) {
		t.Fatalf("set_mode calls %v", setModes)
	}
	if last := setOptions[len(setOptions)-1]; last["type"] != "boolean" || last["value"] != true {
		t.Fatalf("boolean set_config_option %v", last)
	}
	h.checkLog()
}

func TestRejectedPermissionModeSwitchKeepsTheMode(t *testing.T) {
	h := newHarness(t)
	h.agent.rejectModes = []string{"default"}
	h.run()
	idle := h.waitIdle()
	id := h.write(&sandcrewv1.BridgeInput{Input: &sandcrewv1.BridgeInput_SetMode{SetMode: &sandcrewv1.SetModeInput{
		Target: &sandcrewv1.SetModeInput_PermissionMode{PermissionMode: sandcrewv1.PermissionMode_PERMISSION_MODE_ASK},
	}}})
	accepted := h.waitAccepted(idle, id)
	rejected := h.waitKind(accepted, "bridge_error")
	if kind := h.events()[rejected].GetBridgeError().GetKind(); kind != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED {
		t.Fatalf("bridge_error %s", kind)
	}
	// The agent stays in bypassPermissions, so the session stays in
	// AUTO_APPROVE: a permission request is answered by the bridge.
	h.write(prompt("permission"))
	h.waitKind(rejected, "permission_response")
	for _, e := range h.events()[accepted:] {
		if e.GetModeChanged() != nil {
			t.Fatalf("mode_changed after a rejected switch: %v", e)
		}
	}
	if got := h.events()[h.waitKind(rejected, "permission_response")].GetPermissionResponse(); !got.GetAutoApproved() {
		t.Fatalf("permission_response %v", got)
	}
	h.waitState(h.waitKind(rejected, "turn_ended"), sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	h.checkLog()
}

func TestStartConfigModeAndOptions(t *testing.T) {
	h := newHarness(t, func(_ *harness, start *sandcrewv1.BridgeStartConfig) {
		start.ModeId = "plan"
		start.ConfigOptions = []*sandcrewv1.ConfigOptionSelection{
			{ConfigOptionId: "model", ValueId: "opus"},
			{ConfigOptionId: "model", ValueId: "unknown-model"},
		}
	})
	h.run()
	idle := h.waitIdle()
	got := kinds(h.events()[:idle+1])
	want := []string{"hello", "session_state", "agent_ready", "config_options", "mode_changed", "mode_changed", "config_options", "bridge_error", "session_state"}
	if !slices.Equal(got, want) {
		t.Fatalf("start events %v, want %v", got, want)
	}
	events := h.events()
	if events[5].GetModeChanged().GetCurrentModeId() != "plan" || events[6].GetConfigOptions().GetOptions()[0].GetCurrentValueId() != "opus" ||
		events[7].GetBridgeError().GetKind() != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_OPTION_REJECTED {
		t.Fatalf("start configuration events %v", events[5:8])
	}
}

func TestBridgeAnswersPermissionsWhenTheAgentHasNoModes(t *testing.T) {
	h := newHarness(t)
	h.agent.withoutModes = true
	h.run()
	idle := h.waitIdle()
	// Without session modes and without a MODE option, the bridge answers
	// permission requests itself.
	if mode := h.events()[idle-1].GetModeChanged(); mode.GetCurrentModeId() != "" || mode.GetPermissionMode() != sandcrewv1.PermissionMode_PERMISSION_MODE_AUTO_APPROVE {
		t.Fatalf("mode_changed %v", mode)
	}
	if _, _, setModes, _, _ := h.agent.snapshot(); len(setModes) != 0 {
		t.Fatalf("set_mode without modes: %v", setModes)
	}
}

func TestInputsAreDeliveredAtMostOnce(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()

	malformed, _ := uuidv7.NewString()
	h.writeRaw(malformed+".json", []byte("{not json"))
	h.waitRejected(idle, malformed, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED)

	large, _ := uuidv7.NewString()
	h.writeRaw(large+".json", bytes.Repeat([]byte(" "), 1<<20+1))
	h.waitRejected(idle, large, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED)

	unsupported := h.write(&sandcrewv1.BridgeInput{})
	h.waitRejected(idle, unsupported, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_UNSUPPORTED)

	mismatched, _ := uuidv7.NewString()
	other, _ := uuidv7.NewString()
	data, _ := protojson.Marshal(&sandcrewv1.BridgeInput{InputId: other, Input: &sandcrewv1.BridgeInput_Prompt{Prompt: &sandcrewv1.PromptInput{Text: "x"}}})
	h.writeRaw(mismatched+".json", data)
	h.waitRejected(idle, mismatched, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED)

	empty := h.write(prompt(""))
	h.waitRejected(idle, empty, sandcrewv1.InputRejectionReason_INPUT_REJECTION_REASON_MALFORMED)

	// Other names are ignored and stay.
	h.writeRaw("notes.txt", []byte("x"))
	h.writeRaw("not-a-uuid.json", []byte("{}"))

	input := prompt("once")
	id := h.write(input)
	end := h.waitKind(h.waitAccepted(idle, id), "turn_ended")
	h.waitState(end, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	before := len(h.events())
	// A repeated write of the same input is deleted without an event.
	h.write(input)
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(inboxFiles(t, h.stateDir), id+".json") {
		if time.Now().After(deadline) {
			t.Fatal("the repeated input was not deleted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if after := len(h.events()); after != before {
		t.Fatalf("a repeated input wrote %v", kinds(h.events()[before:]))
	}
	files := inboxFiles(t, h.stateDir)
	slices.Sort(files)
	if !slices.Equal(files, []string{"not-a-uuid.json", "notes.txt"}) {
		t.Fatalf("inbox files %v", files)
	}
	h.checkLog()
}

func TestInputsAreHandledInInputIDOrder(t *testing.T) {
	h := newHarness(t)
	// Write the files before the bridge starts, newest ID first.
	var ids []string
	for range 3 {
		id, _ := uuidv7.NewString()
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond)
	}
	for i := len(ids) - 1; i >= 0; i-- {
		in := prompt(fmt.Sprintf("p%d", i))
		in.InputId = ids[i]
		h.write(in)
	}
	h.run()
	h.waitIdle()
	for i := range ids {
		h.waitAccepted(0, ids[i])
	}
	var accepted []string
	for _, e := range h.events() {
		if id := e.GetInputAccepted().GetInputId(); id != "" {
			accepted = append(accepted, id)
		}
	}
	if !slices.Equal(accepted, ids) {
		t.Fatalf("accepted order %v, want %v", accepted, ids)
	}
}

func TestInboxNotificationsDeliverInputs(t *testing.T) {
	h := newHarness(t)
	// Without notifications the bridge would not read the inbox again
	// after its first read.
	h.cfg.PollInterval = time.Hour
	h.cfg.FallbackPollInterval = time.Hour
	h.run()
	idle := h.waitIdle()
	id := h.write(prompt("notified"))
	accepted := h.waitAccepted(idle, id)
	answer := h.waitFor(accepted, "answer", func(e *sandcrewv1.BridgeEvent) bool {
		return e.GetAgentMessageChunk().GetContent().GetText() == "echo: notified"
	})
	h.waitState(h.waitKind(answer, "turn_ended"), sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	h.checkLog()
}

func TestRestartContinuesTheLog(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	input := prompt("first")
	id := h.write(input)
	end := h.waitKind(h.waitAccepted(idle, id), "turn_ended")
	h.waitState(end, sandcrewv1.AgentActivity_AGENT_ACTIVITY_IDLE)
	if code := h.stop(); code != ExitAgentExited {
		t.Fatalf("exit code %d", code)
	}
	last := h.events()[len(h.events())-1]
	if last.GetAgentExited().GetSignal() != "terminated" {
		t.Fatalf("last event %v, want agent_exited", last)
	}
	h.checkLog()

	// A new bridge process on the same state directory.
	h2 := &harness{t: t, stateDir: h.stateDir, stdout: &syncBuffer{}, cfg: h.cfg}
	h2.agent = newFakeAgent(t)
	h2.cfg.Stdout = h2.stdout
	h2.cfg.StartAgent = func(_ context.Context, launch Launch) (agent.Agent, error) {
		return agent.NewACPAgent(h2.agent, agent.ACPOptions{Handler: launch.Handler}), nil
	}
	h2.write(input)
	h2.run()
	h2.waitIdle()
	first := h2.events()[0]
	if first.GetHello() == nil || first.GetSequence() != last.GetSequence()+1 {
		t.Fatalf("first event after restart %v, want hello with sequence %d", first, last.GetSequence()+1)
	}
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(inboxFiles(t, h.stateDir), id+".json") {
		if time.Now().After(deadline) {
			t.Fatal("the repeated input was not deleted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, e := range h2.events() {
		if e.GetInputAccepted() != nil || e.GetInputRejected() != nil {
			t.Fatalf("the restarted bridge handled a logged input again: %v", e)
		}
	}
	data, _ := os.ReadFile(filepath.Join(h.stateDir, EventsFileName))
	if !strings.HasSuffix(string(data), h2.stdout.String()) || !strings.HasPrefix(string(data), h.stdout.String()) {
		t.Fatal("the restarted bridge did not append to the log")
	}
}

func TestAgentExitEndsTheBridge(t *testing.T) {
	h := newHarness(t)
	h.run()
	idle := h.waitIdle()
	h.write(prompt("exit"))
	if code := h.wait(); code != ExitAgentExited {
		t.Fatalf("exit code %d", code)
	}
	exited := h.waitKind(idle, "agent_exited")
	if code := h.events()[exited].GetAgentExited().GetExitCode(); code != 7 {
		t.Fatalf("agent exit code %d", code)
	}
	if exited != len(h.events())-1 {
		t.Fatalf("agent_exited is not the last event: %v", kinds(h.events()))
	}
	h.checkLog()
}

func TestStartFailures(t *testing.T) {
	cases := []struct {
		name   string
		option harnessOption
		kind   sandcrewv1.BridgeErrorKind
	}{
		{"missing credential", func(h *harness, _ *sandcrewv1.BridgeStartConfig) {
			h.cfg.Environ = []string{"PATH=/usr/bin"}
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CREDENTIAL_MISSING},
		{"credential kind of another vendor", func(_ *harness, start *sandcrewv1.BridgeStartConfig) {
			start.CredentialKind = sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CREDENTIAL_MISSING},
		{"missing codex auth.json", func(_ *harness, start *sandcrewv1.BridgeStartConfig) {
			start.Vendor = sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX
			start.CredentialKind = sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CREDENTIAL_MISSING},
		{"checksum mismatch", func(h *harness, _ *sandcrewv1.BridgeStartConfig) {
			h.cfg.Install = func(context.Context, sandcrewv1.AgentVendor) (install.Result, error) {
				return install.Result{}, &install.Error{Kind: sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CHECKSUM_MISMATCH, Err: errors.New("digest differs")}
			}
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CHECKSUM_MISMATCH},
		{"download failure", func(h *harness, _ *sandcrewv1.BridgeStartConfig) {
			h.cfg.Install = func(context.Context, sandcrewv1.AgentVendor) (install.Result, error) {
				return install.Result{}, &install.Error{Kind: sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED, Err: errors.New("HTTP status 503")}
			}
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED},
		{"process start failure", func(h *harness, _ *sandcrewv1.BridgeStartConfig) {
			h.cfg.StartAgent = func(context.Context, Launch) (agent.Agent, error) { return nil, errors.New("exec format error") }
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED},
		{"session/new failure", func(h *harness, _ *sandcrewv1.BridgeStartConfig) {
			h.agent.failSessionNew = true
		}, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED},
		{"unreadable start.json", nil, sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_START_FAILED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var options []harnessOption
			if tc.option != nil {
				options = append(options, tc.option)
			}
			h := newHarness(t, options...)
			if tc.option == nil {
				if err := os.WriteFile(filepath.Join(h.stateDir, StartFileName), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.run()
			if code := h.wait(); code != ExitStartFailed {
				t.Fatalf("exit code %d, want %d", code, ExitStartFailed)
			}
			events := h.events()
			got := kinds(events)
			if !slices.Equal(got, []string{"hello", "session_state", "bridge_error"}) {
				t.Fatalf("events %v", got)
			}
			if k := events[2].GetBridgeError().GetKind(); k != tc.kind {
				t.Fatalf("bridge_error kind %s, want %s", k, tc.kind)
			}
			if strings.Contains(h.stdout.String(), testCredential) {
				t.Fatal("stdout contains the credential")
			}
			h.checkLog()
		})
	}
}

// failingFile fails every write after the first n lines.
type failingFile struct {
	mu     sync.Mutex
	file   *os.File
	writes int
	limit  int
}

func (f *failingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes >= f.limit {
		return 0, errors.New("disk full")
	}
	f.writes++
	return f.file.Write(p)
}
func (f *failingFile) Sync() error  { return f.file.Sync() }
func (f *failingFile) Close() error { return f.file.Close() }

func TestEventLogFailureStopsTheBridge(t *testing.T) {
	h := newHarness(t)
	var file *failingFile
	h.cfg.OpenEventLog = func(path string) (eventlog.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		file = &failingFile{file: f, limit: 1 << 30}
		return file, nil
	}
	h.run()
	idle := h.waitIdle()
	file.mu.Lock()
	file.limit = file.writes
	file.mu.Unlock()
	id := h.write(prompt("hello"))
	if code := h.wait(); code != ExitEventLogFailed {
		t.Fatalf("exit code %d, want %d", code, ExitEventLogFailed)
	}
	// The acceptance is not in the log, so the file stays for the next
	// bridge process.
	if files := inboxFiles(t, h.stateDir); !slices.Contains(files, id+".json") {
		t.Fatalf("inbox files %v, want the unlogged input %s", files, id)
	}
	events := h.events()
	last := events[len(events)-1]
	if last.GetBridgeError().GetKind() != sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_EVENT_LOG_WRITE_FAILED || last.GetSequence() != events[idle].GetSequence() {
		t.Fatalf("last event %v, want EVENT_LOG_WRITE_FAILED with the last logged sequence %d", last, events[idle].GetSequence())
	}
	select {
	case <-h.agent.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was not stopped")
	}
}

func codexAuthJSON(h *harness, start *sandcrewv1.BridgeStartConfig) {
	start.Vendor = sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX
	start.CredentialKind = sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON
	h.cfg.Environ = []string{"PATH=/usr/bin"}
	dir := filepath.Join(h.stateDir, "codex-home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":"one"}`), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func TestCodexAuthJSONChangesProduceCredentialUpdates(t *testing.T) {
	h := newHarness(t, codexAuthJSON)
	h.agent.modes = []string{"read-only", "workspace-write", "agent", "agent-full-access"}
	h.agent.currentMode = "agent"
	h.run()
	idle := h.waitIdle()
	if mode := h.events()[idle-1].GetModeChanged(); mode.GetCurrentModeId() != "agent-full-access" {
		t.Fatalf("codex auto-approve mode %v", mode)
	}
	launch := h.launches[0]
	if !slices.Contains(launch.Env, "CODEX_HOME="+filepath.Join(h.stateDir, "codex-home")) || launch.AuthMethodID != "" {
		t.Fatalf("codex launch %+v", launch)
	}
	authFile := filepath.Join(h.stateDir, "codex-home", "auth.json")
	update := func(content string, revision uint64) {
		t.Helper()
		if err := os.WriteFile(authFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		h.waitFor(idle, fmt.Sprintf("credential_update %d", revision), func(e *sandcrewv1.BridgeEvent) bool {
			return e.GetCredentialUpdate().GetRevision() == revision
		})
	}
	update(`{"tokens":"two"}`, 1)
	// The same content with a new modification time is no change.
	now := time.Now().Add(time.Minute)
	if err := os.Chtimes(authFile, now, now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	update(`{"tokens":"three, longer"}`, 2)
	var updates []*sandcrewv1.BridgeEvent
	for _, e := range h.events() {
		if e.GetCredentialUpdate() != nil {
			updates = append(updates, e)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("credential updates %v, want 2", updates)
	}
	for _, e := range updates {
		if e.GetCredentialUpdate().GetKind() != sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_CODEX_AUTH_JSON || e.GetSequence() != h.events()[idle].GetSequence() {
			t.Fatalf("credential_update %v", e)
		}
	}
	if strings.Contains(h.stdout.String(), "tokens") {
		t.Fatal("stdout contains auth.json content")
	}
	h.checkLog()
}

func TestCodexAuthJSONRefreshDuringStartIsReported(t *testing.T) {
	h := newHarness(t, codexAuthJSON)
	// The interval check does not run in this test.
	h.cfg.CredentialInterval = time.Hour
	authFile := filepath.Join(h.stateDir, "codex-home", "auth.json")
	h.agent.onSessionNew = func() {
		if err := os.WriteFile(authFile, []byte(`{"tokens":"refreshed"}`), 0o600); err != nil {
			t.Error(err)
		}
	}
	h.run()
	h.waitIdle()
	h.waitFor(0, "credential_update 1", func(e *sandcrewv1.BridgeEvent) bool {
		return e.GetCredentialUpdate().GetRevision() == 1
	})
	h.checkLog()
}

func TestCodexAPIKeyAuthenticates(t *testing.T) {
	h := newHarness(t, func(h *harness, start *sandcrewv1.BridgeStartConfig) {
		start.Vendor = sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX
		start.CredentialKind = sandcrewv1.VendorCredentialKind_VENDOR_CREDENTIAL_KIND_API_KEY
	})
	h.run()
	h.waitIdle()
	_, authIDs, _, _, _ := h.agent.snapshot()
	if !slices.Equal(authIDs, []string{"api-key"}) {
		t.Fatalf("authenticate calls %v", authIDs)
	}
	if !slices.Contains(h.launches[0].Env, "CODEX_API_KEY="+testCredential) {
		t.Fatalf("codex environment %v", h.launches[0].Env)
	}
}

// Every BridgeEvent case except credential_update reaches the log in some
// test of this file; this test keeps the list of cases in sync with the
// schema, so a new case needs a mapping and a test.
func TestEveryEventCaseIsKnown(t *testing.T) {
	known := []string{
		"hello", "input_accepted", "input_rejected", "user_message_chunk", "agent_message_chunk", "agent_thought_chunk",
		"tool_call", "tool_call_update", "plan", "permission_request", "permission_response", "turn_ended", "mode_changed",
		"config_options", "session_state", "bridge_error", "agent_exited", "credential_update", "unknown_update", "agent_ready",
	}
	fields := (&sandcrewv1.BridgeEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event").Fields()
	var schema []string
	for i := 0; i < fields.Len(); i++ {
		schema = append(schema, string(fields.Get(i).Name()))
	}
	slices.Sort(schema)
	slices.Sort(known)
	if !slices.Equal(schema, known) {
		t.Fatalf("event cases %v, known %v", schema, known)
	}
}
