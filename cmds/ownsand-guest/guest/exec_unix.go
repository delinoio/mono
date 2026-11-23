// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/delinoio/mono/internal/procgroup"
)

// forceAfterTerminate: Linux and macOS guests send SIGTERM first and SIGKILL
// after KillGrace.
const forceAfterTerminate = true

// envKeyFold: environment variable names are case-sensitive on Unix.
func envKeyFold(key string) string { return key }

// hasPathSeparator reports whether a program name is a path and not a bare
// name for the PATH search.
func hasPathSeparator(name string) bool { return strings.ContainsRune(name, '/') }

// lookPathIn finds the program that a bare name selects in the PATH of env,
// like exec.LookPath does for the PATH of the process: the first directory of
// PATH that holds a regular file with an execute bit. A missing program
// returns an error that wraps exec.ErrNotFound. A match in a relative
// directory (an empty entry of a longer list means the current directory)
// returns exec.ErrDot, which os/exec returns for the same match of the
// process PATH.
func lookPathIn(name string, env []string) (string, error) {
	pathValue, _ := envValue(env, pathEnvKey, envKeyFold)
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		if !filepath.IsAbs(path) {
			return path, &exec.Error{Name: name, Err: exec.ErrDot}
		}
		return path, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// terminate and kill signal the process group that procgroup.Set created,
// so a timeout stops the command together with its children.
func terminate(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGTERM)
}

func kill(p *os.Process) {
	_ = procgroup.Signal(p, syscall.SIGKILL)
}

// processGroupAlive reports whether the process group of p still has a
// member. The group id equals the process id of the command, which stays
// reserved while a member lives.
func processGroupAlive(p *os.Process) bool {
	return !errors.Is(syscall.Kill(-p.Pid, 0), syscall.ESRCH)
}

// processExited reports whether the direct child was already reaped by
// cmd.Wait, which happens before the output pipes are drained.
func processExited(p *os.Process) bool {
	return errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 0, int32(ws.Signal()), true
	}
	return int64(state.ExitCode()), 0, false
}
