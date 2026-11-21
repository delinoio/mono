// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBuildSteps returns build steps that create real temporary directories
// under t.TempDir and count their calls, so a test can check which
// directories remain.
type fakeBuildSteps struct {
	steps      buildSteps
	root       string
	builds     int
	rootCalls  int
	createdDir []string
}

func newFakeBuildSteps(t *testing.T, rootErr, buildErr error) *fakeBuildSteps {
	t.Helper()
	f := &fakeBuildSteps{root: t.TempDir()}
	f.steps = buildSteps{
		mkdirTemp: func() (string, error) {
			dir, err := os.MkdirTemp(f.root, "bin-")
			f.createdDir = append(f.createdDir, dir)
			return dir, err
		},
		moduleRoot: func() (string, error) {
			f.rootCalls++
			if rootErr != nil {
				return "", rootErr
			}
			return "/fake/root", nil
		},
		build: func(root, _ string) error {
			f.builds++
			if root != "/fake/root" {
				t.Errorf("build root = %q, want /fake/root", root)
			}
			return buildErr
		},
	}
	return f
}

// remaining returns the entries that are left in the directory that holds the
// temporary directories of the fake steps.
func (f *fakeBuildSteps) remaining(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBinaryBuilderReportsModuleRootFailureOnEveryCall(t *testing.T) {
	f := newFakeBuildSteps(t, errors.New("go env GOMOD: simulated"), nil)
	builder := newBinaryBuilder(f.steps)
	for i := range 2 {
		dir, err := builder()
		if err == nil || !strings.Contains(err.Error(), "go env GOMOD: simulated") {
			t.Fatalf("call %d: err = %v, want it to contain the module root failure", i, err)
		}
		if dir != "" {
			t.Errorf("call %d: dir = %q, want empty on failure", i, dir)
		}
	}
	if f.builds != 0 {
		t.Errorf("go build ran %d times after the module root failed, want 0", f.builds)
	}
	if left := f.remaining(t); len(left) != 0 {
		t.Errorf("temporary directories remain: %v", left)
	}
}

func TestBinaryBuilderReportsBuildFailureOnEveryCall(t *testing.T) {
	f := newFakeBuildSteps(t, nil, errors.New("go build: simulated"))
	builder := newBinaryBuilder(f.steps)
	for i := range 2 {
		dir, err := builder()
		if err == nil || !strings.Contains(err.Error(), "go build: simulated") {
			t.Fatalf("call %d: err = %v, want the build failure", i, err)
		}
		if dir != "" {
			t.Errorf("call %d: dir = %q, want empty on failure", i, dir)
		}
	}
	if f.builds != 1 {
		t.Errorf("build ran %d times, want 1", f.builds)
	}
	if left := f.remaining(t); len(left) != 0 {
		t.Errorf("temporary directories remain: %v", left)
	}
}

func TestBinaryBuilderBuildsOnce(t *testing.T) {
	f := newFakeBuildSteps(t, nil, nil)
	builder := newBinaryBuilder(f.steps)
	first, err := builder()
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Errorf("directories = %q and %q, want the same non-empty directory", first, second)
	}
	if f.builds != 1 || f.rootCalls != 1 || len(f.createdDir) != 1 {
		t.Errorf("builds = %d, root lookups = %d, directories = %d; want 1 each", f.builds, f.rootCalls, len(f.createdDir))
	}
	if left := f.remaining(t); len(left) != 1 {
		t.Errorf("remaining entries = %v, want only the build directory", left)
	}
}

func TestFindModuleRootIncludesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake go command is a shell script")
	}
	// A fake go command that fails and prints a reason to stderr.
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'simulated broken GOROOT' >&2\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, err := findModuleRoot()
	if err == nil || !strings.Contains(err.Error(), "go env GOMOD") || !strings.Contains(err.Error(), "simulated broken GOROOT") {
		t.Fatalf("err = %v, want it to contain go env GOMOD and the stderr text", err)
	}
}

func TestFindModuleRootWithoutGo(t *testing.T) {
	// A PATH without go makes the command fail to start; the error must still
	// name go env GOMOD.
	t.Setenv("PATH", t.TempDir())
	_, err := findModuleRoot()
	if err == nil || !strings.Contains(err.Error(), "go env GOMOD") {
		t.Fatalf("err = %v, want it to contain go env GOMOD", err)
	}
}
