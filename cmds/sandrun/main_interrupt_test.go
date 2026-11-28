// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// Run main in a subprocess so assertions can use the actual exit status.
const runSandRunMain = "SANDRUN_TEST_RUN_MAIN"

const (
	acknowledgeInterrupt      = "acknowledge-interrupt"
	consumeSecondInterrupt    = "consume-second-interrupt"
	interruptAcknowledgmentFD = 3
)

func TestMain(m *testing.M) {
	mode := os.Getenv(runSandRunMain)
	switch mode {
	case "1":
		main()
	case acknowledgeInterrupt, consumeSecondInterrupt:
		ack := os.NewFile(interruptAcknowledgmentFD, "interrupt acknowledgment")
		os.Exit(runSandRun(func() {
			defer ack.Close()
			if mode == consumeSecondInterrupt {
				// Catch the second signal before acknowledging the first. Restore
				// default handling again so only a third signal would end the CLI.
				interrupts := make(chan os.Signal, 1)
				signal.Notify(interrupts, os.Interrupt)
				writeInterruptAcknowledgment(ack)
				<-interrupts
				signal.Stop(interrupts)
			}
			writeInterruptAcknowledgment(ack)
		}))
	default:
		os.Exit(m.Run())
	}
}

func writeInterruptAcknowledgment(ack *os.File) {
	if _, err := ack.Write([]byte{1}); err != nil {
		fmt.Fprintln(os.Stderr, "write interrupt acknowledgment:", err)
		os.Exit(1)
	}
}

func sandRunCommand(t *testing.T, serverURL string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		runSandRunMain+"=1",
		"SANDRUN_API_URL="+serverURL,
		"SANDRUN_API_KEY="+sandruntest.DefaultAPIKey,
		"SANDRUN_CONFIG_DIR="+t.TempDir(),
		"NO_COLOR=1",
	)
	return cmd
}

func startSandRun(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sandrun: %v", err)
	}
	ended := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(ended)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})
	return ended
}

