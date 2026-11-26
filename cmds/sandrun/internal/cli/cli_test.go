// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type fixture struct {
	t          *testing.T
	srv        *sandruntest.Server
	org        string
	cfg        string
	env        map[string]string
	wd         string
	httpClient *http.Client
}

// newFixture starts a fake sandrun-api and stores Logto tokens that the fake
// accepts, so commands run without a Logto tenant.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("acme").GetOrganizationId()
	cfg := t.TempDir()
	st := credstore.Store{Dir: cfg, Program: "sandrun"}
	if err := st.Save(&credstore.Credentials{
		LogtoEndpoint: "https://logto.example.com", AppID: "app", Resource: DefaultLogtoResource,
		RefreshToken: "refresh", AccessToken: srv.AccessToken, AccessTokenExpiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, srv: srv, org: org, cfg: cfg, wd: t.TempDir(), env: map[string]string{
		EnvAPIURL: srv.URL, EnvConfigDir: cfg, EnvNoColor: "1",
	}}
}

type result struct {
	code           int
	stdout, stderr string
}

type execJSONLine struct {
	Event  string `json:"event"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

func parseExecJSONLines(t *testing.T, output string) []execJSONLine {
	t.Helper()
	var got []execJSONLine
	for line := range strings.SplitSeq(strings.TrimSuffix(output, "\n"), "\n") {
		var event execJSONLine
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode exec event %q: %v", line, err)
		}
		got = append(got, event)
	}
	return got
}

func checkExecJSONLines(t *testing.T, output string, want ...execJSONLine) {
	t.Helper()
	got := parseExecJSONLines(t, output)
	if !slices.Equal(got, want) {
		t.Fatalf("exec events = %+v, want %+v", got, want)
	}
}

func TestExecJSONDecodesUTF8AcrossChunks(t *testing.T) {
	stdout, stderr := sandrun.ExecEventStdout, sandrun.ExecEventStderr
	var output bytes.Buffer
	w := execWriter{a: &app{fmt: FormatJSON, env: Env{Stdout: &output}}}
	for _, ev := range []sandrun.ExecEvent{
		{Kind: stdout, Offset: 0, Data: []byte("h\xc3")},
		{Kind: stderr, Offset: 0, Data: []byte("\xe2")},
		{Kind: stdout, Offset: 2, Data: []byte("\xa9 \xe2\x82")},
		{Kind: stderr, Offset: 1, Data: []byte("\x82\xac")},
		{Kind: stdout, Offset: 6, Data: []byte("\xac")},
		{Kind: stdout, Offset: 7, Data: []byte("\xffa\xe2\x82")},
		{Kind: stderr, Offset: 3, Data: []byte("\xc3")},
		{Kind: sandrun.ExecEventOutputTruncated, MissingStdout: sandrun.ByteRange{From: 11, To: 13}},
		{Kind: stderr, Offset: 4, Data: []byte("\xa9")},
		{Kind: stdout, Offset: 13, Data: []byte("\xacz\xf0")},
		{Kind: stdout, Offset: 17, Data: []byte("b")},
	} {
		ev.ExecID = "exec"
		if err := w.event(ev); err != nil {
			t.Fatal(err)
		}
	}
	checkExecJSONLines(t, output.String(),
		execJSONLine{Event: "stdout", Offset: 0, Data: "h"},
		execJSONLine{Event: "stdout", Offset: 1, Data: "é "},
		execJSONLine{Event: "stderr", Offset: 0, Data: "€"},
		execJSONLine{Event: "stdout", Offset: 4, Data: "€"},
		execJSONLine{Event: "stdout", Offset: 7, Data: "�a"},
		execJSONLine{Event: "stdout", Offset: 9, Data: "�"},
		execJSONLine{Event: "output_truncated"},
		execJSONLine{Event: "stderr", Offset: 3, Data: "é"},
		execJSONLine{Event: "stdout", Offset: 13, Data: "�z"},
		execJSONLine{Event: "stdout", Offset: 15, Data: "�"},
		execJSONLine{Event: "stdout", Offset: 17, Data: "b"},
	)
}

func TestExecJSONFlushesUTF8BeforeResult(t *testing.T) {
	for _, tc := range []struct {
		event string
		err   error
	}{
		{event: "exited"},
		{event: "stream_error", err: &sandrun.ExecStreamError{
			ExecID: "exec", StdoutOffset: 2, StderrOffset: 2, Err: errors.New("stream broke"),
		}},
	} {
		t.Run(tc.event, func(t *testing.T) {
			var output bytes.Buffer
			a := &app{fmt: FormatJSON, env: Env{Stdout: &output}, logger: slog.Default()}
			w := execWriter{a: a}
			for _, ev := range []sandrun.ExecEvent{
				{Kind: sandrun.ExecEventStdout, ExecID: "exec", Data: []byte("\xe2\x82")},
				{Kind: sandrun.ExecEventStderr, ExecID: "exec", Data: []byte("x\xe2")},
			} {
				if err := w.event(ev); err != nil {
					t.Fatal(err)
				}
			}
			res := &sandrun.ExecResult{Kind: sandrun.ExecResultExited, ExecID: "exec", StdoutOffset: 2, StderrOffset: 2}
			err := a.finishExec(context.Background(), "session", &w, res, tc.err)
			if tc.err != nil {
				var exit *exitError
				if !errors.As(err, &exit) || exit.code != ExitDataPath {
					t.Fatalf("finishExec = %v, want exit %d", err, ExitDataPath)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			checkExecJSONLines(t, output.String(),
				execJSONLine{Event: "stderr", Offset: 0, Data: "x"},
				execJSONLine{Event: "stdout", Offset: 0, Data: "�"},
				execJSONLine{Event: "stderr", Offset: 1, Data: "�"},
				execJSONLine{Event: tc.event},
			)
		})
	}
}

type failingExecOutput struct {
	bytes.Buffer
	err    error
	failAt int
	writes int
}

func (w *failingExecOutput) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return w.Buffer.Write(p)
}

func TestSessionExecAndAttachOutputWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	f := newFixture(t)
	f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, f.wd)
	f.env[EnvAPIKey] = f.srv.APIKey
	f.env[EnvOrganization] = f.org
	var session struct {
		ID string `json:"dev_session_id"`
	}
	if err := json.Unmarshal([]byte(f.ok("", "--output", "json", "session", "create", "linux").stdout), &session); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"exec", "attach"} {
		for _, tc := range []struct {
			name, format string
			failStderr   bool
		}{
			{name: "stdout", format: "text"},
			{name: "stderr", format: "text", failStderr: true},
			{name: "json", format: "json"},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				execArgs := []string{"session", "exec", session.ID, "--", "sh", "-c", "printf out; printf err >&2"}
				args := execArgs
				if command == "attach" {
					f.ok("", execArgs...)
					requests := f.srv.ExecRequests()
					args = []string{"session", "attach", session.ID, requests[len(requests)-1].GetExecId()}
				}

				var stdout, stderr failingExecOutput
				failed := &stdout
				failAt := 1
				if tc.failStderr {
					failed = &stderr
				}
				if tc.format == "json" && command == "exec" {
					failAt = 2 // The first line reports the start event.
				}
				failed.failAt = failAt
				failed.err = errors.New("local output write failed")
				code := Run(t.Context(), Env{
					Args:      append([]string{"--output", tc.format}, args...),
					Stdout:    &stdout,
					Stderr:    &stderr,
					LookupEnv: func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
				})
				if code != ExitError || !strings.HasSuffix(stderr.String(), "error: local output write failed\n") {
					t.Errorf("exit = %d, stderr = %q, want exit 1 and the write error", code, stderr.String())
				}
				if failed.writes < failAt || strings.Contains(stdout.String(), `"event":"stream_error"`) {
					t.Errorf("writes to failing output = %d, stdout = %q", failed.writes, stdout.String())
				}
			})
		}
	}
}

type interruptExecOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *interruptExecOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(w.Bytes(), []byte(`"event":"stdout"`)) && bytes.Contains(w.Bytes(), []byte(`"event":"stderr"`)) {
		w.cancel()
	}
	return n, err
}

func TestSessionExecJSONResumesBufferedUTF8(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	f := newFixture(t)
	f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, f.wd)
	f.env[EnvAPIKey] = f.srv.APIKey
	f.env[EnvOrganization] = f.org
	var session struct {
		ID string `json:"dev_session_id"`
	}
	if err := json.Unmarshal([]byte(f.ok("", "--output", "json", "session", "create", "linux").stdout), &session); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdout := &interruptExecOutput{cancel: cancel}
	var stderr bytes.Buffer
	// Wait for a local file so the continuation cannot reach the first CLI call.
	const command = `printf 'a\342\202'; printf 'ok' >&2; while [ ! -f resume ]; do sleep 0.1; done; printf '\254'`
	code := Run(ctx, Env{
		Args:      []string{"--output", "json", "session", "exec", session.ID, "--", "sh", "-c", command},
		Stdout:    stdout,
		Stderr:    &stderr,
		LookupEnv: func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
	})
	if code != ExitInterrupted {
		t.Fatalf("interrupted exec = %d %s", code, stderr.String())
	}
	// The two guest pipes can become readable in either order.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("interrupted exec events = %s", stdout.String())
	}
	if strings.Contains(lines[1], `"event":"stdout"`) {
		lines[1], lines[2] = lines[2], lines[1]
	}
	checkExecJSONLines(t, strings.Join(lines, "\n"),
		execJSONLine{Event: "started"},
		execJSONLine{Event: "stderr", Offset: 0, Data: "ok"},
		execJSONLine{Event: "stdout", Offset: 0, Data: "a"},
	)
	_, hint, ok := strings.Cut(stderr.String(), "Continue with: sandrun ")
	if !ok || !strings.HasSuffix(strings.TrimSpace(hint), "--stdout-offset 1 --stderr-offset 2") {
		t.Fatalf("attach hint = %s", stderr.String())
	}
	if err := os.WriteFile(filepath.Join(f.wd, "resume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	attached := f.ok("", strings.Fields(hint)...)
	checkExecJSONLines(t, attached.stdout,
		execJSONLine{Event: "stdout", Offset: 1, Data: "€"},
		execJSONLine{Event: "exited"},
	)
}

func TestExecAttachCommandQuotesArguments(t *testing.T) {
	const value = "--api-url=http://[::1]:8080"
	for _, tc := range []struct {
		label   string
		windows bool
		input   string
		want    string
	}{
		{"empty flag value", false, "--output=", "--output="},
		{"IPv6 POSIX", false, value, "'" + value + "'"},
		{"IPv6 PowerShell", true, value, "'" + value + "'"},
		{"quotes POSIX", false, "--org=a'b $c", `'--org=a'"'"'b $c'`},
		{"quotes PowerShell", true, "--org=a'b‘’‚‛ $c", `'--org=a''b‘‘’’‚‚‛‛ $c'`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := quoteAttachArg(tc.input, tc.windows); got != tc.want {
				t.Fatalf("quoted arg = %q, want %q", got, tc.want)
			}
		})
	}
}

func (f *fixture) run(stdin string, args ...string) result {
	f.t.Helper()
	return f.runContext(context.Background(), stdin, args...)
}

func (f *fixture) runContext(ctx context.Context, stdin string, args ...string) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := Run(ctx, Env{
		Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		LookupEnv:  func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
		Getwd:      func() (string, error) { return f.wd, nil },
		HTTPClient: f.httpClient,
	})
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (f *fixture) ok(stdin string, args ...string) result {
	f.t.Helper()
	r := f.run(stdin, args...)
	if r.code != 0 {
		f.t.Fatalf("sandrun %v exited %d: %s%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

func TestUsageErrors(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{}, {"nope"}, {"org"}, {"org", "nope"}, {"--output", "yaml", "org", "list"},
		{"org", "create"}, {"org", "create", "a", "b"}, {"profile", "create", "--vcpu", "99999999999", "--name", "x"},
		{"profile", "create", "--name", "x", "--kind", "bad", "--os", "linux"}, {"session", "exec", "id"},
		{"session", "create", "linux", "--wait", "soon"}, {"mcp", "--pass-env", "BAD-NAME"},
	} {
		if r := f.run("", args...); r.code != ExitUsage {
			t.Errorf("sandrun %v exited %d, want 2: %s", args, r.code, r.stderr)
		}
	}
	if r := f.run("", "org", "list", "--org"); r.code != ExitUsage {
		t.Errorf("missing flag value exited %d", r.code)
	}
	if r := f.ok("", "help"); !strings.Contains(r.stdout, "session") || !strings.Contains(r.stdout, "org-settings") {
		t.Errorf("help: %s", r.stdout)
	}
	if r := f.ok("", "session", "exec", "--help"); !strings.Contains(r.stdout, "-timeout") {
		t.Errorf("subcommand help: %s", r.stdout)
	}
	// A command that needs an organization fails before any request.
	if r := f.run("", "member", "list"); r.code != ExitUsage || !strings.Contains(r.stderr, "sandrun org use") {
		t.Errorf("no organization: %d %s", r.code, r.stderr)
	}
}

func TestCredentialPrecedenceAndErrors(t *testing.T) {
	f := newFixture(t)
	f.ok("", "--org", f.org, "session", "list")
	if f.srv.LastCredential() != sandruntest.CredentialAccessToken {
		t.Fatal("the stored Logto token was not used")
	}
	f.env[EnvAPIKey] = f.srv.APIKey
	f.ok("", "--org", f.org, "session", "list")
	if f.srv.LastCredential() != sandruntest.CredentialAPIKey {
		t.Fatal("SANDRUN_API_KEY did not take precedence")
	}
	r := f.run("", "org", "list")
	if r.code != ExitError || !strings.Contains(r.stderr, "permission_denied") {
		t.Fatalf("org list with an API key = %d %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, f.srv.APIKey) || strings.Contains(r.stdout, f.srv.APIKey) {
		t.Fatal("the API key was printed")
	}
	delete(f.env, EnvAPIKey)

	// Without stored tokens or an API key, the error says what to do.
	f.env[EnvConfigDir] = t.TempDir()
	r = f.run("", "org", "list")
	if r.code != ExitError || !strings.Contains(r.stderr, "sandrun login") || !strings.Contains(r.stderr, EnvAPIKey) {
		t.Fatalf("no credential = %d %s", r.code, r.stderr)
	}
	f.env[EnvConfigDir] = f.cfg

	// A server error exits 1 with the Connect code.
	r = f.run("", "--org", f.org, "request", "get", "missing")
	if r.code != ExitError || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("server error = %d %s", r.code, r.stderr)
	}
	// A network error exits 1.
	f.env[EnvAPIURL] = "http://127.0.0.1:1"
	if r := f.run("", "org", "list"); r.code != ExitError {
		t.Fatalf("network error = %d %s", r.code, r.stderr)
	}
}

func TestCredentialInput(t *testing.T) {
	f := newFixture(t)
	r := f.ok("ownsand-key-from-stdin\n", "--org", f.org, "ownsand", "link", "--ownsand-org", "os-org")
	if _, key := f.srv.OwnSandLink(f.org); key != "ownsand-key-from-stdin" {
		t.Fatalf("stdin key = %q", key)
	}
	if strings.Contains(r.stdout+r.stderr, "ownsand-key-from-stdin") {
		t.Fatal("the credential from stdin was printed")
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("ownsand-key-from-file\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = f.ok("", "--org", f.org, "ownsand", "link", "--ownsand-org", "os-org", "--api-key-file", file)
	if _, key := f.srv.OwnSandLink(f.org); key != "ownsand-key-from-file" {
		t.Fatalf("file key = %q", key)
	}
	if strings.Contains(r.stdout+r.stderr, "ownsand-key-from-file") {
		t.Fatal("the credential was printed")
	}
	if r := f.run("", "--org", f.org, "ownsand", "link", "--ownsand-org", "os-org"); r.code != ExitUsage {
		t.Fatalf("empty stdin = %d", r.code)
	}
	// There is no flag that takes a credential value.
	if r := f.run("", "--org", f.org, "ownsand", "link", "--ownsand-org", "os-org", "--api-key", "k"); r.code != ExitUsage {
		t.Fatalf("--api-key accepted: %d", r.code)
	}

	f.ok(`{"environment_key": "ek", "webhook_secret": "ws"}`, "--org", f.org, "integration", "create", "--vendor", "openai-codex", "--name", "codex")
	_, reqs := f.srv.Integrations()
	if c := reqs[0].GetOpenaiCodexCredentials(); c.GetEnvironmentKey() != "ek" || c.GetWebhookSecret() != "ws" {
		t.Fatalf("integration credentials = %v", c)
	}
	r = f.run("not json super-secret", "--org", f.org, "integration", "create", "--vendor", "cursor", "--name", "c")
	if r.code != ExitUsage || strings.Contains(r.stderr, "super-secret") {
		t.Fatalf("invalid credentials = %d %s", r.code, r.stderr)
	}
}

func TestOutputFormats(t *testing.T) {
	f := newFixture(t)
	r := f.ok("", "org", "list")
	if !strings.Contains(r.stdout, "NAME") && !strings.Contains(r.stdout, "name") || !strings.Contains(r.stdout, "acme") {
		t.Fatalf("table = %s", r.stdout)
	}
	r = f.ok("", "--output", "json", "org", "list")
	var list struct {
		Organizations []map[string]any `json:"organizations"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list.Organizations) != 1 || list.Organizations[0]["organization_id"] != f.org {
		t.Fatalf("json = %s %v", r.stdout, err)
	}

	// api-key create prints only the secret on stdout in text mode.
	r = f.ok("", "--org", f.org, "api-key", "create", "--name", "agent")
	secret := strings.TrimSpace(r.stdout)
	if !strings.HasPrefix(secret, "sandrun_") || strings.Contains(r.stdout, "agent") || !strings.Contains(r.stderr, "agent") {
		t.Fatalf("secret output: stdout %q stderr %q", r.stdout, r.stderr)
	}
	r = f.ok("", "--org", f.org, "--output", "json", "api-key", "create", "--name", "agent2")
	var key map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &key); err != nil || !strings.HasPrefix(key["secret"].(string), "sandrun_") || key["name"] != "agent2" {
		t.Fatalf("json secret = %s", r.stdout)
	}
	r = f.ok("", "--org", f.org, "api-key", "list")
	if strings.Contains(r.stdout, secret) {
		t.Fatal("api-key list printed a secret")
	}

	// Color is on by default and off with --no-color or NO_COLOR.
	delete(f.env, EnvNoColor)
	if r := f.run("", "nope"); !strings.Contains(r.stderr, "\x1b[") {
		t.Fatalf("no color by default: %q", r.stderr)
	}
	if r := f.run("", "--no-color", "nope"); strings.Contains(r.stderr, "\x1b[") {
		t.Fatalf("color with --no-color: %q", r.stderr)
	}
}

