// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/cmds/internal/logto"
	ownsand "github.com/delinoio/mono/go/ownsand"
)

type result struct {
	code   int
	stdout string
	stderr string
}

type runner struct {
	t     *testing.T
	f     *fakeServer
	env   map[string]string
	stdin string
}

func newRunner(t *testing.T) *runner {
	f := newFakeServer(t)
	return &runner{t: t, f: f, env: map[string]string{
		EnvAPIURL:               f.srv.URL,
		ownsand.EnvAPIKey:       "osk_testkey",
		ownsand.EnvOrganization: f.org,
		EnvTransport:            "relay",
		EnvConfigDir:            filepath.Join(t.TempDir(), "config"),
	}}
}

func (r *runner) run(args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	code := r.runWriters(&out, &errb, args...)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// runWriters runs one invocation on the given standard output and error and
// returns the exit status.
func (r *runner) runWriters(stdout, stderr io.Writer, args ...string) int {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return Run(ctx, Env{
		Args:   args,
		Stdin:  strings.NewReader(r.stdin),
		Stdout: stdout,
		Stderr: stderr,
		LookupEnv: func(k string) (string, bool) {
			v, ok := r.env[k]
			return v, ok
		},
		LoginPollInterval: 10 * time.Millisecond,
	})
}

func TestUsageErrors(t *testing.T) {
	r := newRunner(t)
	for _, args := range [][]string{
		{},
		{"nope"},
		{"sandbox"},
		{"sandbox", "nope"},
		{"sandbox", "create", "--image", "linux", "--vcpu", "1", "--memory", "512", "--disk", "5"},                                                         // no TTL
		{"sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "1h"},                                                                  // no resources
		{"sandbox", "create", "--image", "linux", "--snapshot", "x", "--idle-ttl", "1m", "--max-ttl", "1h", "--vcpu", "1", "--memory", "1", "--disk", "1"}, // two sources
		{"sandbox", "create", "--image", "linux", "--idle-ttl", "2h", "--max-ttl", "1h", "--vcpu", "1", "--memory", "1", "--disk", "1"},                    // idle above max
		{"sandbox", "exec", "not-an-id", "--", "ls"},
		{"sandbox", "exec", r.f.sb},
		{"member", "accept", "osi_secret_as_argument"},
		{"device", "set-limits", r.f.dev, "--vcpu", "1"},
		{"--format", "yaml", "org", "list"},
		{"file", "write", r.f.sb, "/tmp/x", "--mode", "0"},
		{"file", "write", r.f.sb, "/tmp/x", "--mode", "0000"},
		{"file", "write", r.f.sb, "/tmp/x", "--mode", "00"},
	} {
		res := r.run(args...)
		if res.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, res.code, ExitUsage, res.stderr)
		}
	}
	for _, c := range r.f.callNames() {
		if c == "CreateSandbox" || c == "AcceptInvitation" || c == "ReplaceDeviceLimits" {
			t.Fatalf("%s sent for an invalid command", c)
		}
	}
	r.f.mu.Lock()
	writes := r.f.writes
	r.f.mu.Unlock()
	if writes != 0 {
		t.Fatalf("%d WriteFile requests sent for an invalid --mode", writes)
	}
}

// TestFileWriteMode checks that file write rejects a zero --mode, which the
// SDK would turn into 0644, names --mode in the error, and sends every other
// valid mode, including the 0644 default, to the relay unchanged.
func TestFileWriteMode(t *testing.T) {
	for _, mode := range []string{"0", "0000", "00"} {
		r := newRunner(t)
		r.stdin = "x"
		res := r.run("file", "write", r.f.sb, "/tmp/x", "--mode", mode)
		if res.code != ExitUsage || !strings.Contains(res.stderr, "--mode") {
			t.Errorf("--mode %s: %+v", mode, res)
		}
		r.f.mu.Lock()
		writes := r.f.writes
		r.f.mu.Unlock()
		if writes != 0 {
			t.Errorf("--mode %s sent %d write requests", mode, writes)
		}
	}
	for _, c := range []struct {
		args []string
		want uint32
	}{
		{nil, 0o644},
		{[]string{"--mode", "0600"}, 0o600},
		{[]string{"--mode", "07777"}, 0o7777},
	} {
		r := newRunner(t)
		r.stdin = "x"
		res := r.run(append([]string{"file", "write", r.f.sb, "/tmp/x"}, c.args...)...)
		if res.code != ExitOK {
			t.Errorf("%v: %+v", c.args, res)
			continue
		}
		r.f.mu.Lock()
		modes := append([]uint32(nil), r.f.writeModes...)
		r.f.mu.Unlock()
		if len(modes) != 1 || modes[0] != c.want {
			t.Errorf("%v: finish modes %v, want [%#o]", c.args, modes, c.want)
		}
	}
}

// TestRootHelpPrintsUsage checks that every form of the root help prints the
// full command usage on standard output and exits with 0, also when the
// help flag comes before the command (the root flag set reads it there).
func TestRootHelpPrintsUsage(t *testing.T) {
	r := newRunner(t)
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"--no-color", "--help"}} {
		res := r.run(args...)
		if res.code != ExitOK {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, res.code, ExitOK, res.stderr)
		}
		if !strings.HasPrefix(res.stdout, "Usage: ownsand [global flags] <command>") || !strings.Contains(res.stdout, "sandbox ") {
			t.Errorf("%v: stdout %q, want the full usage", args, res.stdout)
		}
		if strings.Contains(res.stdout, "ownsand ownsand") {
			t.Errorf("%v: stdout repeats the program name: %q", args, res.stdout)
		}
	}
	// A subcommand help still lists the subcommand flags.
	if res := r.run("sandbox", "create", "--help"); res.code != ExitOK || !strings.HasPrefix(res.stdout, "Usage: ownsand sandbox create [flags]") {
		t.Errorf("sandbox create --help = (%d, %q)", res.code, res.stdout)
	}
}

