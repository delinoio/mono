package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type cli struct {
	t   *testing.T
	srv *ownsandtest.Server
	env map[string]string
	sb  string
}

func newCLI(t *testing.T) *cli {
	srv := ownsandtest.New(t, ownsandtest.Options{})
	testClock = srv.Clock
	pollSleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { testClock = nil })
	sb := srv.AddSandbox(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return &cli{t: t, srv: srv, sb: sb.GetSandboxId(), env: map[string]string{
		"OWNSAND_API_KEY":    ownsandtest.DefaultAPIKey,
		"OWNSAND_ORG":        srv.Org,
		"OWNSAND_API_ORIGIN": srv.URL,
		"OWNSAND_CONFIG_DIR": filepath.Join(t.TempDir(), "config"),
		"NO_COLOR":           "1",
	}}
}

func (c *cli) run(stdin string, args ...string) (int, string, string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code := run(ctx, args, &stdio{in: strings.NewReader(stdin), out: &out, err: &errb}, func(k string) string { return c.env[k] })
	return code, out.String(), errb.String()
}

// signIn stores a Logto token for a user and removes the API key.
func (c *cli) signIn(token string, expires time.Time) string {
	c.srv.AddUser(token, "user-1")
	delete(c.env, "OWNSAND_API_KEY")
	path := filepath.Join(c.env["OWNSAND_CONFIG_DIR"], credentialFile)
	if err := writeCredential(path, &storedCredential{AccessToken: token, ExpiresAt: expires, Resource: c.srv.URL}); err != nil {
		c.t.Fatal(err)
	}
	return path
}

