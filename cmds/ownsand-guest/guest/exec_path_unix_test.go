// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// writeTool writes an executable shell script that prints out into a new
// temporary directory and returns the directory.
func writeTool(t *testing.T, name, out string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLookPathIn(t *testing.T) {
	tool := writeTool(t, "mytool", "x")
	other := writeTool(t, "mytool", "y")
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "mytool"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirNamed := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirNamed, "mytool"), 0o700); err != nil {
		t.Fatal(err)
	}
	list := func(dirs ...string) []string {
		return []string{"PATH=" + strings.Join(dirs, string(os.PathListSeparator))}
	}

	t.Run("first executable wins", func(t *testing.T) {
		got, err := lookPathIn("mytool", list(tool, other))
		if err != nil || got != filepath.Join(tool, "mytool") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("skips a non-executable file, a directory and a missing directory", func(t *testing.T) {
		got, err := lookPathIn("mytool", list(plain, dirNamed, filepath.Join(plain, "missing"), other))
		if err != nil || got != filepath.Join(other, "mytool") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("missing program wraps ErrNotFound", func(t *testing.T) {
		for name, env := range map[string][]string{
			"not in PATH":        list(plain, dirNamed),
			"empty PATH":         {"PATH="},
			"no PATH":            {"OTHER=" + tool},
			"no environment":     nil,
			"other key spelling": {"path=" + tool},
		} {
			if _, err := lookPathIn("mytool", env); !errors.Is(err, exec.ErrNotFound) {
				t.Errorf("%s: error %v", name, err)
			}
		}
	})
	t.Run("last PATH entry of the environment wins", func(t *testing.T) {
		got, err := lookPathIn("mytool", []string{"PATH=" + plain, "PATH=" + tool})
		if err != nil || got != filepath.Join(tool, "mytool") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("a match in a relative directory returns ErrDot", func(t *testing.T) {
		t.Chdir(tool)
		for _, p := range []string{".", ":" + t.TempDir()} {
			if _, err := lookPathIn("mytool", []string{"PATH=" + p}); !errors.Is(err, exec.ErrDot) {
				t.Errorf("PATH %q: error %v", p, err)
			}
		}
	})
}

func TestNewCommandResolvesBareNameWithRequestPath(t *testing.T) {
	tool := writeTool(t, "mytool", "x")
	reqEnv := map[string]string{"PATH": tool}
	cmd := newCommand([]string{"mytool", "a", "b"}, reqEnv, commandEnv(reqEnv))
	if cmd.Err != nil || cmd.Path != filepath.Join(tool, "mytool") {
		t.Fatalf("path %q, err %v", cmd.Path, cmd.Err)
	}
	if got := strings.Join(cmd.Args, " "); got != "mytool a b" {
		t.Fatalf("args %q", got)
	}

	missing := map[string]string{"PATH": t.TempDir()}
	cmd = newCommand([]string{"mytool"}, missing, commandEnv(missing))
	if !errors.Is(cmd.Err, exec.ErrNotFound) || cmd.Path != "mytool" {
		t.Fatalf("path %q, err %v", cmd.Path, cmd.Err)
	}
	if got := startFailure(cmd.Err).GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("reason %v", got)
	}
}

func TestNewCommandKeepsOSLookupWithoutRequestPath(t *testing.T) {
	tool := writeTool(t, "mytool", "x")
	// The request PATH sets up nothing here: the helper PATH decides.
	t.Setenv("PATH", tool)
	other := map[string]string{"OTHER": "1"}
	cmd := newCommand([]string{"mytool"}, other, commandEnv(other))
	if cmd.Err != nil || cmd.Path != filepath.Join(tool, "mytool") {
		t.Fatalf("path %q, err %v", cmd.Path, cmd.Err)
	}
	t.Setenv("PATH", t.TempDir())
	cmd = newCommand([]string{"mytool"}, nil, commandEnv(nil))
	if !errors.Is(cmd.Err, exec.ErrNotFound) {
		t.Fatalf("err %v", cmd.Err)
	}
	// A name with a path separator is never searched in PATH.
	req := map[string]string{"PATH": tool}
	for _, name := range []string{"./mytool", "sub/mytool", filepath.Join(tool, "mytool")} {
		cmd = newCommand([]string{name}, req, commandEnv(req))
		if cmd.Err != nil || cmd.Path != name {
			t.Fatalf("%q: path %q, err %v", name, cmd.Path, cmd.Err)
		}
	}
}

// TestRequestPathsOfConcurrentCommandsDoNotInterfere starts commands with
// different request PATH values at the same time. Each must run the tool of
// its own directory, and the helper environment must stay as it was.
func TestRequestPathsOfConcurrentCommandsDoNotInterfere(t *testing.T) {
	helperPath := t.TempDir()
	t.Setenv("PATH", helperPath)
	const n = 8
	reqs := make([]map[string]string, n)
	for i := range reqs {
		reqs[i] = map[string]string{"PATH": writeTool(t, "mytool", fmt.Sprint("tool-", i))}
	}
	got := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := newCommand([]string{"mytool"}, reqs[i], commandEnv(reqs[i]))
			cmd.Env = commandEnv(reqs[i])
			out, err := cmd.Output()
			got[i], errs[i] = string(out), err
		}()
	}
	wg.Wait()
	for i := range reqs {
		if errs[i] != nil || got[i] != fmt.Sprint("tool-", i) {
			t.Errorf("command %d: output %q, err %v", i, got[i], errs[i])
		}
	}
	if os.Getenv("PATH") != helperPath {
		t.Fatalf("helper PATH changed to %q", os.Getenv("PATH"))
	}
}

func TestExecBareNameUsesRequestPath(t *testing.T) {
	tool := writeTool(t, "mytool", "from-request-path")
	// The helper PATH holds no tool, and it must stay that way.
	helperPath := t.TempDir()
	t.Setenv("PATH", helperPath)
	h := newHarness(t, nil)

	req := execReq(t, "mytool")
	req.Env = map[string]string{"PATH": tool}
	evs := h.runExec(req)
	if stdout, _ := outputs(evs); stdout != "from-request-path" {
		t.Fatalf("events %v", evs)
	}
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("events %v: %v", evs, err)
	}

	// Without a PATH in the request the helper PATH decides.
	evs = h.runExec(execReq(t, "mytool"))
	if got := evs[len(evs)-1].GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("events %v", evs)
	}
	req = execReq(t, "mytool")
	req.Env = map[string]string{"OTHER": "1"}
	evs = h.runExec(req)
	if got := evs[len(evs)-1].GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("events %v", evs)
	}

	// A request PATH without the program fails the same way.
	req = execReq(t, "mytool")
	req.Env = map[string]string{"PATH": t.TempDir()}
	evs = h.runExec(req)
	if got := evs[len(evs)-1].GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("events %v", evs)
	}

	// An absolute path and a "./" name relative to the working directory keep
	// working with a request PATH that does not hold the program.
	for _, name := range []string{filepath.Join(tool, "mytool"), "./mytool"} {
		req = execReq(t, name)
		req.Env = map[string]string{"PATH": helperPath}
		req.WorkingDirectory = tool
		evs = h.runExec(req)
		if stdout, _ := outputs(evs); stdout != "from-request-path" {
			t.Fatalf("%q: events %v", name, evs)
		}
		if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
			t.Fatalf("%q: events %v: %v", name, evs, err)
		}
	}
	if os.Getenv("PATH") != helperPath {
		t.Fatalf("helper PATH changed to %q", os.Getenv("PATH"))
	}
}
