package cli

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cptest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime/runtimetest"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
)

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, stdin string, env map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(Env{
		Args:    args,
		Stdin:   strings.NewReader(stdin),
		Stdout:  &out,
		Stderr:  &errb,
		Getenv:  func(k string) string { return env[k] },
		Version: "1.0.0",
		NewRuntime: func(runtime.Config) (runtime.Runtime, error) {
			return runtimetest.New(t.TempDir()), nil
		},
	})
	return result{code, out.String(), errb.String()}
}

func TestEnrollFromStdinAndRetryKeepsKey(t *testing.T) {
	cp := cptest.New(t, nil)
	cp.AddToken("ose_token")
	dir := t.TempDir()
	r := runCLI(t, "ose_token\n", nil, "--state-dir", dir, "enroll", "--origin", cp.URL)
	if r.code != 0 {
		t.Fatalf("enroll failed: %s", r.stderr)
	}
	if !strings.Contains(r.stdout, "\x1b[32m") {
		t.Fatal("output has no color by default")
	}
	id, err := (&state.Dir{Path: dir}).LoadIdentity()
	if err != nil || !id.Enrolled() {
		t.Fatalf("identity not saved: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "identity.json")); goruntime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode %v", fi.Mode())
	}
	r = runCLI(t, "ose_token\n", nil, "--state-dir", dir, "enroll", "--origin", cp.URL)
	if r.code == 0 || !strings.Contains(r.stderr, "already enrolled") {
		t.Fatalf("second enroll: %d %s", r.code, r.stderr)
	}
}

func TestEnrollRetryAfterLostResponseReusesPendingKey(t *testing.T) {
	cp := cptest.New(t, nil)
	cp.AddToken("ose_token")
	dir := t.TempDir()
	r := runCLI(t, "ose_token\n", nil, "--state-dir", dir, "enroll", "--origin", cp.URL)
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	// Simulate a lost response: the device ID was never saved.
	d := &state.Dir{Path: dir}
	id, _ := d.LoadIdentity()
	want := id.DeviceID
	id.DeviceID = ""
	_ = d.SaveIdentity(id)
	r = runCLI(t, "ose_token\n", nil, "--state-dir", dir, "enroll", "--origin", cp.URL)
	if r.code != 0 {
		t.Fatalf("retry failed: %s", r.stderr)
	}
	id, _ = d.LoadIdentity()
	if id.DeviceID != want {
		t.Fatal("retry returned another device")
	}
}

func TestEnrollTokenSources(t *testing.T) {
	cp := cptest.New(t, nil)
	cp.AddToken("ose_file")
	dir := t.TempDir()
	if r := runCLI(t, "", nil, "--state-dir", dir, "enroll", "--origin", cp.URL, "ose_file"); r.code != 2 {
		t.Fatalf("a token argument must be rejected: %d %s", r.code, r.stderr)
	}
	tf := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tf, []byte("ose_file\n"), 0o600)
	r := runCLI(t, "", map[string]string{"NO_COLOR": "1"}, "--state-dir", dir, "enroll", "--origin", cp.URL, "--token-file", tf)
	if r.code != 0 {
		t.Fatalf("token file enroll: %s", r.stderr)
	}
	if strings.Contains(r.stdout, "\x1b[") {
		t.Fatal("NO_COLOR output has ANSI codes")
	}
	if r := runCLI(t, "ose_bad\n", nil, "--state-dir", t.TempDir(), "enroll", "--origin", cp.URL); r.code != 1 || !strings.Contains(r.stderr, "expired, revoked or already used") {
		t.Fatalf("bad token: %d %s", r.code, r.stderr)
	}
	if r := runCLI(t, "not-a-token\n", nil, "--state-dir", t.TempDir(), "enroll", "--origin", cp.URL); r.code != 1 {
		t.Fatalf("a malformed token must fail: %d", r.code)
	}
}

func TestStatusResetAndImageBuild(t *testing.T) {
	dir := t.TempDir()
	r := runCLI(t, "", nil, "--no-color", "--state-dir", dir, "status")
	if r.code != 0 || !strings.Contains(r.stdout, "not enrolled") || strings.Contains(r.stdout, "\x1b[") {
		t.Fatalf("status: %d %q", r.code, r.stdout)
	}
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"device_id":"x"}`), 0o600)
	if r := runCLI(t, "", nil, "--state-dir", dir, "reset"); r.code != 2 {
		t.Fatalf("reset without --yes: %d", r.code)
	}
	if r := runCLI(t, "", nil, "--state-dir", dir, "reset", "--yes"); r.code != 0 {
		t.Fatalf("reset: %s", r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Fatal("reset kept the identity")
	}
	r = runCLI(t, "", nil, "image", "build")
	if r.code != 1 {
		t.Fatalf("image build: %d", r.code)
	}
	if goruntime.GOOS == "linux" && !strings.Contains(r.stderr, "not supported on Linux") {
		t.Fatalf("image build explanation missing: %s", r.stderr)
	}
}