func TestSandboxCreateArgumentValidation(t *testing.T) {
	c := newCLI(t)
	base := []string{"sandbox", "create", "--image", "linux", "--vcpu", "2", "--memory", "1024", "--disk", "10"}
	cases := [][]string{
		append(append([]string{}, base...), "--max-ttl", "1h"),
		append(append([]string{}, base...), "--idle-ttl", "10m"),
		{"sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "1h"},
		{"sandbox", "create", "--image", "linux", "--vcpu", "2", "--memory", "1024", "--idle-ttl", "10m", "--max-ttl", "1h"},
		append(append([]string{}, base...), "--idle-ttl", "10m", "--max-ttl", "8d"),
		append(append([]string{}, base...), "--idle-ttl", "2h", "--max-ttl", "1h"),
		append(append([]string{}, base...), "--snapshot", "s", "--idle-ttl", "10m", "--max-ttl", "1h"),
		{"sandbox", "create", "--vcpu", "1", "--memory", "1", "--disk", "1", "--idle-ttl", "1m", "--max-ttl", "1h"},
		append(append([]string{}, base...), "--idle-ttl", "10m", "--max-ttl", "1h", "--network", "bogus"),
	}
	for _, args := range cases {
		code, _, stderr := c.run("", args...)
		if code != exitUsage || !strings.Contains(stderr, "error:") {
			t.Errorf("%v: code %d stderr %q", args, code, stderr)
		}
	}
	if n := c.srv.CreateCount(); n != 0 {
		t.Fatalf("server received %d creates", n)
	}
	code, out, stderr := c.run("", append(base, "--idle-ttl", "10m", "--max-ttl", "7d", "--label", "team=ci", "--format", "json")...)
	if code != 0 {
		t.Fatalf("valid create: %d %s", code, stderr)
	}
	var sb map[string]any
	if err := json.Unmarshal([]byte(out), &sb); err != nil || sb["sandbox_id"] == "" || sb["state"] != "SANDBOX_STATE_CREATING" {
		t.Fatalf("output %q", out)
	}
}

func TestExecExitCodePropagation(t *testing.T) {
	c := newCLI(t)
	progs := map[string]ownsandtest.Program{
		"exit0": func(ctx context.Context, x *ownsandtest.ExecIO) {
			x.Stdout([]byte("out\n"))
			x.Stderr([]byte("err\n"))
			x.Exit(0)
		},
		"exit3":   func(ctx context.Context, x *ownsandtest.ExecIO) { x.Exit(3) },
		"exit255": func(ctx context.Context, x *ownsandtest.ExecIO) { x.Exit(255) },
		"exit300": func(ctx context.Context, x *ownsandtest.ExecIO) { x.Exit(300) },
		"exitneg": func(ctx context.Context, x *ownsandtest.ExecIO) { x.Exit(-1073741819) },
		"sig9":    func(ctx context.Context, x *ownsandtest.ExecIO) { x.Signal(9) },
		"sig200":  func(ctx context.Context, x *ownsandtest.ExecIO) { x.Signal(200) },
		"timeout": func(ctx context.Context, x *ownsandtest.ExecIO) { x.ExitTimedOut(143) },
		"ended": func(ctx context.Context, x *ownsandtest.ExecIO) {
			x.SandboxEnded(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED)
		},
	}
	for k, p := range progs {
		c.srv.Agent.Programs[k] = p
	}
	cases := []struct {
		prog string
		text int
	}{
		{"exit0", 0}, {"exit3", 3}, {"exit255", 255}, {"exit300", 255}, {"exitneg", 255},
		{"sig9", 137}, {"sig200", 255}, {"timeout", 124}, {"not-installed", 127}, {"ended", 125},
	}
	for _, tc := range cases {
		code, out, stderr := c.run("", "sandbox", "exec", c.sb, "--transport", "relay", "--", tc.prog, "arg")
		if code != tc.text {
			t.Errorf("%s text: exit %d, want %d (stderr %q)", tc.prog, code, tc.text, stderr)
		}
		if tc.prog == "exit0" && (out != "out\n" || stderr != "err\n") {
			t.Errorf("streams: out %q err %q", out, stderr)
		}
		if tc.prog == "not-installed" && !strings.Contains(stderr, "could not start") {
			t.Errorf("failed reason missing: %q", stderr)
		}
		if tc.prog == "ended" && !strings.Contains(stderr, "MAX_TTL_EXPIRED") {
			t.Errorf("end reason missing: %q", stderr)
		}
		code, out, _ = c.run("", "sandbox", "exec", c.sb, "--transport", "relay", "--format", "json", "--", tc.prog)
		if code != 0 {
			t.Errorf("%s json: exit %d", tc.prog, code)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var last map[string]any
		_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
		switch tc.prog {
		case "exit300":
			if last["event"] != "exited" || last["exit_code"] != float64(300) {
				t.Errorf("json keeps the exact value: %v", last)
			}
		case "sig9":
			if last["signal"] != float64(9) || last["exit_code"] != nil {
				t.Errorf("json signal: %v", last)
			}
		case "timeout":
			if last["timed_out"] != true || last["exit_code"] != float64(143) {
				t.Errorf("json timeout: %v", last)
			}
		case "not-installed":
			if last["event"] != "failed" || last["reason"] == "" {
				t.Errorf("json failed: %v", last)
			}
		case "ended":
			if last["event"] != "sandbox_ended" || last["reason"] != "MAX_TTL_EXPIRED" {
				t.Errorf("json ended: %v", last)
			}
		}
		var first map[string]any
		_ = json.Unmarshal([]byte(lines[0]), &first)
		if first["event"] != "started" || first["exec_id"] == "" {
			t.Errorf("first json event %v", first)
		}
	}
}

func TestExecJSONOutputEncoding(t *testing.T) {
	c := newCLI(t)
	c.srv.Agent.Programs["bin"] = func(ctx context.Context, x *ownsandtest.ExecIO) { x.Stdout([]byte{'o', 'k', 0xff}); x.Exit(0) }
	_, out, _ := c.run("", "sandbox", "exec", c.sb, "--format", "json", "--transport", "relay", "--", "bin")
	if !strings.Contains(out, `"data":"ok�"`) && !strings.Contains(out, `"data":"ok�"`) {
		t.Fatalf("text output: %s", out)
	}
	_, out, _ = c.run("", "sandbox", "exec", c.sb, "--format", "json", "--binary", "--transport", "relay", "--", "bin")
	if !strings.Contains(out, `"data":"b2v/"`) || !strings.Contains(out, `"encoding":"base64"`) {
		t.Fatalf("binary output: %s", out)
	}
}

func TestExecDataPathErrorExitStatus(t *testing.T) {
	c := newCLI(t)
	release := make(chan struct{})
	c.srv.Agent.Programs["wait"] = func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout([]byte("partial"))
		select {
		case <-release:
		case <-ctx.Done():
		}
		x.Exit(0)
	}
	defer close(release)
	c.srv.OnRelay = func(rc *ownsandtest.RelayCall) *ownsandtest.Fault {
		switch rc.N {
		case 1:
			return &ownsandtest.Fault{AfterMessages: 2, After: ownsandtest.ActionAbort}
		case 2:
			return &ownsandtest.Fault{Err: connect.NewError(connect.CodeNotFound, errors.New("exec not found"))}
		}
		return nil
	}
	code, out, stderr := c.run("", "sandbox", "exec", c.sb, "--transport", "relay", "--", "wait")
	if code != 255 || out != "partial" || !strings.Contains(stderr, "data path") {
		t.Fatalf("text: %d %q %q", code, out, stderr)
	}
	code, out, _ = c.run("", "sandbox", "exec", c.sb, "--transport", "relay", "--format", "json", "--", "wait")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 255 || !strings.Contains(lines[len(lines)-1], `"event":"data_path_error"`) {
		t.Fatalf("json: %d %s", code, out)
	}
}

func TestExecLostOutputExitStatus(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		c := newCLI(t)
		c.srv.Agent.KeepBytes = 100
		wrote := make(chan struct{})
		gate := make(chan struct{})
		c.srv.Agent.Programs["flood"] = func(ctx context.Context, x *ownsandtest.ExecIO) {
			x.Stdout(bytes.Repeat([]byte("a"), 10))
			<-gate
			x.Stdout(bytes.Repeat([]byte("b"), 1000))
			close(wrote)
			x.Exit(0)
		}
		c.srv.OnRelay = func(rc *ownsandtest.RelayCall) *ownsandtest.Fault {
			switch rc.N {
			case 1:
				return &ownsandtest.Fault{AfterMessages: 2, After: ownsandtest.ActionAbort}
			case 2:
				close(gate)
				<-wrote
			}
			return nil
		}
		code, out, stderr := c.run("", "sandbox", "exec", c.sb, "--transport", "relay", "--format", format, "--", "flood")
		if code != 255 {
			t.Fatalf("%s: exit %d", format, code)
		}
		if format == "text" && !strings.Contains(stderr, "lost stdout output at offsets [10, 910)") {
			t.Fatalf("warning missing: %q", stderr)
		}
		if format == "json" && !strings.Contains(out, `"event":"output_truncated"`) {
			t.Fatalf("json: %s", out)
		}
	}
}

