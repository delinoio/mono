// SPDX-License-Identifier: FSL-1.1-ALv2

package codex

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

// TestSignatureVector checks the test vector of the Standard Webhooks
// reference libraries.
func TestSignatureVector(t *testing.T) {
	key, err := SecretKey("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"test": 2432232314}`)
	if got := Sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", body); got != "g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("signature %s", got)
	}
	h := http.Header{}
	h.Set("webhook-id", "msg_p5jXN8AQM9LWM0D4loKWxJek")
	h.Set("webhook-timestamp", "1614265330")
	h.Set("webhook-signature", "v1,Zm9v v1a,x v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=")
	at := time.Unix(1614265330, 0)
	if err := VerifySignature(key, h, body, at); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := VerifySignature(key, h, body, at.Add(SignatureTolerance)); err != nil {
		t.Fatalf("at the tolerance: %v", err)
	}
	for name, fn := range map[string]func(http.Header) []byte{
		"changed body":    func(http.Header) []byte { return []byte(`{"test": 2432232315}`) },
		"no id":           func(h http.Header) []byte { h.Del("webhook-id"); return body },
		"no timestamp":    func(h http.Header) []byte { h.Del("webhook-timestamp"); return body },
		"text timestamp":  func(h http.Header) []byte { h.Set("webhook-timestamp", "soon"); return body },
		"no signature":    func(h http.Header) []byte { h.Del("webhook-signature"); return body },
		"v1 without data": func(h http.Header) []byte { h.Set("webhook-signature", "v1,"); return body },
		"other id":        func(h http.Header) []byte { h.Set("webhook-id", "msg_other"); return body },
	} {
		hh := h.Clone()
		if err := VerifySignature(key, hh, fn(hh), at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := VerifySignature(key, h, body, at.Add(SignatureTolerance+time.Second)); err == nil {
		t.Error("stale: accepted")
	}
	if err := VerifySignature(key, h, body, at.Add(-SignatureTolerance-time.Second)); err == nil {
		t.Error("future: accepted")
	}
}

func TestSecretKey(t *testing.T) {
	if k, err := SecretKey("plain-secret"); err != nil || string(k) != "plain-secret" {
		t.Fatalf("without prefix: %q %v", k, err)
	}
	for _, bad := range []string{"whsec_", "whsec_not base64!", "whsec_YQ"} {
		if _, err := SecretKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNeedsConnection(t *testing.T) {
	s := &Session{}
	if s.NeedsConnection() {
		t.Fatal("no environment")
	}
	s.Environment = &struct {
		Type               string `json:"type"`
		ID                 string `json:"id"`
		RemoteURL          string `json:"remote_url"`
		WorkspaceDirectory string `json:"workspace_directory"`
	}{Type: "self_hosted", ID: "env", RemoteURL: "https://api.openai.com/v1/agents/api"}
	if s.NeedsConnection() {
		t.Fatal("no required action")
	}
	s.RequiredActions = append(s.RequiredActions, struct {
		Type          string `json:"type"`
		EnvironmentID string `json:"environment_id"`
	}{Type: "environment_connection"})
	if !s.NeedsConnection() {
		t.Fatal("connection required")
	}
	s.Environment.Type = "openai_hosted"
	if s.NeedsConnection() {
		t.Fatal("not self-hosted")
	}
}

func TestValidRemote(t *testing.T) {
	long := "https://relay.openai.example/" + strings.Repeat("a", 2048-len("https://relay.openai.example/"))
	for u, want := range map[string]bool{
		"https://relay.openai.example/env/1":                       true,
		"https://relay.openai.example/" + strings.Repeat("a", 300): true,
		long:                                true,
		long + "a":                          false,
		"https://":                          false,
		"https:///executor":                 false,
		"https://:443/executor":             false,
		"https://[::1/executor":             false,
		"https://relay.openai.example:bad":  false,
		"https://relay.openai.example/%":    false,
		"http://relay.openai.example":       false,
		"https://relay.openai.example/a b":  false,
		"https://relay.openai.example/\x00": false,
	} {
		if got := validRemote(u); got != want {
			t.Errorf("validRemote(%q) = %v", u, got)
		}
	}
}

func TestValidWorkspace(t *testing.T) {
	for dir, ok := range map[string]bool{"/workspace": true, "/home/u/w": true, "workspace": false, "/a/../b": false, "/a\nb": false, "": false} {
		if validWorkspace(dir) != ok {
			t.Errorf("%q", dir)
		}
	}
}

// TestNodeChecksums fails until the checksum table matches the pinned
// Node.js version (Renovate updates the version only).
func TestNodeChecksums(t *testing.T) {
	if nodeChecksumsVersion != NodeVersion {
		t.Fatalf("nodeSHA256 is for %s, NodeVersion is %s: copy the four .tar.gz sums from SHASUMS256.txt", nodeChecksumsVersion, NodeVersion)
	}
	if !strings.HasPrefix(NodeVersion, "24.") {
		t.Fatalf("NodeVersion %s is not Node.js 24", NodeVersion)
	}
	for _, k := range []string{"linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64"} {
		if len(nodeSHA256[k]) != 64 {
			t.Errorf("%s: %q", k, nodeSHA256[k])
		}
	}
}

// TestInstallScript runs the install script with stub programs.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	run := func(stubs map[string]string) (int, string) {
		home := t.TempDir()
		bin := filepath.Join(home, ".local", "bin")
		tools := filepath.Join(home, "tools")
		for _, d := range []string{bin, tools} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"uname", "cut", "mktemp", "rm", "mkdir", "mv", "tar", "chmod"} {
			if p, err := exec.LookPath(name); err == nil {
				_ = os.Symlink(p, filepath.Join(tools, name))
			}
		}
		for name, body := range stubs {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+strings.ReplaceAll(body, "$BIN", bin)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("/bin/sh", "-c", InstallScript(sandrunv1.GuestOs_GUEST_OS_LINUX))
		cmd.Env = []string{"HOME=" + home, "PATH=" + tools}
		out, _ := cmd.CombinedOutput()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	npm := `[ "$NPM_CONFIG_PREFIX" = "$HOME/.local" ] || exit 9
[ "$3" = "@openai/codex@` + CodexVersion + `" ] || exit 8
printf '#!/bin/sh\nexit 0\n' > "$BIN/codex"; chmod +x "$BIN/codex"`
	cases := map[string]struct {
		stubs map[string]string
		code  int
	}{
		"codex present":        {map[string]string{"codex": "exit 0"}, 0},
		"node 20 present":      {map[string]string{"node": "echo 20", "npm": npm}, 0},
		"npm fails":            {map[string]string{"node": "echo 22", "npm": "exit 1"}, 14},
		"node 14, no download": {map[string]string{"node": "echo 14", "npm": npm, "curl": "exit 22"}, 12},
		"no node, no tools":    {map[string]string{}, 10},
	}
	// Node.js 14 in $HOME/.local/bin and a download that succeeds: the
	// downloaded Node.js comes first on PATH.
	if arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]; runtime.GOOS == "linux" && arch != "" {
		cases["node 14, download"] = struct {
			stubs map[string]string
			code  int
		}{map[string]string{"node": "echo 14", "npm": npm, "curl": "exit 0", "sha256sum": "echo " + nodeSHA256["linux-"+arch],
			"tar": `mkdir -p "$4/bin" && printf '#!/bin/sh\necho 24\n' > "$4/bin/node" && chmod +x "$4/bin/node"`}, 0}
	}
	for name, tc := range cases {
		if code, out := run(tc.stubs); code != tc.code {
			t.Errorf("%s: exit %d, want %d: %s", name, code, tc.code, out)
		}
	}
	if s := InstallScript(sandrunv1.GuestOs_GUEST_OS_WINDOWS); s != "exit 18\n" {
		t.Fatalf("Windows: %q", s)
	}
}