func TestMissingOrganizationFailsBeforeRequest(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvOrganization)
	res := r.run("sandbox", "list")
	if res.code != ExitUsage || !strings.Contains(res.stderr, "organization") {
		t.Fatalf("%+v", res)
	}
	if len(r.f.callNames()) != 0 {
		t.Fatal("a request was sent")
	}
	// Organization-less commands ignore the missing organization.
	if res := r.run("org", "list"); res.code != 0 {
		t.Fatalf("org list: %+v", res)
	}
}

func TestExecExitStatus(t *testing.T) {
	r := newRunner(t)
	cases := []struct {
		cmd  string
		code int
		msg  string
	}{
		{"echo", 0, ""},
		{"exit3", 3, ""},
		{"exit300", 255, ""},
		{"sig9", 137, ""},
		{"timeout", 124, "timeout"},
		{"fail", 127, "could not start"},
		{"ended", 125, "max-ttl-expired"},
		{"trunc", 255, "stdout bytes 2 to 5"},
		{"denied", 255, "data path"},
	}
	for _, c := range cases {
		res := r.run("sandbox", "exec", r.f.sb, "--", c.cmd, "hello", "world")
		if res.code != c.code || !strings.Contains(res.stderr, c.msg) {
			t.Errorf("%s: exit %d stderr %q, want %d and %q", c.cmd, res.code, res.stderr, c.code, c.msg)
		}
	}
	res := r.run("sandbox", "exec", r.f.sb, "--", "echo", "hello", "world")
	if res.stdout != "hello world\n" || !strings.Contains(res.stderr, "err\xff\n") {
		t.Fatalf("streams: %+v", res)
	}
}

func TestExecJSONEvents(t *testing.T) {
	r := newRunner(t)
	res := r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "exit3")
	if res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if lines[0] == "" || !strings.Contains(lines[0], `"event":"started"`) || last["event"] != "exited" || last["exit_code"] != float64(3) || last["timed_out"] != false {
		t.Fatalf("events %q", res.stdout)
	}
	if _, ok := last["signal"]; ok {
		t.Fatal("exited carries both exit_code and signal")
	}
	res = r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "echo", "hi")
	if !strings.Contains(res.stdout, `"data":"err�\n"`) {
		t.Fatalf("invalid UTF-8 not replaced: %s", res.stdout)
	}
	res = r.run("--format", "json", "sandbox", "exec", "--binary", r.f.sb, "--", "echo", "hi")
	if !strings.Contains(res.stdout, `"data_base64":"aGkK"`) {
		t.Fatalf("binary: %s", res.stdout)
	}
	for _, c := range []struct {
		cmd  string
		code int
		ev   string
	}{{"fail", 0, "failed"}, {"ended", 0, "sandbox_ended"}, {"sig9", 0, "exited"}, {"trunc", 255, "output_truncated"}, {"denied", 255, "data_path_error"}} {
		res := r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", c.cmd)
		if res.code != c.code || !strings.Contains(res.stdout, `"event":"`+c.ev+`"`) {
			t.Errorf("%s: %+v", c.cmd, res)
		}
	}
}

func TestExecJSONDataPathErrorCarriesExecID(t *testing.T) {
	r := newRunner(t)
	res := r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "denied")
	if res.code != 255 {
		t.Fatalf("exit %d, want 255: %+v", res.code, res)
	}
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	r.f.mu.Lock()
	ids := append([]string(nil), r.f.execIDs...)
	r.f.mu.Unlock()
	if len(ids) == 0 || ids[0] == "" {
		t.Fatalf("the fake recorded no exec ID: %q", ids)
	}
	if last["event"] != "data_path_error" || last["exec_id"] != ids[len(ids)-1] || last["message"] == "" {
		t.Fatalf("last line %v, want data_path_error with exec_id %q", last, ids[len(ids)-1])
	}
	// Text mode is unchanged: no JSON on stdout.
	res = r.run("sandbox", "exec", r.f.sb, "--", "denied")
	if res.code != 255 || res.stdout != "" || !strings.Contains(res.stderr, "data path") {
		t.Fatalf("text mode: %+v", res)
	}
}

func TestDataPathErrorEventWithoutExecID(t *testing.T) {
	// An error before the data path is wrapped without an operation ID, so
	// the line omits exec_id.
	err := &ownsand.DataPathError{Operation: "exec", Err: errors.New("no credential")}
	var out bytes.Buffer
	if werr := writeJSON(&out, dataPathErrorEvent(err)); werr != nil {
		t.Fatal(werr)
	}
	var line map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &line); jerr != nil {
		t.Fatal(jerr)
	}
	if _, ok := line["exec_id"]; ok || line["event"] != "data_path_error" || line["message"] == "" {
		t.Fatalf("line %v", line)
	}
	with := dataPathErrorEvent(&ownsand.DataPathError{Operation: "exec", OperationID: "e1", Err: errors.New("x")})
	if with.ExecID != "e1" {
		t.Fatalf("exec ID %q", with.ExecID)
	}
}

func TestExecJSONDecodesUTF8AcrossChunks(t *testing.T) {
	r := newRunner(t)
	lines := func(res result) []string {
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
			var v map[string]any
			if err := json.Unmarshal([]byte(l), &v); err != nil {
				t.Fatalf("%q: %v", l, err)
			}
			switch v["event"] {
			case "stdout", "stderr":
				out = append(out, fmt.Sprintf("%s@%v:%s", v["event"], v["offset"], v["data"]))
			default:
				out = append(out, v["event"].(string))
			}
		}
		return out
	}
	res := r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "split")
	want := []string{"started", "stdout@0:h", "stdout@1:é ", "stdout@4:€", "stderr@0:x", "stderr@1:\ufffd", "exited"}
	if got := lines(res); res.code != 0 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("split: %d %q, want %q", res.code, got, want)
	}
	res = r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "splitgap")
	want = []string{"started", "stdout@0:a", "stdout@1:\ufffd", "output_truncated", "stdout@5:\ufffdz", "exited"}
	if got := lines(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("splitgap: %q, want %q", got, want)
	}
}