func TestSecretsPrintedToStdoutOnly(t *testing.T) {
	c := newCLI(t)
	c.signIn("user-token", time.Now().Add(time.Hour))
	for _, args := range [][]string{
		{"api-key", "create", "ci"},
		{"member", "invite", "--role", "member"},
		{"enrollment-token", "create", "--expires-in", "1h"},
	} {
		code, out, stderr := c.run("", args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
		secret := strings.TrimSpace(out)
		if strings.Count(out, "\n") != 1 || !strings.HasPrefix(secret, "os") || strings.Contains(stderr, secret) {
			t.Fatalf("%v: stdout %q stderr %q", args, out, stderr)
		}
		if !strings.Contains(stderr, "secret_prefix") {
			t.Fatalf("%v: other fields not on stderr: %q", args, stderr)
		}
		code, out, stderr = c.run("", append(args, "--format", "json")...)
		var obj map[string]any
		if code != 0 || json.Unmarshal([]byte(out), &obj) != nil || obj["secret"] == "" || len(obj) != 2 || stderr != "" {
			t.Fatalf("%v json: %d %q %q", args, code, out, stderr)
		}
	}
	// List commands never print secrets.
	_, out, _ := c.run("", "api-key", "list", "--format", "json")
	if strings.Contains(out, "osk_") && !strings.Contains(out, "secret_prefix") {
		t.Fatalf("list output %s", out)
	}
}

func TestAPIKeyTakesPrecedenceOverStoredTokens(t *testing.T) {
	c := newCLI(t)
	c.signIn("user-token", time.Now().Add(time.Hour))
	if code, _, stderr := c.run("", "org", "list"); code != 0 {
		t.Fatalf("user: %d %s", code, stderr)
	}
	c.env["OWNSAND_API_KEY"] = ownsandtest.DefaultAPIKey
	code, _, stderr := c.run("", "org", "list")
	if code != exitError || !strings.Contains(stderr, "permission_denied") {
		t.Fatalf("API key: %d %q", code, stderr)
	}
	if last := c.srv.SeenCredentials[len(c.srv.SeenCredentials)-1]; last != "Bearer "+ownsandtest.DefaultAPIKey {
		t.Fatal("stored token used despite OWNSAND_API_KEY")
	}
	// An API key with --org of another organization gets not_found.
	code, _, stderr = c.run("", "device", "list", "--org", "0190f5d6-1c2b-7a3e-8f00-00000000ffff")
	if code != exitError || !strings.Contains(stderr, "not_found") {
		t.Fatalf("other org: %d %q", code, stderr)
	}
}

func TestNotSignedIn(t *testing.T) {
	c := newCLI(t)
	delete(c.env, "OWNSAND_API_KEY")
	code, _, stderr := c.run("", "sandbox", "list")
	if code != exitError || !strings.Contains(stderr, "ownsand login") {
		t.Fatalf("%d %q", code, stderr)
	}
}

// fakeLogto serves the device authorization and token endpoints.
type fakeLogto struct {
	srv      *httptest.Server
	polls    atomic.Int32
	refresh  atomic.Int32
	lastForm atomic.Value
}

func newFakeLogto(t *testing.T) *fakeLogto {
	f := &fakeLogto{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.lastForm.Store(r.Form)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oidc/device/auth":
			if r.Form.Get("client_id") != "cli-app" || r.Form.Get("resource") == "" || !strings.Contains(r.Form.Get("scope"), "offline_access") {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri":"https://login.example/device","expires_in":600,"interval":5}`))
		case "/oidc/token":
			switch r.Form.Get("grant_type") {
			case deviceGrant:
				switch f.polls.Add(1) {
				case 1:
					_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				case 2:
					_, _ = w.Write([]byte(`{"error":"slow_down"}`))
				default:
					_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"token_type":"Bearer"}`))
				}
			case "refresh_token":
				f.refresh.Add(1)
				if r.Form.Get("refresh_token") != "rt-1" {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`))
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestLoginStoresUserOnlyCredentialFile(t *testing.T) {
	c := newCLI(t)
	lg := newFakeLogto(t)
	delete(c.env, "OWNSAND_API_KEY")
	code, out, stderr := c.run("", "login", "--logto-endpoint", lg.srv.URL, "--app-id", "cli-app")
	if code != 0 || out != "" || !strings.Contains(stderr, "ABCD-EFGH") || !strings.Contains(stderr, "https://login.example/device") {
		t.Fatalf("login: %d %q %q", code, out, stderr)
	}
	if strings.Contains(stderr, "at-1") || strings.Contains(stderr, "rt-1") {
		t.Fatal("token printed")
	}
	path := filepath.Join(c.env["OWNSAND_CONFIG_DIR"], credentialFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("credential file mode %v", st.Mode().Perm())
		}
		dst, _ := os.Stat(filepath.Dir(path))
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %v", dst.Mode().Perm())
		}
	}
	b, _ := os.ReadFile(path)
	var cred storedCredential
	if err := json.Unmarshal(b, &cred); err != nil || cred.AccessToken != "at-1" || cred.RefreshToken != "rt-1" || cred.Resource != c.srv.URL {
		t.Fatalf("stored %s", b)
	}
	if code, _, _ := c.run("", "logout"); code != 0 {
		t.Fatal("logout")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("logout kept the credential file")
	}
	if code, _, stderr := c.run("", "login"); code != exitUsage || !strings.Contains(stderr, "OWNSAND_LOGTO_ENDPOINT") {
		t.Fatalf("login without configuration: %d %q", code, stderr)
	}
	if code, _, _ := c.run("", "login", "--logto-endpoint", "http://logto.example", "--app-id", "x"); code != exitUsage {
		t.Fatal("plain http Logto endpoint accepted")
	}
}

func TestExpiredTokenIsRefreshedAndRotated(t *testing.T) {
	c := newCLI(t)
	lg := newFakeLogto(t)
	path := c.signIn("at-1", time.Now().Add(-time.Minute))
	c.srv.AddUser("at-2", "user-1")
	cred := &storedCredential{LogtoEndpoint: lg.srv.URL, AppID: "cli-app", Resource: c.srv.URL, AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := writeCredential(path, cred); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := c.run("", "org", "list"); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	if lg.refresh.Load() != 1 || c.srv.SeenCredentials[len(c.srv.SeenCredentials)-1] != "Bearer at-2" {
		t.Fatal("token not refreshed")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "rt-2") {
		t.Fatal("rotated refresh token not stored")
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode after refresh %v", st.Mode().Perm())
	}
}

func TestMemberAcceptReadsSecretFromStdinOrFile(t *testing.T) {
	c := newCLI(t)
	c.signIn("user-token", time.Now().Add(time.Hour))
	_, secret, _ := c.run("", "member", "invite", "--role", "admin")
	secret = strings.TrimSpace(secret)
	if code, _, stderr := c.run("", "member", "accept", secret); code != exitUsage || !strings.Contains(stderr, "not accepted as an argument") {
		t.Fatalf("argument accepted: %d %q", code, stderr)
	}
	if code, out, stderr := c.run(secret+"\n", "member", "accept", "--format", "json"); code != 0 || !strings.Contains(out, "ROLE_ADMIN") {
		t.Fatalf("stdin: %d %q %q", code, out, stderr)
	}
	_, secret2, _ := c.run("", "member", "invite", "--role", "member")
	f := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(f, []byte(secret2), 0o600)
	// --org is ignored: the invitation names the organization.
	if code, _, stderr := c.run("", "member", "accept", "--secret-file", f, "--org", "other"); code != 0 {
		t.Fatalf("file: %d %q", code, stderr)
	}
	if code, _, _ := c.run("", "member", "accept"); code != exitUsage {
		t.Fatal("empty stdin accepted")
	}
}

func TestFileReadWriteRaw(t *testing.T) {
	c := newCLI(t)
	data := "raw\x00bytes\n"
	if code, out, stderr := c.run(data, "file", "write", c.sb, "/tmp/x", "--format", "json", "--transport", "relay"); code != 0 || out != "" {
		t.Fatalf("write: %d %q %q", code, out, stderr)
	}
	if code, out, _ := c.run("", "file", "read", c.sb, "/tmp/x", "--format", "json", "--transport", "relay"); code != 0 || out != data {
		t.Fatalf("read: %d %q", code, out)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if code, _, _ := c.run("", "file", "read", c.sb, "/tmp/x", "--to", dst, "--transport", "relay"); code != 0 {
		t.Fatal("read --to")
	}
	if b, _ := os.ReadFile(dst); string(b) != data {
		t.Fatal("--to content")
	}
	if code, _, stderr := c.run("", "file", "read", c.sb, "/missing", "--transport", "relay"); code != 255 || !strings.Contains(stderr, "not_found") {
		t.Fatalf("missing: %d %q", code, stderr)
	}
}

func TestOutputFormatsAndColor(t *testing.T) {
	c := newCLI(t)
	code, out, _ := c.run("", "sandbox", "list", "--format", "json")
	var obj struct {
		Items []map[string]any `json:"items"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &obj) != nil || len(obj.Items) != 1 {
		t.Fatalf("json list: %q", out)
	}
	code, out, _ = c.run("", "sandbox", "get", c.sb)
	if code != 0 || !strings.Contains(out, "sandbox_id: "+c.sb) || strings.Contains(out, "\x1b[") {
		t.Fatalf("text get: %q", out)
	}
	delete(c.env, "NO_COLOR")
	_, out, _ = c.run("", "sandbox", "get", c.sb)
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("no color by default")
	}
	_, out, _ = c.run("", "--no-color", "sandbox", "get", c.sb)
	if strings.Contains(out, "\x1b[") {
		t.Fatal("--no-color ignored")
	}
	if code, _, _ := c.run("", "sandbox", "list", "--format", "yaml"); code != exitUsage {
		t.Fatal("bad format accepted")
	}
	delete(c.env, "OWNSAND_ORG")
	if code, _, stderr := c.run("", "sandbox", "list"); code != exitUsage || !strings.Contains(stderr, "--org") {
		t.Fatalf("missing org: %d %q", code, stderr)
	}
	if code, _, _ := c.run("", "sandbox", "list", "--org", c.srv.Org); code != 0 {
		t.Fatal("--org ignored")
	}
}

func TestVerboseLogsNoSecrets(t *testing.T) {
	c := newCLI(t)
	code, out, stderr := c.run("", "--verbose", "sandbox", "exec", c.sb, "--transport", "relay", "--", "echo", "hidden-content")
	if code != 0 || out != "hidden-content\n" {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(stderr, "level=DEBUG") || !strings.Contains(stderr, "RelayService") {
		t.Fatalf("no structured logs: %q", stderr)
	}
	if strings.Contains(stderr, "hidden-content") || strings.Contains(stderr, ownsandtest.DefaultAPIKey) {
		t.Fatalf("logs leak content or credentials: %q", stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	c := newCLI(t)
	for _, args := range [][]string{{}, {"bogus"}, {"sandbox"}, {"sandbox", "bogus"}, {"sandbox", "exec", c.sb}, {"member", "invite", "--role", "owner"}} {
		if code, _, _ := c.run("", args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code, out, _ := c.run("", "help"); code != 0 || !strings.Contains(out, "enrollment-token") {
		t.Fatal("help")
	}
}