func TestGitHubInstallURLOutput(t *testing.T) {
	f := newFixture(t)
	const url = "https://github.com/apps/sandrun/installations/new?state=test-state"
	text := f.ok("", "github", "install-url", "--org", f.org)
	if text.stdout != url+"\n" {
		t.Errorf("text stdout = %q, want the URL and a newline", text.stdout)
	}
	if !strings.HasPrefix(text.stderr, "Open the URL in a browser before ") ||
		!strings.HasSuffix(text.stderr, "; the SandRun console completes the link.\n") {
		t.Errorf("text stderr = %q, want the browser instruction", text.stderr)
	}

	jsonOutput := f.ok("", "github", "install-url", "--org", f.org, "--output", "json")
	var response map[string]string
	if err := json.Unmarshal([]byte(jsonOutput.stdout), &response); err != nil {
		t.Fatalf("decode install URL JSON: %v", err)
	}
	if len(response) != 2 || response["url"] != url || response["expire_time"] == "" || jsonOutput.stderr != "" {
		t.Errorf("JSON stdout = %q, stderr = %q, want the URL and expiry on stdout only", jsonOutput.stdout, jsonOutput.stderr)
	}
}

type installURLFailingWriter struct{ err error }

func (w installURLFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestGitHubInstallURLWriteError(t *testing.T) {
	f := newFixture(t)
	writeErr := errors.New("stdout write failed")
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Run(context.Background(), Env{
				Args:   []string{"github", "install-url", "--org", f.org, "--output", format},
				Stdout: installURLFailingWriter{err: writeErr},
				Stderr: &stderr,
				LookupEnv: func(key string) (string, bool) {
					value, ok := f.env[key]
					return value, ok
				},
			})
			if code != ExitError || stderr.String() != "error: "+writeErr.Error()+"\n" {
				t.Fatalf("exit = %d, stderr = %q, want exit %d and only the write error", code, stderr.String(), ExitError)
			}
		})
	}
}