func TestSecretsPrintedOnceOnStdout(t *testing.T) {
	r := newRunner(t)
	res := r.run("api-key", "create", "--name", "ci")
	if res.code != 0 || res.stdout != "osk_newsecretvalue\n" || !strings.Contains(res.stderr, "osk_new") || strings.Contains(res.stderr, "osk_newsecretvalue") {
		t.Fatalf("%+v", res)
	}
	res = r.run("member", "invite", "--role", "admin")
	if res.code != 0 || res.stdout != "osi_abcdefsecret\n" {
		t.Fatalf("%+v", res)
	}
	res = r.run("--format", "json", "api-key", "create", "--name", "ci")
	var v map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &v); err != nil || v["api_key_secret"] != "osk_newsecretvalue" || v["name"] != "ci" {
		t.Fatalf("json: %q %v", res.stdout, err)
	}
	if strings.Count(strings.TrimSpace(res.stdout), "\n") != 0 {
		t.Fatal("JSON secret output must be one object")
	}
}

func TestAPIKeyTakesPrecedenceOverStoredTokens(t *testing.T) {
	r := newRunner(t)
	st := credstore.Store{Dir: r.env[EnvConfigDir]}
	if err := st.Save(&credstore.Credentials{RefreshToken: "r", AccessToken: "stored-access-token", AccessTokenExpiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res := r.run("org", "get"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	if got := r.f.lastAuth(); got != "Bearer osk_testkey" {
		t.Fatalf("Authorization %q", got)
	}
	delete(r.env, ownsand.EnvAPIKey)
	if res := r.run("org", "get"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	if got := r.f.lastAuth(); got != "Bearer stored-access-token" {
		t.Fatalf("Authorization %q", got)
	}
	_ = st.Delete()
	res := r.run("org", "get")
	if res.code != ExitError || !strings.Contains(res.stderr, "ownsand login") {
		t.Fatalf("%+v", res)
	}
}

// fakeLogto serves the device flow and the refresh token grant.
type fakeLogto struct {
	mu        sync.Mutex
	polls     int
	refreshes []string
}

func (l *fakeLogto) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case r.URL.Path == "/oidc/device/auth":
			if r.Form.Get("client_id") != "app" || !strings.Contains(r.Form.Get("scope"), "offline_access") || !slices.Contains(strings.Fields(r.Form.Get("scope")), RequiredLogtoScope) || r.Form.Get("resource") != DefaultLogtoResource {
				t.Errorf("device auth form %v", r.Form)
			}
			fmt.Fprint(w, `{"device_code":"dev-secret","user_code":"ABCD-EFGH","verification_uri":"https://login.example/device","expires_in":60,"interval":0}`)
		case r.URL.Path == "/oidc/token" && r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code":
			l.polls++
			if l.polls < 3 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"opaque","refresh_token":"refresh-1","expires_in":3600}`)
		case r.URL.Path == "/oidc/token" && r.Form.Get("grant_type") == "refresh_token":
			l.refreshes = append(l.refreshes, r.Form.Get("refresh_token"))
			if r.Form.Get("resource") != DefaultLogtoResource {
				t.Errorf("refresh without the resource: %v", r.Form)
			}
			n := len(l.refreshes)
			fmt.Fprintf(w, `{"access_token":"jwt-%d","refresh_token":"refresh-%d","expires_in":1}`, n, n+1)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestLoginStoresUserOnlyCredentialsAndLogout(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	lg := &fakeLogto{}
	ls := httptest.NewServer(lg.handler(t))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"
	res := r.run("login")
	if res.code != 0 {
		t.Fatalf("login: %+v", res)
	}
	if !strings.Contains(res.stderr, "https://login.example/device") || !strings.Contains(res.stderr, "ABCD-EFGH") {
		t.Fatalf("login output %q", res.stderr)
	}
	for _, secret := range []string{"dev-secret", "refresh-", "jwt-"} {
		if strings.Contains(res.stdout+res.stderr, secret) {
			t.Fatalf("login printed a secret: %q", res.stdout+res.stderr)
		}
	}
	path := filepath.Join(r.env[EnvConfigDir], credstore.FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential file mode %v", info.Mode().Perm())
		}
		dir, _ := os.Stat(r.env[EnvConfigDir])
		if dir.Mode().Perm() != 0o700 {
			t.Fatalf("credential directory mode %v", dir.Mode().Perm())
		}
	}
	// The stored access token expires in one second, so the next command
	// refreshes it and stores the rotated refresh token.
	if res := r.run("org", "get"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	if got := r.f.lastAuth(); got != "Bearer jwt-2" {
		t.Fatalf("Authorization %q", got)
	}
	creds, err := credstore.Store{Dir: r.env[EnvConfigDir]}.Load()
	if err != nil || creds.RefreshToken != "refresh-3" {
		t.Fatalf("rotated refresh token not stored: %v", err)
	}
	if res := r.run("logout"); res.code != 0 {
		t.Fatalf("logout: %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout left the credential file")
	}
}

// login and logout take no operands, also none after the "--" terminator. A
// rejected logout must leave the stored credentials untouched and a rejected
// login must not send a request to Logto.
func TestLoginAndLogoutRejectOperands(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	var requests atomic.Int32
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	r.env[EnvLogtoAppID] = "app"
	st := credstore.Store{Dir: r.env[EnvConfigDir]}
	if err := st.Save(&credstore.Credentials{SessionID: "s", LogtoEndpoint: ls.URL, AppID: "app", RefreshToken: "keep"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.env[EnvConfigDir], credstore.FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"logout", "extra"},
		{"logout", "--", "extra"},
		{"login", "extra"},
		{"login", "--", "extra"},
	} {
		res := r.run(args...)
		if res.code != ExitUsage || !strings.Contains(res.stderr, "usage: ownsand "+args[0]) {
			t.Errorf("%v = %+v, want a usage error", args, res)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Errorf("%v changed the credential file: %v", args, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("a rejected command sent %d request(s) to Logto", n)
	}
	for _, name := range []string{"login", "logout"} {
		if res := r.run(name, "--help"); res.code != ExitOK || !strings.HasPrefix(res.stdout, "Usage: ownsand "+name) {
			t.Errorf("%s --help = (%d, %q)", name, res.code, res.stdout)
		}
	}
	// A valid logout still works.
	if res := r.run("logout"); res.code != ExitOK {
		t.Fatalf("logout: %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("logout left the credential file")
	}
}

// A device code that expires while the user has not signed in ends the login
// with the expiry message at the expiry time, not after the polling interval,
// and without a token request after that point.
func TestLoginReportsExpiryWithoutLateTokenRequest(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	r.env[EnvLogtoAppID] = "app"
	var tokenRequests atomic.Int32
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/oidc/device/auth":
			// A long interval and a lifetime of one second.
			fmt.Fprint(w, `{"device_code":"dev-secret","user_code":"ABCD-EFGH","verification_uri":"https://login.example/device","expires_in":1,"interval":5}`)
		case "/oidc/token":
			tokenRequests.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ls.Close()
	r.env[EnvLogtoEndpoint] = ls.URL
	start := time.Now()
	res := r.run("login")
	elapsed := time.Since(start)
	if res.code != ExitError || !strings.Contains(res.stderr, logto.ErrExpired.Error()) {
		t.Fatalf("%+v", res)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("login took %v, want about 1s", elapsed)
	}
	if n := tokenRequests.Load(); n != 0 {
		t.Fatalf("%d token requests after the code expired", n)
	}
}

func TestStoredCredentialsRejectChangedLogtoConfiguration(t *testing.T) {
	r := newRunner(t)
	delete(r.env, ownsand.EnvAPIKey)
	lg := &fakeLogto{}
	ls := httptest.NewServer(lg.handler(t))
	defer ls.Close()
	st := credstore.Store{Dir: r.env[EnvConfigDir]}
	if err := st.Save(&credstore.Credentials{LogtoEndpoint: ls.URL, AppID: "app", Resource: DefaultLogtoResource, RefreshToken: "old-refresh", AccessToken: "stored-access-token", AccessTokenExpiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Matching or unset configuration uses the stored tokens; a trailing
	// slash on the endpoint does not change the tenant.
	r.env[EnvLogtoEndpoint] = ls.URL + "/"
	r.env[EnvLogtoAppID] = "app"
	if res := r.run("org", "get"); res.code != 0 || r.f.lastAuth() != "Bearer stored-access-token" {
		t.Fatalf("%+v %q", res, r.f.lastAuth())
	}
	for _, c := range []struct{ key, value string }{
		{EnvLogtoEndpoint, "https://other-tenant.example"},
		{EnvLogtoAppID, "other-app"},
		{EnvLogtoResource, "https://other.example"},
	} {
		prev, had := r.env[c.key]
		r.env[c.key] = c.value
		before := len(r.f.callNames())
		res := r.run("org", "get")
		if res.code != ExitError || !strings.Contains(res.stderr, c.key) || !strings.Contains(res.stderr, "ownsand login") {
			t.Errorf("%s: %+v", c.key, res)
		}
		if len(r.f.callNames()) != before {
			t.Errorf("%s: the CLI sent a request with mismatched credentials", c.key)
		}
		if had {
			r.env[c.key] = prev
		} else {
			delete(r.env, c.key)
		}
	}
	if len(lg.refreshes) != 0 {
		t.Fatalf("refresh token sent: %v", lg.refreshes)
	}
}

func TestLoginWithoutConfiguration(t *testing.T) {
	r := newRunner(t)
	res := r.run("login")
	if res.code != ExitError || !strings.Contains(res.stderr, EnvLogtoEndpoint) {
		t.Fatalf("%+v", res)
	}
}

func TestMemberAcceptReadsSecretFromStdinOrFile(t *testing.T) {
	r := newRunner(t)
	r.stdin = "osi_from_stdin\n"
	if res := r.run("member", "accept"); res.code != 0 {
		t.Fatalf("stdin: %+v", res)
	}
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("osi_from_stdin"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.stdin = ""
	if res := r.run("member", "accept", "--secret-file", file); res.code != 0 {
		t.Fatalf("file: %+v", res)
	}
}

func TestDeviceSetLimitsKeepsCurrentOvercommit(t *testing.T) {
	r := newRunner(t)
	if res := r.run("device", "set-limits", r.f.dev, "--vcpu", "8", "--memory", "16384", "--disk", "200"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	r.f.mu.Lock()
	l := r.f.limit
	r.f.mu.Unlock()
	if l.GetVcpuCount() != 8 || l.GetMemoryMib() != 16384 || l.GetDiskGib() != 200 || l.GetVcpuOvercommitPercent() != 150 {
		t.Fatalf("limits %v", l)
	}
	for _, c := range r.f.callNames() {
		if c == "UpdateDeviceLimits" {
			t.Fatal("device set-limits called the legacy UpdateDeviceLimits")
		}
	}
	if res := r.run("device", "set-limits", r.f.dev, "--vcpu", "8", "--memory", "1", "--disk", "1", "--vcpu-overcommit", "110"); res.code != ExitUsage {
		t.Fatalf("overcommit not in steps of 25 accepted: %+v", res)
	}
}

// TestDeviceSetLimitsOvercommitFlag checks that only an absent
// --vcpu-overcommit falls back to the device's current value, that every
// explicit value is validated before any RPC, and that an explicit valid value
// is sent without a preceding GetDevice.
func TestDeviceSetLimitsOvercommitFlag(t *testing.T) {
	base := []string{"--vcpu", "8", "--memory", "16384", "--disk", "200"}
	for _, tc := range []struct {
		name      string
		flag      []string
		wantCode  int
		wantCalls []string
		wantPct   int32
	}{
		{name: "absent keeps the current value", wantCalls: []string{"GetDevice", "ReplaceDeviceLimits"}, wantPct: 150},
		{name: "explicit 100", flag: []string{"--vcpu-overcommit", "100"}, wantCalls: []string{"ReplaceDeviceLimits"}, wantPct: 100},
		{name: "explicit 400", flag: []string{"--vcpu-overcommit=400"}, wantCalls: []string{"ReplaceDeviceLimits"}, wantPct: 400},
		{name: "explicit 110", flag: []string{"--vcpu-overcommit", "110"}, wantCode: ExitUsage},
		{name: "explicit -1", flag: []string{"--vcpu-overcommit", "-1"}, wantCode: ExitUsage},
		{name: "explicit -25", flag: []string{"--vcpu-overcommit=-25"}, wantCode: ExitUsage},
		{name: "explicit minimum int32", flag: []string{"--vcpu-overcommit", "-2147483648"}, wantCode: ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(t)
			args := append([]string{"device", "set-limits", r.f.dev}, base...)
			res := r.run(append(args, tc.flag...)...)
			if res.code != tc.wantCode {
				t.Fatalf("exit code %d, want %d: %+v", res.code, tc.wantCode, res)
			}
			calls := r.f.callNames()
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("calls %v, want %v", calls, tc.wantCalls)
			}
			for i := range calls {
				if calls[i] != tc.wantCalls[i] {
					t.Fatalf("calls %v, want %v", calls, tc.wantCalls)
				}
			}
			r.f.mu.Lock()
			l := r.f.limit
			r.f.mu.Unlock()
			if tc.wantCode != 0 {
				if !strings.Contains(res.stderr, "--vcpu-overcommit") || !strings.Contains(res.stderr, "limits.vcpu_overcommit_percent") || !strings.Contains(res.stderr, "100 to 400 in steps of 25") {
					t.Fatalf("stderr does not name the option, the field and the allowed range: %q", res.stderr)
				}
				if l != nil {
					t.Fatalf("limits were sent: %v", l)
				}
				return
			}
			if l.GetVcpuCount() != 8 || l.GetMemoryMib() != 16384 || l.GetDiskGib() != 200 || l.VcpuOvercommitPercent == nil || l.GetVcpuOvercommitPercent() != tc.wantPct {
				t.Fatalf("limits %v, want overcommit %d", l, tc.wantPct)
			}
		})
	}
}

// TestDeviceSetLimitsSendsExplicitZero checks that a zero flag value is sent
// as a present zero, which the strict RPC accepts, and that every field of
// the request is present.
func TestDeviceSetLimitsSendsExplicitZero(t *testing.T) {
	r := newRunner(t)
	if res := r.run("device", "set-limits", r.f.dev, "--vcpu", "0", "--memory", "0", "--disk", "0", "--vcpu-overcommit", "100"); res.code != 0 {
		t.Fatalf("%+v", res)
	}
	r.f.mu.Lock()
	l := r.f.limit
	r.f.mu.Unlock()
	if l.VcpuCount == nil || l.MemoryMib == nil || l.DiskGib == nil || l.VcpuOvercommitPercent == nil {
		t.Fatalf("a field is missing in %v", l)
	}
	if l.GetVcpuCount() != 0 || l.GetMemoryMib() != 0 || l.GetDiskGib() != 0 || l.GetVcpuOvercommitPercent() != 100 {
		t.Fatalf("limits %v", l)
	}
}

func TestSandboxCreateWaitsAndSendsFlags(t *testing.T) {
	r := newRunner(t)
	res := r.run("--format", "json", "sandbox", "create", "--image", "linux", "--idle-ttl", "30m", "--max-ttl", "7d",
		"--vcpu", "2", "--memory", "2048", "--disk", "20", "--network", "none", "--os", "linux", "--arch", "amd64", "--label", "team=ci")
	if res.code != 0 || !strings.Contains(res.stdout, `"state":"SANDBOX_STATE_RUNNING"`) {
		t.Fatalf("%+v", res)
	}
	r.f.mu.Lock()
	req := r.f.creat[0]
	r.f.mu.Unlock()
	if req.GetMaxTtl().AsDuration() != 7*24*time.Hour || req.GetNetwork().GetMode().String() != "NETWORK_MODE_NONE" || req.GetLabels()["team"] != "ci" || len(req.GetIdempotencyKey()) != 36 {
		t.Fatalf("request %v", req)
	}
}

func TestFileReadWrite(t *testing.T) {
	r := newRunner(t)
	r.stdin = "file contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	res := r.run("--format", "json", "file", "read", r.f.sb, "/tmp/x")
	if res.code != 0 || res.stdout != "file contents" {
		t.Fatalf("read: %+v", res)
	}
	to := filepath.Join(t.TempDir(), "out")
	if res := r.run("file", "read", "--to", to, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	if b, _ := os.ReadFile(to); string(b) != "file contents" {
		t.Fatalf("--to content %q", b)
	}
	if res := r.run("file", "read", r.f.sb, "/missing"); res.code != ExitError || !strings.Contains(res.stderr, "not_found") {
		t.Fatalf("missing: %+v", res)
	}
}

func TestFileReadFailureKeepsDestination(t *testing.T) {
	r := newRunner(t)
	dir := t.TempDir()
	to := filepath.Join(dir, "out")
	if err := os.WriteFile(to, []byte("previous contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	if res := r.run("file", "read", "--to", to, r.f.sb, "/missing"); res.code != ExitError || !strings.Contains(res.stderr, "not_found") {
		t.Fatalf("missing: %+v", res)
	}
	if b, err := os.ReadFile(to); err != nil || string(b) != "previous contents" {
		t.Fatalf("a failed read changed the destination: %q %v", b, err)
	}
	r.stdin = "new contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(to, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	if b, _ := os.ReadFile(to); string(b) != "new contents" {
		t.Fatalf("--to content %q", b)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the download replaced the symbolic link")
	}
	if info, _ := os.Stat(to); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("the download changed the mode to %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestFileReadDanglingSymlink(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the download replaced the dangling symbolic link")
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "downloaded contents" {
		t.Fatalf("target content %q %v", b, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}

	// A relative link in a chain resolves against the directory of the link.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(sub, "inner")
	if err := os.Symlink(filepath.Join("..", "chain-target"), inner); err != nil {
		t.Skipf("symlink: %v", err)
	}
	outer := filepath.Join(dir, "outer")
	if err := os.Symlink(inner, outer); err != nil {
		t.Fatal(err)
	}
	if res := r.run("file", "read", "--to", outer, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to chain: %+v", res)
	}
	for _, l := range []string{outer, inner} {
		if info, err := os.Lstat(l); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the download replaced the symbolic link %s", l)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "chain-target")); err != nil || string(b) != "downloaded contents" {
		t.Fatalf("chain target content %q %v", b, err)
	}
}

func TestFileReadDanglingSymlinkThroughSymlinkedDirectory(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	// alias -> real/sub and real/sub/link -> ../target: the target of link is
	// real/target. A lexical join would pick the unrelated file "target" that
	// lies beside alias.
	dir := t.TempDir()
	sub := filepath.Join(dir, "real", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("real", "sub"), filepath.Join(dir, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "target"), filepath.Join(sub, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	unrelated := filepath.Join(dir, "target")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := r.run("file", "read", "--to", filepath.Join(dir, "alias", "link"), r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	if b, err := os.ReadFile(unrelated); err != nil || string(b) != "unrelated" {
		t.Fatalf("the download changed an unrelated file: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "real", "target")); err != nil || string(b) != "downloaded contents" {
		t.Fatalf("target content %q %v", b, err)
	}
	if info, err := os.Lstat(filepath.Join(sub, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the download replaced the symbolic link")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "real")); len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestFileReadDanglingSymlinkMissingDirectory(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing", "target"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code == 0 {
		t.Fatalf("read --to a link into a missing directory succeeded: %+v", res)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the failed download replaced the symbolic link")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

func TestFileReadDanglingSymlinkDotDotAfterSymlink(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	// alias -> real/sub and link -> alias/../out.txt: the kernel resolves
	// alias first and applies ".." to real/sub, so the target of link is
	// real/out.txt. A lexical clean would pick the unrelated path "out.txt"
	// beside link.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("real", "sub"), filepath.Join(dir, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	link := filepath.Join(dir, "link")
	// Build the target without filepath.Join, which would clean it.
	if err := os.Symlink("alias"+string(filepath.Separator)+".."+string(filepath.Separator)+"out.txt", link); err != nil {
		t.Fatal(err)
	}
	if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("read --to: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "real", "out.txt")); err != nil || string(b) != "downloaded contents" {
		t.Fatalf("target content %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the download created the lexically cleaned path: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the download replaced the symbolic link")
	}
	for _, d := range []string{dir, filepath.Join(dir, "real")} {
		for _, e := range mustReadDir(t, d) {
			if strings.HasSuffix(e, ".tmp") {
				t.Fatalf("temporary file left in %s: %s", d, e)
			}
		}
	}
}

func TestFileReadDanglingSymlinkDotDotAfterMissingDirectory(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	// missing does not exist, so the kernel fails the lookup of
	// missing/../out.txt. A lexical clean would turn it into out.txt beside
	// the link and create that file. Windows path handling cleans the path
	// that way, so the CLI must reject the missing directory itself.
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	// Build the target without filepath.Join, which would clean it.
	if err := os.Symlink("missing"+string(filepath.Separator)+".."+string(filepath.Separator)+"out.txt", link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := resolveMissingTarget(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve a link through a missing directory: %v, want an error that wraps os.ErrNotExist", err)
	}
	if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code == 0 {
		t.Fatalf("read --to a link through a missing directory succeeded: %+v", res)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the failed download replaced the symbolic link")
	}
	if entries := mustReadDir(t, dir); len(entries) != 1 {
		t.Fatalf("the failed download changed the directory: %v", entries)
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFileReadSymlinkLoop(t *testing.T) {
	r := newRunner(t)
	r.stdin = "downloaded contents"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if res := r.run("file", "read", "--to", a, r.f.sb, "/tmp/x"); res.code == 0 {
		t.Fatalf("read --to a link loop succeeded: %+v", res)
	}
	if info, err := os.Lstat(a); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the failed download replaced the symbolic link")
	}
}

func TestListOutputAndColor(t *testing.T) {
	r := newRunner(t)
	res := r.run("api-key", "list")
	if res.code != 0 || !strings.Contains(res.stdout, "ci") || !strings.Contains(res.stderr, "--page-token next") {
		t.Fatalf("%+v", res)
	}
	res = r.run("sandbox", "get", "bad")
	if !strings.Contains(res.stderr, ansiRed) {
		t.Fatalf("color is on by default: %q", res.stderr)
	}
	res = r.run("--no-color", "sandbox", "get", "bad")
	if strings.Contains(res.stderr, "\x1b[") {
		t.Fatalf("--no-color: %q", res.stderr)
	}
	r.env[EnvNoColor] = "1"
	res = r.run("sandbox", "get", "bad")
	if strings.Contains(res.stderr, "\x1b[") {
		t.Fatalf("NO_COLOR: %q", res.stderr)
	}
	// An empty NO_COLOR counts as unset, so color stays on.
	r.env[EnvNoColor] = ""
	res = r.run("sandbox", "get", "bad")
	if !strings.Contains(res.stderr, ansiRed) {
		t.Fatalf("an empty NO_COLOR must keep color: %q", res.stderr)
	}
	// --no-color still wins over an empty NO_COLOR.
	res = r.run("--no-color", "sandbox", "get", "bad")
	if strings.Contains(res.stderr, "\x1b[") {
		t.Fatalf("--no-color with an empty NO_COLOR: %q", res.stderr)
	}
}

func TestVerboseLogsNeverContainSecretsOrContent(t *testing.T) {
	r := newRunner(t)
	res := r.run("--verbose", "--no-color", "sandbox", "exec", r.f.sb, "--", "echo", "topsecret-arg")
	if res.code != 0 || !strings.Contains(res.stderr, "ownsand rpc") {
		t.Fatalf("%+v", res)
	}
	logs := strings.ReplaceAll(res.stderr, "err\xff\n", "")
	for _, s := range []string{"osk_testkey", "topsecret-arg"} {
		if strings.Contains(logs, s) {
			t.Fatalf("verbose logs contain %q: %s", s, logs)
		}
	}
}

func TestExecExitStatusTable(t *testing.T) {
	code := func(v int64) *int64 { return &v }
	sig := func(v int32) *int32 { return &v }
	cases := []struct {
		res  *ownsand.ExecResult
		err  error
		want int
	}{
		{&ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: code(0)}, nil, 0},
		{&ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: code(-1)}, nil, 255},
		{&ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: code(3221225477)}, nil, 255},
		{&ownsand.ExecResult{Kind: ownsand.ExecResultExited, Signal: sig(200)}, nil, 255},
		{&ownsand.ExecResult{Kind: ownsand.ExecResultExited, Signal: sig(15), TimedOut: true}, nil, 124},
		{&ownsand.ExecResult{Kind: ownsand.ExecResultSandboxEnded, Incomplete: true}, nil, 255},
		{nil, io.ErrUnexpectedEOF, 255},
	}
	for i, c := range cases {
		if got := execExitStatus(c.res, c.err); got != c.want {
			t.Errorf("case %d: %d, want %d", i, got, c.want)
		}
	}
}

func TestExecArgumentForms(t *testing.T) {
	r := newRunner(t)
	for _, args := range [][]string{
		{"sandbox", "exec", r.f.sb, "--", "echo", "-n", "x"},
		{"sandbox", "exec", "--timeout", "1m", r.f.sb, "echo", "-n", "x"},
		{"sandbox", "exec", r.f.sb, "--env", "A=B", "echo", "-n", "x"},
		{"--org", r.f.org, "sandbox", "exec", r.f.sb, "--cwd", "/", "--", "echo", "-n", "x"},
	} {
		res := r.run(args...)
		if res.code != 0 || res.stdout != "-n x\n" {
			t.Errorf("%v: %+v", args, res)
		}
	}
}

func TestLogtoScopesAlwaysIncludeRequiredScope(t *testing.T) {
	if got := logtoScopes(""); !slices.Equal(got, []string{RequiredLogtoScope}) {
		t.Fatalf("logtoScopes(\"\") = %v", got)
	}
	if got := logtoScopes(" extra:one  extra:two "); !slices.Equal(got, []string{RequiredLogtoScope, "extra:one", "extra:two"}) {
		t.Fatalf("logtoScopes(extra) = %v", got)
	}
}

// TestFileReadLongDestinationName checks that the staging file name does not
// depend on the destination name, so names at the filesystem limit work.
func TestFileReadLongDestinationName(t *testing.T) {
	probe := t.TempDir()
	if f, err := os.Create(filepath.Join(probe, strings.Repeat("p", 255))); err != nil {
		t.Skipf("the temporary directory cannot hold a 255-byte name: %v", err)
	} else {
		_ = f.Close()
	}
	r := newRunner(t)
	r.stdin = "sandbox bytes"
	if res := r.run("file", "write", r.f.sb, "/tmp/x"); res.code != 0 {
		t.Fatalf("write: %+v", res)
	}
	names := map[string]string{
		"255":       strings.Repeat("a", 255),
		"234":       strings.Repeat("b", 234),
		"multibyte": strings.Repeat("é", 120), // 240 bytes
		"multi238":  strings.Repeat("é", 119), // 238 bytes
	}
	for label, name := range names {
		t.Run(label+"/new", func(t *testing.T) {
			dir := t.TempDir()
			to := filepath.Join(dir, name)
			if res := r.run("file", "read", "--to", to, r.f.sb, "/tmp/x"); res.code != 0 {
				t.Fatalf("read --to: %+v", res)
			}
			if b, _ := os.ReadFile(to); string(b) != "sandbox bytes" {
				t.Fatalf("content %q", b)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Fatalf("temporary files left: %v", entries)
			}
		})
		t.Run(label+"/existing", func(t *testing.T) {
			dir := t.TempDir()
			to := filepath.Join(dir, name)
			if err := os.WriteFile(to, []byte("previous"), 0o640); err != nil {
				t.Fatal(err)
			}
			if res := r.run("file", "read", "--to", to, r.f.sb, "/missing"); res.code != ExitError {
				t.Fatalf("missing: %+v", res)
			}
			if b, _ := os.ReadFile(to); string(b) != "previous" {
				t.Fatalf("a failed read changed the destination: %q", b)
			}
			if res := r.run("file", "read", "--to", to, r.f.sb, "/tmp/x"); res.code != 0 {
				t.Fatalf("read --to: %+v", res)
			}
			if b, _ := os.ReadFile(to); string(b) != "sandbox bytes" {
				t.Fatalf("content %q", b)
			}
			if info, _ := os.Stat(to); runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
				t.Fatalf("mode %v", info.Mode().Perm())
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Fatalf("temporary files left: %v", entries)
			}
		})
	}
	t.Run("link to long target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, strings.Repeat("t", 255))
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if res := r.run("file", "read", "--to", link, r.f.sb, "/tmp/x"); res.code != 0 {
			t.Fatalf("read --to: %+v", res)
		}
		if b, _ := os.ReadFile(target); string(b) != "sandbox bytes" {
			t.Fatalf("content %q", b)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the download replaced the symbolic link")
		}
	})
	t.Run("name over the limit", func(t *testing.T) {
		// The operating systems report an over-limit name with different
		// errors (ENAMETOOLONG on Linux and macOS, ERROR_INVALID_NAME on
		// Windows), so expect the error that the platform reports when the
		// test creates such a file itself.
		over := strings.Repeat("a", 256)
		var want *os.PathError
		if f, err := os.Create(filepath.Join(probe, over)); err == nil {
			_ = f.Close()
			t.Skip("the temporary directory accepts a 256-byte name")
		} else if !errors.As(err, &want) {
			t.Fatalf("create an over-limit name: %v", err)
		}
		dir := t.TempDir()
		res := r.run("file", "read", "--to", filepath.Join(dir, over), r.f.sb, "/tmp/x")
		if res.code != ExitError || !strings.Contains(res.stderr, want.Err.Error()) {
			t.Fatalf("over limit: %+v, want an error that contains %q", res, want.Err.Error())
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("files created: %v", entries)
		}
	})
}

// TestOutOfRangeInt32FlagsAreUsageErrors covers values that a 64-bit int flag
// would accept and the int32 request field would silently wrap (4294967297
// becomes 1).
func TestOutOfRangeInt32FlagsAreUsageErrors(t *testing.T) {
	const wrapping = "4294967297"
	const firstOverflow = "2147483648"
	createBase := []string{"sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "1h", "--memory", "512", "--disk", "5"}
	for _, c := range []struct {
		flag string
		args []string
	}{
		{"vcpu", append(slices.Clone(createBase), "--vcpu", wrapping)},
		{"vcpu", append(slices.Clone(createBase), "--vcpu", firstOverflow)},
		{"vcpu", []string{"device", "set-limits", "01900000-0000-7000-8000-000000000001", "--memory", "1", "--disk", "1", "--vcpu", wrapping}},
		{"vcpu-overcommit", []string{"device", "set-limits", "01900000-0000-7000-8000-000000000001", "--memory", "1", "--disk", "1", "--vcpu", "1", "--vcpu-overcommit", wrapping}},
		{"vcpu-overcommit", []string{"device", "set-limits", "01900000-0000-7000-8000-000000000001", "--memory", "1", "--disk", "1", "--vcpu", "1", "--vcpu-overcommit", "-2147483649"}},
		{"page-size", []string{"org", "list", "--page-size", wrapping}},
		{"page-size", []string{"member", "list", "--page-size", firstOverflow}},
		{"page-size", []string{"member", "invitations", "--page-size", wrapping}},
		{"page-size", []string{"device", "list", "--page-size", wrapping}},
		{"page-size", []string{"enrollment-token", "list", "--page-size", wrapping}},
		{"page-size", []string{"api-key", "list", "--page-size", wrapping}},
		{"page-size", []string{"sandbox", "list", "--page-size", wrapping}},
		{"page-size", []string{"snapshot", "list", "--page-size", wrapping}},
		{"page-size", []string{"template", "list", "--page-size", wrapping}},
	} {
		r := newRunner(t)
		res := r.run(c.args...)
		if res.code != ExitUsage || !strings.Contains(res.stderr, "-"+c.flag) || !strings.Contains(res.stderr, "out of range") {
			t.Errorf("%v: %+v", c.args, res)
		}
		if calls := r.f.callNames(); len(calls) != 0 {
			t.Errorf("%v: sent %v for an out-of-range value", c.args, calls)
		}
	}
}

func TestInt32FlagBoundariesAreAccepted(t *testing.T) {
	r := newRunner(t)
	if res := r.run("org", "list", "--page-size", "2147483647"); res.code != 0 {
		t.Fatalf("largest page size: %+v", res)
	}
	if res := r.run("sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "1h", "--vcpu", "2147483647", "--memory", "512", "--disk", "5"); res.code != ExitUsage && res.code != 0 {
		// The server (or SDK validation) may reject the huge count, but the
		// value must reach the request unchanged and not wrap.
		t.Logf("create with the largest vCPU: %+v", res)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for _, req := range r.f.creat {
		if req.GetResources().GetVcpuCount() != 2147483647 {
			t.Fatalf("vCPU count %d, want 2147483647", req.GetResources().GetVcpuCount())
		}
	}
}

func TestParseDurationDayOverflow(t *testing.T) {
	const maxDays = 106751 // floor(MaxInt64 / 24h)
	if d, err := parseDuration("max-ttl", "7d"); err != nil || d != 7*24*time.Hour {
		t.Fatalf("7d: %v, %v", d, err)
	}
	if d, err := parseDuration("max-ttl", "106751d"); err != nil || d != maxDays*24*time.Hour || d <= 0 {
		t.Fatalf("largest day count: %v, %v", d, err)
	}
	for _, s := range []string{"106752d", "213504d", "9223372036854775807d", "-106752d", "-9223372036854775808d"} {
		if d, err := parseDuration("max-ttl", s); err == nil || !strings.Contains(err.Error(), "--max-ttl") {
			t.Errorf("%s: %v, %v", s, d, err)
		}
	}
	r := newRunner(t)
	res := r.run("sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "213504d", "--vcpu", "1", "--memory", "512", "--disk", "5")
	if res.code != ExitUsage || !strings.Contains(res.stderr, "invalid duration") || len(r.f.callNames()) != 0 {
		t.Fatalf("%+v %v", res, r.f.callNames())
	}
}

// TestDeviceSetLimitsPrintsLargeIntegersInFull checks that text mode prints
// the largest int32 value in decimal form, and that JSON mode keeps it too.
func TestDeviceSetLimitsPrintsLargeIntegersInFull(t *testing.T) {
	r := newRunner(t)
	args := []string{"device", "set-limits", r.f.dev, "--vcpu", "2147483647", "--memory", "1", "--disk", "1", "--vcpu-overcommit", "100"}
	res := r.run(append(args, "--no-color")...)
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"limits.vcpu_count: 2147483647\n", "effective_vcpu_limit: 2147483647\n"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout misses %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "e+") {
		t.Errorf("exponent form in stdout:\n%s", res.stdout)
	}
	res = r.run(append(args, "--format", "json")...)
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{`"vcpu_count":2147483647`, `"effective_vcpu_limit":2147483647`} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("JSON stdout misses %q:\n%s", want, res.stdout)
		}
	}
}
