// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/tide"
	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

type harness struct {
	t       *testing.T
	srv     *tidetest.Server
	org     string
	session *mcp.ClientSession
	server  *Server
	ws      string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	srv := tidetest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("test").GetOrganizationId()
	ws := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.AddDevSessionProfile(org, "linux", tidev1.GuestOs_GUEST_OS_LINUX, ws)
	if cfg.Client == nil {
		cfg.Client = func() (*tide.Client, error) { return tide.New(tide.Options{BaseURL: srv.URL, APIKey: srv.APIKey}) }
	}
	cfg.OrganizationID = org
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	s := New(cfg)
	t.Cleanup(s.Close)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &harness{t: t, srv: srv, org: org, session: cs, server: s, ws: ws}
}

// call calls a tool and decodes its structured result into out. It
// returns the text content and whether the result is an error.
func (h *harness) call(name string, args map[string]any, out any) (string, bool) {
	h.t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if out != nil && !res.IsError {
		reflect.ValueOf(out).Elem().SetZero()
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("%s: decode %s: %v", name, b, err)
		}
	}
	return text.String(), res.IsError
}

func (h *harness) running() string {
	h.t.Helper()
	var r sessionResult
	if text, isErr := h.call("create_session", map[string]any{"profile": "linux"}, &r); isErr {
		h.t.Fatalf("create_session: %s", text)
	}
	return r.Session.SessionID
}

func needPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
}

func TestToolList(t *testing.T) {
	h := newHarness(t, Config{})
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	schemas := map[string]map[string]any{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		schemas[tool.Name] = m
	}
	slices.Sort(names)
	want := []string{"create_session", "delete_session", "download", "exec", "exec_output", "get_session", "list_profiles", "list_sessions", "sync", "transfer_status", "wait_session"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}
	required := func(tool string) []string {
		var out []string
		for _, r := range schemas[tool]["required"].([]any) {
			out = append(out, r.(string))
		}
		slices.Sort(out)
		return out
	}
	for tool, want := range map[string][]string{
		"create_session": {"profile"}, "exec": {"command", "session_id"}, "exec_output": {"exec_id", "session_id", "stderr_offset", "stdout_offset"},
		"download": {"guest_path", "local_path", "session_id"}, "transfer_status": {"transfer_id"}, "sync": {"session_id"},
	} {
		if got := required(tool); !slices.Equal(got, want) {
			t.Errorf("%s required = %v, want %v", tool, got, want)
		}
	}
	props := schemas["exec"]["properties"].(map[string]any)
	for _, p := range []string{"cwd", "env", "timeout_seconds", "wait_seconds"} {
		if _, ok := props[p]; !ok {
			t.Errorf("exec has no %s argument", p)
		}
	}
}

func TestWaitLimit(t *testing.T) {
	h := newHarness(t, Config{})
	id := h.running()
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": 51}, nil); !isErr || !strings.HasPrefix(text, "invalid_argument") {
		t.Fatalf("wait_seconds 51 = %q %v", text, isErr)
	}
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": -1}, nil); !isErr {
		t.Fatalf("wait_seconds -1 accepted: %q", text)
	}
	var r sessionResult
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": 50}, &r); isErr || r.Session.State != "running" {
		t.Fatalf("wait_seconds 50 = %q %+v", text, r)
	}

	// A queued session returns its state when the wait passes.
	h.srv.NewSessionState = tidev1.RequestState_REQUEST_STATE_QUEUED
	queued := h.running()
	start := time.Now()
	if _, isErr := h.call("wait_session", map[string]any{"session_id": queued, "wait_seconds": 1}, &r); isErr || r.Session.State != "queued" {
		t.Fatalf("queued wait = %+v", r)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait took longer than wait_seconds")
	}
	if _, isErr := h.call("wait_session", map[string]any{"session_id": queued, "wait_seconds": 0}, &r); isErr || r.Session.State != "queued" {
		t.Fatalf("zero wait = %+v", r)
	}
	if waitDur, err := h.server.waitDuration(nil); err != nil || waitDur != MaxWait {
		t.Fatalf("default wait = %v %v", waitDur, err)
	}
}

