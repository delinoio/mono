// SPDX-License-Identifier: Apache-2.0

package tidetest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func newExecCommand(ctx context.Context, args []string, requestEnv map[string]string) *exec.Cmd {
	env := os.Environ()
	pathExt := os.Getenv("PATHEXT")
	var searchPath string
	var hasSearchPath bool
	for key, value := range requestEnv {
		env = append(env, key+"="+value)
		switch {
		case key == "PATH" || runtime.GOOS == "windows" && strings.EqualFold(key, "PATH"):
			searchPath, hasSearchPath = value, true
		case runtime.GOOS == "windows" && strings.EqualFold(key, "PATHEXT"):
			pathExt = value
		}
	}

	program := args[0]
	var err error
	if hasSearchPath && program != "" && !hasProgramPath(program) {
		// CommandContext uses the host PATH for bare names. Resolve the name
		// first so each request can supply its own search path.
		program, err = lookupRequestProgram(program, searchPath, pathExt)
	}
	cmd := exec.CommandContext(ctx, program, args[1:]...)
	cmd.Args[0] = args[0]
	if err != nil {
		cmd.Err = err
	}
	cmd.Env = env
	configureExecCommand(cmd)
	return cmd
}

func hasProgramPath(name string) bool {
	if runtime.GOOS == "windows" {
		return strings.ContainsAny(name, `:\/`)
	}
	return strings.ContainsRune(name, '/')
}

func lookupRequestProgram(name, searchPath, pathExt string) (string, error) {
	dirs := filepath.SplitList(searchPath)
	var extensions []string
	if runtime.GOOS == "windows" {
		// The Windows guest checks the current directory before PATH.
		dirs = append([]string{"."}, dirs...)
		if pathExt == "" {
			pathExt = ".com;.exe;.bat;.cmd"
		}
		for ext := range strings.SplitSeq(strings.ToLower(pathExt), ";") {
			if ext == "" {
				continue
			}
			if ext[0] != '.' {
				ext = "." + ext
			}
			extensions = append(extensions, ext)
		}
	}
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if path := findRequestProgram(candidate, extensions); path != "" {
			if !filepath.IsAbs(path) {
				return "", &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func findRequestProgram(path string, extensions []string) string {
	if runtime.GOOS != "windows" {
		if !filepath.IsAbs(path) {
			path = "./" + path
		}
		// An explicit path lets LookPath check execute permissions without
		// consulting the host PATH.
		resolved, _ := exec.LookPath(path)
		return resolved
	}
	isFile := func(candidate string) bool {
		info, err := os.Stat(candidate)
		return err == nil && !info.IsDir()
	}
	if (filepath.Ext(path) != "" || len(extensions) == 0) && isFile(path) {
		return path
	}
	for _, ext := range extensions {
		if candidate := path + ext; isFile(candidate) {
			return candidate
		}
	}
	return ""
}
