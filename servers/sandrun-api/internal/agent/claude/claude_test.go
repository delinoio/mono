// SPDX-License-Identifier: FSL-1.1-ALv2

package claude

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

type acceptUnixFunc func() (*net.UnixConn, error)

func (f acceptUnixFunc) AcceptUnix() (*net.UnixConn, error) { return f() }

func TestClaudeHookAcceptRecovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Claude orchestrators run on Linux and macOS")
	}
	// Keep the run directory short, as Watch does for the real socket.
	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, socketName)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	w := &agent.Watch{Logger: slog.New(slog.NewJSONHandler(&logs, nil)).With("agent_integration_id", "test-integration")}
	attempts := 0
	listener := acceptUnixFunc(func() (*net.UnixConn, error) {
		attempts++
		if attempts == 1 || attempts == 3 {
			return nil, os.NewSyscallError("accept", syscall.EMFILE)
		}
		return ln.AcceptUnix()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Vendor{}).serve(t.Context(), w, listener)
	}()
	for range 2 {
		// A pre-warming request exercises the real hook exchange without a store.
		reply, err := exchange(socket, hookRequest{OrderID: "order"})
		if err != nil {
			t.Error(err)
			break
		}
		if reply.Result != HookReject {
			t.Errorf("hook result = %q, want %q", reply.Result, HookReject)
			break
		}
	}
	_ = ln.Close()
	<-done

	var failures int
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, `"msg":"claude_hook_accept_failed"`) {
			continue
		}
		failures++
		for _, field := range []string{
			`"level":"ERROR"`, `"agent_integration_id":"test-integration"`, syscall.EMFILE.Error(),
			fmt.Sprintf(`"retry_in":%d`, hookAcceptBackoffMin.Nanoseconds()),
		} {
			if !strings.Contains(line, field) {
				t.Errorf("accept log is missing %q: %s", field, line)
			}
		}
	}
	if failures != 2 {
		t.Fatalf("logged %d accept failures, want 2", failures)
	}
}

func TestClaudeHookAcceptClosed(t *testing.T) {
	var logs bytes.Buffer
	w := &agent.Watch{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	attempts := 0
	listener := acceptUnixFunc(func() (*net.UnixConn, error) {
		attempts++
		if attempts > 1 {
			t.Fatal("serve retried a closed listener")
		}
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: net.ErrClosed}
	})
	(&Vendor{}).serve(t.Context(), w, listener)
	if attempts != 1 || logs.Len() != 0 {
		t.Fatalf("closed listener: %d accepts and %q logs, want one accept and no logs", attempts, logs.String())
	}
}

func TestClaudeHookAcceptBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		w := &agent.Watch{Logger: slog.New(slog.DiscardHandler)}
		delays := []time.Duration{5, 10, 20, 40, 80, 160, 320, 640, 1000, 1000}
		attempts := make(chan time.Time, len(delays)+2)
		listener := acceptUnixFunc(func() (*net.UnixConn, error) {
			select {
			case attempts <- time.Now():
			case <-ctx.Done():
			}
			return nil, syscall.EMFILE
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&Vendor{}).serve(ctx, w, listener)
		}()
		synctest.Wait()
		if len(attempts) != 1 {
			t.Fatalf("got %d initial accept attempts, want one", len(attempts))
		}
		previous := <-attempts
		for i, ms := range delays {
			delay := ms * time.Millisecond
			time.Sleep(delay - time.Nanosecond)
			synctest.Wait()
			if len(attempts) != 0 {
				t.Fatalf("accept retried before the %s delay", delay)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if len(attempts) != 1 {
				t.Fatalf("got %d accept attempts at retry %d, want one", len(attempts), i+1)
			}
			retry := <-attempts
			if got := retry.Sub(previous); got != delay {
				t.Fatalf("retry %d followed after %s, want %s", i+1, got, delay)
			}
			previous = retry
		}
		beforeCancel := time.Now()
		cancel()
		<-done
		if !time.Now().Equal(beforeCancel) || len(attempts) != 0 {
			t.Fatal("cancel did not stop the retry wait immediately")
		}
	})
}

func TestHookExitCodes(t *testing.T) {
	for r, want := range map[HookResult]int{HookStored: 0, HookDuplicate: 0, HookRetry: 1, HookReject: 2, "unknown": 1} {
		if got := r.ExitCode(); got != want {
			t.Errorf("%s: %d, want %d", r, got, want)
		}
	}
}

func TestSocketPath(t *testing.T) {
	none := func(string) string { return "" }
	// SocketPath works with host paths, so build the expected path with the
	// host's separators and an absolute root (a drive letter on Windows).
	root := t.TempDir()
	hook := filepath.Join(root, "hooks", "spawn-runner")
	if got, want := SocketPath(none, hook), filepath.Join(root, "sock"); got != want {
		t.Fatalf("from the hook path: %q, want %q", got, want)
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
		cmd := exec.Command("/bin/sh", "-c", InstallScript(sandrunv1.GuestOs_GUEST_OS_LINUX))
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
	if s := InstallScript(sandrunv1.GuestOs_GUEST_OS_WINDOWS); !strings.Contains(s, "exit 15") {
		t.Fatalf("Windows: %s", s)
	}
}

func TestProgramRetireAt(t *testing.T) {
	created := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	retireAt := strconv.FormatInt(created.Add(55*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name      string
		profile   *profile.Profile
		storedTTL time.Duration
		elapsed   time.Duration
		want      string
	}{
		{"raised profile TTL", &profile.Profile{MaxTTL: 8 * time.Hour}, time.Hour, time.Minute, retireAt},
		{"lowered profile TTL", &profile.Profile{MaxTTL: 30 * time.Minute}, time.Hour, time.Minute, retireAt},
		{"deleted profile", nil, time.Hour, time.Minute, retireAt},
		{"over a minute away", nil, time.Hour, 54*time.Minute - time.Second, retireAt},
		{"exactly a minute away", nil, time.Hour, 54 * time.Minute, ""},
		{"under a minute away", nil, time.Hour, 54*time.Minute + time.Second, ""},
		{"in the past", nil, time.Hour, 56 * time.Minute, ""},
		{"unavailable sandbox TTL", &profile.Profile{MaxTTL: time.Hour}, 0, time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &work.Guest{Runner: &work.Runner{ID: "r1", CreateTime: created}, Profile: tc.profile, SandboxMaxTTL: tc.storedTTL}
			p := Program("/tmp/sandrun-runner-r1", g, created.Add(tc.elapsed))
			if !strings.Contains(strings.Join(p.Args, " "), "--capacity 1") || p.CredentialEnv != "SELF_HOSTED_RUNNER_ENVIRONMENT_SECRET" {
				t.Fatalf("unexpected program %q", p.Args)
			}
			index := slices.Index(p.Args, "--retire-at")
			if tc.want == "" {
				if index >= 0 {
					t.Fatalf("unexpected --retire-at in %q", p.Args)
				}
			} else if index < 0 || index+1 == len(p.Args) || p.Args[index+1] != tc.want {
				t.Fatalf("want --retire-at %s in %q", tc.want, p.Args)
			}
		})
	}
}