func TestExecWithinAndBeyondWait(t *testing.T) {
	needPOSIX(t)
	h := newHarness(t, Config{})
	id := h.running()

	var r execResult
	text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"sh", "-c", "echo hello; echo oops >&2; exit 4"}}, &r)
	if isErr || r.Running || r.ExitCode == nil || *r.ExitCode != 4 || r.Stdout.Text != "hello\n" || r.Stderr.Text != "oops\n" {
		t.Fatalf("short exec = %q %+v", text, r)
	}
	if r.NextStdoutOffset != 6 || r.Stdout.Omitted != nil {
		t.Fatalf("offsets %+v", r)
	}

	// A command that runs longer than the wait keeps running.
	text, isErr = h.call("exec", map[string]any{"session_id": id, "command": []string{"sh", "-c", "echo begin; sleep 2; echo finish"}, "wait_seconds": 0}, &r)
	if isErr || !r.Running || r.ExecID == "" || r.ExitCode != nil {
		t.Fatalf("long exec = %q %+v", text, r)
	}
	var out execResult
	var all strings.Builder
	all.WriteString(r.Stdout.Text)
	so, se := r.NextStdoutOffset, r.NextStderrOffset
	for range 10 {
		text, isErr = h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": so, "stderr_offset": se, "wait_seconds": 5}, &out)
		if isErr {
			t.Fatalf("exec_output: %s", text)
		}
		all.WriteString(out.Stdout.Text)
		so, se = out.NextStdoutOffset, out.NextStderrOffset
		if !out.Running {
			break
		}
	}
	if out.Running || out.ExitCode == nil || *out.ExitCode != 0 || all.String() != "begin\nfinish\n" {
		t.Fatalf("exec_output end = %+v, output %q", out, all.String())
	}

	// A timeout stops the command.
	if _, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"sleep", "10"}, "timeout_seconds": 1}, &r); isErr || !r.TimedOut {
		t.Fatalf("timeout exec = %+v", r)
	}
}

func TestExecOutputWindowAndOffsets(t *testing.T) {
	needPOSIX(t)
	h := newHarness(t, Config{})
	id := h.running()
	// 100000 bytes of stdout: the result keeps 8 KiB + 24 KiB and states
	// the omitted range.
	cmd := []string{"sh", "-c", "i=0; while [ $i -lt 1000 ]; do printf '%099d\\n' $i; i=$((i+1)); done"}
	var r execResult
	text, isErr := h.call("exec", map[string]any{"session_id": id, "command": cmd}, &r)
	if isErr || r.ExitCode == nil {
		t.Fatalf("exec = %q", text)
	}
	if r.Stdout.To != 100000 || r.Stdout.Omitted == nil {
		t.Fatalf("window = from %d to %d omitted %v", r.Stdout.From, r.Stdout.To, r.Stdout.Omitted)
	}
	if *r.Stdout.Omitted != (tide.ByteRange{From: headBytes, To: 100000 - tailBytes}) {
		t.Fatalf("omitted = %+v", *r.Stdout.Omitted)
	}
	if !strings.HasPrefix(r.Stdout.Text, strings.Repeat("0", 99)+"\n") || !strings.HasSuffix(r.Stdout.Text, "000000999\n") {
		t.Fatal("window text lacks the head or the tail")
	}
	// The omitted range can be read again from its offset.
	var out execResult
	if text, isErr := h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": r.Stdout.Omitted.From, "stderr_offset": 0}, &out); isErr {
		t.Fatalf("exec_output: %s", text)
	}
	if out.Stdout.From != headBytes || out.Stdout.To != 100000 || out.ExitCode == nil {
		t.Fatalf("exec_output from offset = %+v", out.Stdout)
	}
	if out.Stdout.Omitted == nil || out.Stdout.Omitted.From != 2*headBytes {
		t.Fatalf("exec_output window %+v", out.Stdout.Omitted)
	}

	// Output that the guest no longer keeps is reported.
	h.srv.OutputKeepBytes = 100
	if _, isErr := h.call("exec", map[string]any{"session_id": id, "command": cmd}, &r); isErr {
		t.Fatal("exec")
	}
	if text, isErr := h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": 0, "stderr_offset": 0}, &out); isErr || len(out.OutputTruncated) == 0 {
		t.Fatalf("truncated = %q %+v", text, out.OutputTruncated)
	}
	if m := out.OutputTruncated[0].MissingStdout; m == nil || m.From != 0 || m.To != 100000-100 {
		t.Fatalf("missing = %+v", m)
	}
}

