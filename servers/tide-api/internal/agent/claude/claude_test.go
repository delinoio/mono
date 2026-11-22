// SPDX-License-Identifier: FSL-1.1-ALv2

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

func TestHookExitCodes(t *testing.T) {
	for r, want := range map[HookResult]int{HookStored: 0, HookDuplicate: 0, HookRetry: 1, HookReject: 2, "unknown": 1} {
		if got := r.ExitCode(); got != want {
			t.Errorf("%s: %d, want %d", r, got, want)
		}
	}
}

func TestSocketPath(t *testing.T) {
	none := func(string) string { return "" }
	if got := SocketPath(none, "/run/tc1/hooks/spawn-runner"); got != "/run/tc1/sock" {
		t.Fatalf("from the hook path: %q", got)
	}
	env := func(k string) string {
		if k == SocketEnv {
			return "/elsewhere/sock"
		}
		return ""
	}
	if got := SocketPath(env, "/run/tc1/hooks/spawn-runner"); got != "/elsewhere/sock" {
		t.Fatalf("from the environment: %q", got)
	}
}

// TestInstallScript runs the install script with stub programs: a claude
// at or above MinVersion skips the installer, an older one runs it, and a
// guest without git fails with its own exit code.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	run := func(claudeVersion string, git bool) int {
		home := t.TempDir()
		bin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		stub := func(name, body string) {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if claudeVersion != "" {
			stub("claude", "echo '"+claudeVersion+" (Claude Code)'")
		}
		if git {
			stub("git", "exit 0")
		}
		// The installer download fails: no network access in tests.
		stub("curl", "exit 22")
		// Only the tools that the script needs, so a git or curl of the
		// test host is not found.
		tools := filepath.Join(home, "tools")
		if err := os.Mkdir(tools, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sed", "sort", "head", "mktemp", "rm", "bash"} {
			if p, err := exec.LookPath(name); err == nil {
				_ = os.Symlink(p, filepath.Join(tools, name))
			}
		}
		cmd := exec.Command("/bin/sh", "-c", InstallScript(tidev1.GuestOs_GUEST_OS_LINUX))
		cmd.Env = []string{"HOME=" + home, "PATH=" + tools}
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}
	for _, tc := range []struct {
		version string
		git     bool
		code    int
	}{
		{"2.1.224", true, 0},
		{"2.1.330", true, 0},
		{"2.10.0", true, 0},
		{"3.0.0", true, 0},
		{"2.1.223", true, 12},
		{"2.0.999", true, 12},
		{"", true, 12},
		{"2.1.330", false, 14},
	} {
		if code := run(tc.version, tc.git); code != tc.code {
			t.Errorf("claude %q git %v: exit %d, want %d", tc.version, tc.git, code, tc.code)
		}
	}
	if s := InstallScript(tidev1.GuestOs_GUEST_OS_WINDOWS); !strings.Contains(s, "exit 15") {
		t.Fatalf("Windows: %s", s)
	}
}

func TestProgramRetireAt(t *testing.T) {
	created := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	g := &work.Guest{Runner: &work.Runner{ID: "r1", CreateTime: created}, Profile: &profile.Profile{MaxTTL: 4 * time.Hour}}
	p := Program("/tmp/tide-runner-r1", g, created.Add(time.Minute))
	args := strings.Join(p.Args, " ")
	want := "--retire-at " + strconv.FormatInt(created.Add(4*time.Hour-retireMargin).Unix(), 10)
	if !strings.Contains(args, "--capacity 1") || !strings.Contains(args, want) || p.CredentialEnv != "SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET" {
		t.Fatalf("program %q", args)
	}
	// A max_ttl that already passed adds no retirement.
	p = Program("/tmp/tide-runner-r1", g, created.Add(5*time.Hour))
	if strings.Contains(strings.Join(p.Args, " "), "--retire-at") {
		t.Fatalf("retire-at in the past: %q", p.Args)
	}
}