type organizationListFixture struct {
	sandrunv1connect.UnimplementedOrganizationServiceHandler
	organizations []*sandrunv1.Organization
}

func (f organizationListFixture) ListOrganizations(context.Context, *connect.Request[sandrunv1.ListOrganizationsRequest]) (*connect.Response[sandrunv1.ListOrganizationsResponse], error) {
	return connect.NewResponse(&sandrunv1.ListOrganizationsResponse{Organizations: f.organizations}), nil
}

func TestOrgListMarksSelectedUUIDRegardlessOfCase(t *testing.T) {
	const selected = "019abcde-1234-7abc-8def-012345abcdef"
	const other = "019abcde-1234-7abc-8def-012345abcdea"
	f := newFixture(t)
	_, handler := sandrunv1connect.NewOrganizationServiceHandler(organizationListFixture{
		organizations: []*sandrunv1.Organization{{OrganizationId: selected}, {OrganizationId: other}},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	f.env[EnvAPIURL] = srv.URL
	f.httpClient = srv.Client()
	jsonList := f.ok("", "--output", "json", "org", "list").stdout

	for _, source := range []string{"flag", "environment", "settings"} {
		for _, spelling := range []struct{ name, id string }{
			{"lowercase", selected},
			{"uppercase", strings.ToUpper(selected)},
			{"mixed-case", "019aBCde-1234-7aBc-8DeF-012345abcdef"},
		} {
			t.Run(source+"/"+spelling.name, func(t *testing.T) {
				f := *f
				f.t = t
				args := []string{"org", "list"}
				saved := other
				delete(f.env, EnvOrganization)
				switch source {
				case "flag":
					args = append(args, "--org", spelling.id)
					f.env[EnvOrganization] = other
				case "environment":
					f.env[EnvOrganization] = spelling.id
				case "settings":
					saved = spelling.id
				}
				config := []byte(`{"organization_id":"` + saved + `"}`)
				if err := os.WriteFile(filepath.Join(f.cfg, configFile), config, 0o600); err != nil {
					t.Fatal(err)
				}

				out := f.ok("", args...).stdout
				var marked []string
				for line := range strings.SplitSeq(out, "\n") {
					fields := strings.Fields(line)
					if len(fields) >= 2 && fields[0] == "*" {
						marked = append(marked, fields[1])
					}
				}
				if !slices.Equal(marked, []string{selected}) || !strings.Contains(out, other) {
					t.Fatalf("want only %s marked with %s listed, got:\n%s", selected, other, out)
				}
				if got := f.ok("", append(args, "--output", "json")...).stdout; got != jsonList {
					t.Fatalf("selection changed JSON output: got %q, want %q", got, jsonList)
				}
			})
		}
	}
}

func TestOrgUseAndSettings(t *testing.T) {
	f := newFixture(t)
	other := f.srv.AddOrganization("other").GetOrganizationId()
	f.ok("", "org", "use", f.org)
	r := f.ok("", "org", "list")
	if !strings.Contains(r.stdout, "* ") && !strings.Contains(r.stdout, "*  ") {
		t.Fatalf("selected organization not marked: %s", r.stdout)
	}
	f.ok("", "member", "list")
	if r := f.run("", "org", "use", "missing"); r.code != ExitError {
		t.Fatalf("org use of an unknown organization = %d", r.code)
	}
	r = f.ok("", "--output", "json", "org-settings", "get")
	if strings.TrimSpace(r.stdout) != "{}" {
		t.Fatalf("settings = %s", r.stdout)
	}
	f.ok("", "org-settings", "set-dev-session-limit", "2")
	r = f.ok("", "org-settings", "get")
	if !strings.Contains(r.stdout, "max_dev_sessions_per_user: 2") {
		t.Fatalf("settings = %s", r.stdout)
	}
	f.ok("", "org-settings", "clear-dev-session-limit")
	if r := f.ok("", "org-settings", "get"); !strings.Contains(r.stdout, "none") {
		t.Fatalf("cleared settings = %s", r.stdout)
	}
	if r := f.run("", "org-settings", "set-dev-session-limit", "0"); r.code != ExitUsage {
		t.Fatalf("limit 0 = %d", r.code)
	}
	// SANDRUN_ORG and --org override the stored organization.
	f.env[EnvOrganization] = other
	r = f.ok("", "--output", "json", "member", "list")
	if !strings.Contains(r.stdout, other) {
		t.Fatalf("SANDRUN_ORG not used: %s", r.stdout)
	}
	r = f.ok("", "--org", f.org, "--output", "json", "member", "list")
	if !strings.Contains(r.stdout, f.org) {
		t.Fatalf("--org not used: %s", r.stdout)
	}
}

func TestStatus(t *testing.T) {
	f := newFixture(t)
	f.srv.AddRequest(&sandrunv1.Request{RequestId: "r1", OrganizationId: f.org, State: sandrunv1.RequestState_REQUEST_STATE_QUEUED, Kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB})
	f.srv.AddRunner(&sandrunv1.Runner{RunnerId: "n1", OrganizationId: f.org, State: sandrunv1.RunnerState_RUNNER_STATE_BUSY})
	f.srv.AddDevice(&sandrunv1.DeviceCapacity{OwnsandDeviceId: "d1", Name: "mac-mini", State: sandrunv1.DeviceState_DEVICE_STATE_ONLINE})
	r := f.ok("", "--org", f.org, "status")
	for _, want := range []string{"1 queued", "busy: 1", "mac-mini", "github-job"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q: %s", want, r.stdout)
		}
	}
	r = f.ok("", "--org", f.org, "--output", "json", "status")
	var st map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || st["runners_by_state"].(map[string]any)["busy"] != float64(1) {
		t.Fatalf("status json = %s %v", r.stdout, err)
	}
	if r := f.run("", "--org", f.org, "request", "cancel", "r1"); r.code != 0 {
		t.Fatalf("cancel = %d %s", r.code, r.stderr)
	}
}

func TestSessionCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	f := newFixture(t)
	ws := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, ws)
	f.env[EnvAPIKey] = f.srv.APIKey
	f.env[EnvOrganization] = f.org

	r := f.ok("", "session", "profiles")
	if !strings.Contains(r.stdout, "linux") || !strings.Contains(r.stdout, ws) {
		t.Fatalf("profiles = %s", r.stdout)
	}
	f.srv.NewSessionState = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	f.srv.PromoteAfterGets = 2
	r = f.ok("", "--output", "json", "session", "create", "linux", "--label", "ci", "--wait", "30s")
	var ds map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil || ds["state"] != "REQUEST_STATE_RUNNING" {
		t.Fatalf("create --wait = %s", r.stdout)
	}
	id := ds["dev_session_id"].(string)
	if r := f.run("", "session", "create", "missing"); r.code != ExitError || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("unknown profile = %d %s", r.code, r.stderr)
	}
	f.srv.PromoteAfterGets = 0
	queued := strings.TrimSpace(f.ok("", "--output", "json", "session", "create", "linux").stdout)
	var q map[string]any
	_ = json.Unmarshal([]byte(queued), &q)
	if r := f.run("", "session", "wait", q["dev_session_id"].(string), "--timeout", "100ms"); r.code != ExitError || !strings.Contains(r.stderr, "still queued") {
		t.Fatalf("wait timeout = %d %s", r.code, r.stderr)
	}
	r = f.ok("", "session", "list")
	if !strings.Contains(r.stdout, id) {
		t.Fatalf("list = %s", r.stdout)
	}

	// Exit status propagation.
	r = f.run("", "session", "exec", id, "--", "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("exec = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := f.run("", "session", "exec", "--env", "X=1", id, "sh", "-c", `test "$X" = 1`); r.code != 0 {
		t.Fatalf("exec env = %d %s", r.code, r.stderr)
	}
	if r := f.run("", "session", "exec", id, "--", "sh", "-c", "kill -TERM $$"); r.code != ExitSignalBase+15 {
		t.Fatalf("signal exit = %d", r.code)
	}
	if r := f.run("", "session", "exec", id, "--", "sandrun-no-such-program"); r.code != ExitCommandNotStart {
		t.Fatalf("missing program = %d %s", r.code, r.stderr)
	}
	if r := f.run("", "session", "exec", "--timeout", "100ms", id, "--", "sleep", "5"); r.code != ExitExecTimedOut {
		t.Fatalf("timeout = %d", r.code)
	}
	r = f.run("", "--output", "json", "session", "exec", id, "--", "sh", "-c", "echo hi; exit 7")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var last map[string]any
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if r.code != 0 || last["event"] != "exited" || last["exit_code"] != float64(7) || !strings.Contains(r.stdout, `"data":"hi\n"`) {
		t.Fatalf("json exec = %d %s", r.code, r.stdout)
	}
	const splitUTF8 = `printf 'a\342\202'; sleep 0.5; printf '\254z'`
	r = f.ok("", "--output", "json", "session", "exec", id, "--", "sh", "-c", splitUTF8)
	var joined strings.Builder
	for _, event := range parseExecJSONLines(t, r.stdout) {
		if event.Event == "stdout" {
			joined.WriteString(event.Data)
		}
	}
	if joined.String() != "a€z" {
		t.Fatalf("JSON exec events = %s", r.stdout)
	}
	if r := f.ok("", "session", "exec", id, "--", "sh", "-c", splitUTF8); r.stdout != "a€z" {
		t.Fatalf("text exec stdout = %q", r.stdout)
	}

	// Sync and download.
	if _, err := exec.LookPath("git"); err == nil {
		repo := f.wd
		cmd := exec.Command("git", "-C", repo, "init", "-q")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %s", out)
		}
		if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		r = f.ok("", "session", "sync", id)
		if !strings.Contains(r.stdout, "Uploaded 1") {
			t.Fatalf("sync = %s %s", r.stdout, r.stderr)
		}
		r = f.ok("", "--output", "json", "session", "sync", id, ".", "--force")
		if !strings.Contains(r.stdout, `"skipped":1`) {
			t.Fatalf("second sync = %s", r.stdout)
		}
		r = f.ok("", "session", "download", id, "hello.txt", "copy.txt")
		if b, err := os.ReadFile(filepath.Join(repo, "copy.txt")); err != nil || string(b) != "hello" {
			t.Fatalf("download = %s %v", r.stdout, err)
		}
	}
	if r := f.run("", "session", "download", id, "missing", "x"); r.code != ExitError || !strings.Contains(r.stderr, "not_found") {
		t.Fatalf("download missing = %d %s", r.code, r.stderr)
	}

	// A session that is not running rejects commands with exit status 1.
	f.srv.SetSessionState(id, sandrunv1.RequestState_REQUEST_STATE_STARTING)
	if r := f.run("", "session", "exec", id, "--", "true"); r.code != ExitError || !strings.Contains(r.stderr, "failed_precondition") {
		t.Fatalf("exec in a starting session = %d %s", r.code, r.stderr)
	}
	r = f.ok("", "--output", "json", "session", "delete", id)
	if !strings.Contains(r.stdout, "REQUEST_STATE_ENDED") {
		t.Fatalf("delete = %s", r.stdout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionCreateWaitFailures(t *testing.T) {
	const getSession = sandrunv1connect.DevSessionServiceGetDevSessionProcedure
	const queued = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	const starting = sandrunv1.RequestState_REQUEST_STATE_STARTING
	denied := connect.NewError(connect.CodePermissionDenied, errors.New("nope"))
	unavailable := make([]error, 50)
	for i := range unavailable {
		unavailable[i] = connect.NewError(connect.CodeUnavailable, errors.New("unavailable"))
	}

	for _, tc := range []struct {
		name     string
		wait     string
		faults   []error
		state    sandrunv1.RequestState
		cancel   bool
		stderr   string
		maxPolls int
	}{
		{name: "deadline before first read", wait: "300ms", faults: unavailable, state: queued, stderr: "error: the dev session is still queued after 300ms\n", maxPolls: 1},
		{name: "first poll fails", wait: "30s", faults: []error{denied}, state: queued, stderr: "error: permission_denied: nope\n", maxPolls: 1},
		{name: "later poll fails", wait: "30s", faults: []error{nil, denied}, state: starting, stderr: "error: permission_denied: nope\n", maxPolls: 2},
		{name: "parent canceled", wait: "30s", state: starting, cancel: true, stderr: "error: context canceled\n", maxPolls: 1},
	} {
		for _, format := range []string{"text", "json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.env[EnvOrganization] = f.org
				f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, f.wd)
				f.srv.NewSessionState = queued
				f.srv.Fail(getSession, tc.faults...)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				// Return a different state from polling so the test can distinguish it from creation.
				if tc.state == starting {
					f.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == getSession {
							if f.srv.Calls(getSession) == 0 {
								session := f.srv.Sessions()[0]
								f.srv.SetSessionState(session.GetDevSessionId(), starting)
							} else if tc.cancel {
								// The SDK has received the first response when it starts the second poll.
								cancel()
								return nil, r.Context().Err()
							}
						}
						return http.DefaultTransport.RoundTrip(r)
					})}
				}

				r := f.runContext(ctx, "", "--output", format, "session", "create", "linux", "--wait", tc.wait)
				if r.code != ExitError || r.stderr != tc.stderr {
					t.Fatalf("create --wait = %d, stderr %q, want %d and %q", r.code, r.stderr, ExitError, tc.stderr)
				}
				sessions := f.srv.Sessions()
				if len(sessions) != 1 {
					t.Fatalf("created %d sessions, want one", len(sessions))
				}
				id := sessions[0].GetDevSessionId()
				if format == "json" {
					var session struct {
						ID    string `json:"dev_session_id"`
						State string `json:"state"`
					}
					if err := json.Unmarshal([]byte(r.stdout), &session); err != nil || session.ID != id || session.State != tc.state.String() {
						t.Fatalf("create --wait JSON = %q (%v), want %s in %s", r.stdout, err, id, tc.state)
					}
				} else if strings.Count(r.stdout, "dev_session_id: "+id+"\n") != 1 || !strings.Contains(r.stdout, "\nstate: "+tc.state.String()+"\n") {
					t.Fatalf("create --wait text = %q, want %s in %s", r.stdout, id, tc.state)
				}
				if got := f.srv.Calls(getSession); got > tc.maxPolls {
					t.Errorf("got %d polls, want at most %d", got, tc.maxPolls)
				}
				if got := f.srv.Calls(sandrunv1connect.DevSessionServiceDeleteDevSessionProcedure); got != 0 {
					t.Errorf("got %d deletes, want none", got)
				}
			})
		}
	}

	t.Run("explicit wait deadline before first read", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.env[EnvOrganization] = f.org
		f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, f.wd)
		f.srv.NewSessionState = queued
		f.ok("", "session", "create", "linux")
		id := f.srv.Sessions()[0].GetDevSessionId()
		f.srv.Fail(getSession, unavailable...)
		if r := f.run("", "session", "wait", id, "--timeout", "300ms"); r.code != ExitError || r.stdout != "" || r.stderr != "error: context deadline exceeded\n" {
			t.Fatalf("session wait = %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		}
	})
}