func TestPassEnvValuesNeverInResults(t *testing.T) {
	needPOSIX(t)
	const secret = "s3cr3t-value-for-test"
	env := map[string]string{"NPM_TOKEN": secret}
	h := newHarness(t, Config{
		PassEnv:   []string{"NPM_TOKEN", "MISSING_TOKEN"},
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	id := h.running()
	var r execResult
	text, isErr := h.call("exec", map[string]any{
		"session_id": id,
		"command":    []string{"sh", "-c", `test "$NPM_TOKEN" = "` + secret + `" && echo ok; test -z "$MISSING_TOKEN" && echo unset`},
		"env":        map[string]string{"NPM_TOKEN": "agent-value"},
	}, &r)
	if isErr || r.Stdout.Text != "ok\nunset\n" {
		t.Fatalf("exec = %q %+v", text, r)
	}
	if strings.Contains(text, secret) {
		t.Fatal("the --pass-env value appears in the tool result")
	}
	if !slices.Equal(r.MissingEnv, []string{"MISSING_TOKEN"}) {
		t.Fatalf("missing_env = %v", r.MissingEnv)
	}
	reqs := h.srv.ExecRequests()
	if reqs[len(reqs)-1].GetEnv()["NPM_TOKEN"] != secret {
		t.Fatal("the --pass-env value did not reach the guest")
	}
	// A tool call cannot read the value back through the output either
	// except by printing it itself; the server adds nothing of it.
	res, _ := h.session.ListTools(context.Background(), nil)
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), secret) {
		t.Fatal("the --pass-env value appears in the tool list")
	}
}