func TestFirstInterruptCancelsMCPTransfer(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sandrunv1connect.DevSessionServiceGetDevSessionProcedure {
			http.NotFound(w, r)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-t.Context().Done():
		}
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	cmd := sandRunCommand(t, srv.URL, "mcp", "--org", "fixture-org")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	ended := startSandRun(t, cmd)
	_, err = fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"download","arguments":{"session_id":"fixture-session","guest_path":"file","local_path":"fixture","wait_seconds":4}}}`)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ended:
		t.Fatalf("SandRun exited before starting the transfer: %v", cmd.ProcessState)
	case <-time.After(10 * time.Second):
		t.Fatal("SandRun did not start the transfer")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	// Keep stdin open and require cancellation and exit before the tool timer.
	deadline, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	select {
	case <-canceled:
	case <-deadline.Done():
		t.Fatal("the transfer RPC was not canceled after SIGINT")
	}
	select {
	case <-ended:
		if !cmd.ProcessState.Success() {
			t.Fatalf("SandRun exited with %v, want exit 0", cmd.ProcessState)
		}
	case <-deadline.Done():
		t.Fatal("SandRun did not exit after SIGINT")
	}
	var initialized struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.SplitN(stdout.Bytes(), []byte("\n"), 2)[0], &initialized); err != nil ||
		initialized.JSONRPC != "2.0" || initialized.ID != 1 || initialized.Result.ProtocolVersion == "" {
		t.Fatalf("MCP stdout = %q, want a JSON-RPC initialize result: %v", stdout.String(), err)
	}
}

func TestFirstInterruptPrintsExecAttachCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "explicit JSON", output: "json"},
		{name: "explicit text", output: "text"},
		{name: "environment defaults"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testExecAttachCommand(t, tc.output)
		})
	}
}

func testExecAttachCommand(t *testing.T, output string) {
	t.Helper()
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("exec-org").GetOrganizationId()
	workspace := t.TempDir()
	profile := srv.AddDevSessionProfile(org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, workspace)
	client, err := sandrun.New(sandrun.Options{BaseURL: srv.URL, APIKey: srv.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.DevSessions().CreateDevSession(t.Context(), connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	session := res.Msg.GetDevSession().GetDevSessionId()

	sandRunPath := filepath.Join(t.TempDir(), "sandrun")
	if err := os.Symlink(os.Args[0], sandRunPath); err != nil {
		t.Fatal(err)
	}
	fallbackURL, fallbackOrg := srv.URL, org
	args := []string{"session", "exec", session}
	hintOptions := ""
	var fallbackCalls atomic.Int32
	if output != "" {
		fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fallbackCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(fallback.Close)
		fallbackURL = fallback.URL
		fallbackOrg = srv.AddOrganization("fallback-org").GetOrganizationId()
		// Read options in both parse stages and keep the last output value.
		args = []string{"--api-url", srv.URL, "--output", "table", "session", "exec", session, "--org", org, "--output", output}
		hintOptions = fmt.Sprintf(" --org=%s --api-url=%s --output=%s", org, srv.URL, output)
	}
	const guestSecret = "guest-fixture-secret"
	const guest = `
		printf 'one\n\342\202'
		printf 'err1\n' >&2
		while [ ! -f stage2 ]; do sleep 0.02; done
		printf '\254two\n\342\202'
		printf 'err2\n' >&2
		while [ ! -f stage3 ]; do sleep 0.02; done
		printf '\254done\n'
		printf 'last\n' >&2
	`
	args = append(args, "--env", "FIXTURE_SECRET="+guestSecret, "--", "sh", "-c", guest)
	cmd := sandRunCommand(t, fallbackURL, args...)
	cmd.Path = sandRunPath
	cmd.Env = append(cmd.Env, "SANDRUN_ORG="+fallbackOrg, "PATH="+filepath.Dir(sandRunPath)+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	resume := func(hint string) *exec.Cmd {
		// Replace sh so SIGINT is sent to SandRun itself.
		attach := exec.CommandContext(ctx, "sh", "-c", "exec "+hint)
		attach.Env = cmd.Env
		return attach
	}

	stdoutOne, stderrOne := "one\n\xe2\x82", "err1\n"
	stdoutTwo, stderrTwo := "\xactwo\n\xe2\x82", "err2\n"
	firstStdoutOffset, secondStdoutOffset := 6, 13
	if output == "json" {
		stdoutOne, stderrOne = `"data":"one\n"`, `"data":"err1\n"`
		stdoutTwo, stderrTwo = `"data":"€two\n"`, `"data":"err2\n"`
		firstStdoutOffset, secondStdoutOffset = 4, 11
	}
	first := interruptAfterExecOutput(t, cmd, output == "json", stdoutOne, stderrOne)
	reqs := srv.ExecRequests()
	if len(reqs) != 1 {
		t.Fatalf("got %d exec requests, want 1", len(reqs))
	}
	checkHint := func(hint string, stdoutOffset, stderrOffset int) {
		t.Helper()
		if strings.Contains(hint, srv.APIKey) || strings.Contains(hint, guestSecret) {
			t.Fatal("the resume command contains a credential")
		}
		want := fmt.Sprintf("sandrun session attach %s %s%s --stdout-offset %d --stderr-offset %d",
			session, reqs[0].GetExecId(), hintOptions, stdoutOffset, stderrOffset)
		if hint != want {
			t.Fatalf("resume command = %q, want %q", hint, want)
		}
	}
	checkHint(first, firstStdoutOffset, 5)
	if err := os.WriteFile(filepath.Join(workspace, "stage2"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second := interruptAfterExecOutput(t, resume(first), output == "json", stdoutTwo, stderrTwo)
	checkHint(second, secondStdoutOffset, 10)
	if err := os.WriteFile(filepath.Join(workspace, "stage3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	final := resume(second)
	var stderr bytes.Buffer
	final.Stderr = &stderr
	stdout, err := final.Output()
	if err != nil {
		t.Fatalf("complete resumed attach: %v, stderr %q", err, stderr.String())
	}
	if output == "json" {
		if !strings.Contains(string(stdout), `"data":"€done\n"`) || !strings.Contains(string(stdout), `"data":"last\n"`) ||
			strings.Contains(string(stdout), "two") || strings.Contains(string(stdout), "err2") || stderr.Len() != 0 {
			t.Fatalf("resumed JSON stdout = %q, stderr = %q", stdout, stderr.String())
		}
	} else if string(stdout) != "\xacdone\n" || stderr.String() != "last\n" {
		t.Fatalf("resumed text stdout = %q, stderr = %q", stdout, stderr.String())
	}
	if calls := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); calls != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf("API calls: attach = %d, fallback = %d, want 2 and 0", calls, fallbackCalls.Load())
	}
}

func interruptAfterExecOutput(t *testing.T, cmd *exec.Cmd, jsonMode bool, stdoutText, stderrText string) string {
	t.Helper()
	stdout := newExecOutput(stdoutText)
	stderr := newExecOutput(stderrText)
	if jsonMode {
		stdout = newExecOutput(stdoutText, stderrText)
		stderr = newExecOutput()
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	ended := startSandRun(t, cmd)
	wait := func(ready <-chan struct{}) {
		t.Helper()
		select {
		case <-ready:
		case <-ended:
			t.Fatalf("SandRun exited before output: stdout %q, stderr %q", stdout.buf.String(), stderr.buf.String())
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for exec output")
		}
	}
	wait(stdout.ready)
	if !jsonMode {
		wait(stderr.ready)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
		if code := cmd.ProcessState.ExitCode(); code != 130 {
			t.Fatalf("SandRun exited with %d, want 130. Stderr: %s", code, stderr.buf.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SandRun did not exit after SIGINT")
	}
	const detached = "interrupted. Detached from the output stream after the command started. Continue with: "
	_, hint, ok := strings.Cut(stderr.buf.String(), detached)
	if !ok {
		t.Fatalf("missing detachment diagnostic or resume command on stderr: %s", stderr.buf.String())
	}
	return strings.TrimSuffix(hint, "\n")
}

type execOutput struct {
	buf   bytes.Buffer
	want  []string
	ready chan struct{}
}

func newExecOutput(text ...string) *execOutput {
	return &execOutput{want: text, ready: make(chan struct{})}
}

func (o *execOutput) Write(p []byte) (int, error) {
	n, err := o.buf.Write(p)
	for _, text := range o.want {
		if !strings.Contains(o.buf.String(), text) {
			return n, err
		}
	}
	if len(o.want) != 0 {
		close(o.ready)
		o.want = nil
	}
	return n, err
}