func TestSyncAndDownloadThroughTransferStatus(t *testing.T) {
	needPOSIX(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "main.c"), []byte("int main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{WorkDir: repo})
	id := h.running()

	var r transferResult
	text, isErr := h.call("sync", map[string]any{"session_id": id, "wait_seconds": 0}, &r)
	if isErr || r.TransferID == "" || r.Kind != "sync" {
		t.Fatalf("sync = %q %+v", text, r)
	}
	if r.Running && r.Progress == nil {
		t.Fatal("a running transfer has no progress")
	}
	if text, isErr := h.call("transfer_status", map[string]any{"transfer_id": r.TransferID}, &r); isErr || r.Running || r.Sync == nil || r.Sync.Uploaded != 1 {
		t.Fatalf("transfer_status = %q %+v", text, r)
	}
	if b, err := os.ReadFile(filepath.Join(h.ws, "main.c")); err != nil || string(b) != "int main(){}" {
		t.Fatal("the sync did not copy the file")
	}

	if err := os.WriteFile(filepath.Join(h.ws, "a.out"), []byte("built"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, isErr = h.call("download", map[string]any{"session_id": id, "guest_path": "a.out", "local_path": "out/a.out", "wait_seconds": 0}, &r)
	if isErr {
		t.Fatalf("download = %q", text)
	}
	if r.Running {
		if text, isErr = h.call("transfer_status", map[string]any{"transfer_id": r.TransferID}, &r); isErr {
			t.Fatalf("transfer_status = %q", text)
		}
	}
	if r.Download == nil || r.Download.Files != 1 || r.Download.LocalPath != filepath.Join(repo, "out/a.out") {
		t.Fatalf("download result %+v", r)
	}
	if text, isErr := h.call("transfer_status", map[string]any{"transfer_id": "missing"}, nil); !isErr || !strings.HasPrefix(text, "not_found") {
		t.Fatalf("unknown transfer = %q", text)
	}

	// A failed transfer reports its error with the Connect code.
	h.srv.SetSessionState(id, tidev1.RequestState_REQUEST_STATE_STARTING)
	if text, isErr := h.call("sync", map[string]any{"session_id": id}, nil); !isErr || !strings.HasPrefix(text, "failed_precondition") {
		t.Fatalf("sync of a starting session = %q", text)
	}
}

func TestErrorMapping(t *testing.T) {
	h := newHarness(t, Config{})
	if text, isErr := h.call("create_session", map[string]any{"profile": "missing"}, nil); !isErr || !strings.HasPrefix(text, "not_found") {
		t.Fatalf("missing profile = %q", text)
	}
	// The per-user dev session limit.
	ctx := context.Background()
	c, _ := tide.New(tide.Options{BaseURL: h.srv.URL, APIKey: h.srv.AccessToken})
	one := int32(1)
	if _, err := c.Organizations().UpdateOrganizationSettings(ctx, connect.NewRequest(&tidev1.UpdateOrganizationSettingsRequest{
		OrganizationId: h.org, Settings: &tidev1.OrganizationSettings{MaxDevSessionsPerUser: &one}, UpdateMask: fieldMask("max_dev_sessions_per_user"),
	})); err != nil {
		t.Fatal(err)
	}
	id := h.running()
	if text, isErr := h.call("create_session", map[string]any{"profile": "linux"}, nil); !isErr || !strings.HasPrefix(text, "resource_exhausted") {
		t.Fatalf("limit = %q", text)
	}
	h.srv.SetSessionState(id, tidev1.RequestState_REQUEST_STATE_QUEUED)
	if text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"true"}}, nil); !isErr || !strings.HasPrefix(text, "failed_precondition") {
		t.Fatalf("exec in a queued session = %q", text)
	}
	var d sessionResult
	if text, isErr := h.call("delete_session", map[string]any{"session_id": id}, &d); isErr || d.Session.State != "canceled" || d.Session.EndReason != "canceled_by_user" {
		t.Fatalf("delete = %q %+v", text, d)
	}
	var l listSessionsResult
	if _, isErr := h.call("list_sessions", map[string]any{"include_ended": true}, &l); isErr || len(l.Sessions) != 1 {
		t.Fatalf("list = %+v", l)
	}
	if text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{}}, nil); !isErr || !strings.HasPrefix(text, "invalid_argument") {
		t.Fatalf("empty command = %q", text)
	}

	// Without a credential, every tool tells the user what to do.
	noCred := newHarness(t, Config{Client: func() (*tide.Client, error) {
		return nil, errors.New("not signed in: run 'tide login' or set TIDE_API_KEY")
	}})
	for _, tool := range []string{"list_profiles", "list_sessions"} {
		if text, isErr := noCred.call(tool, map[string]any{}, nil); !isErr || !strings.Contains(text, "tide login") || !strings.Contains(text, "TIDE_API_KEY") {
			t.Fatalf("%s without credential = %q", tool, text)
		}
	}
	var p listProfilesResult
	if _, isErr := h.call("list_profiles", map[string]any{}, &p); isErr || len(p.Profiles) != 1 || p.Profiles[0].GuestOS != "linux" || p.Profiles[0].MaxQueueWait != "30m0s" {
		t.Fatalf("profiles = %+v", p)
	}
}

func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
