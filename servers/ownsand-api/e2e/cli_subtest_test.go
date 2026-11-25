// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand"
)

func TestCLISubtestFailures(t *testing.T) {
	const (
		failureEnv  = "OWNSAND_E2E_CLI_SUBTEST_FAILURE"
		missingTemp = "missing-temp"
		marker      = "CLI sibling ran"
	)
	if failure := os.Getenv(failureEnv); failure != "" {
		// The outer test allocates the binary directory so this test's first
		// TempDir call can fail even when the CLI uses the wrong test handle.
		bins, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		s := &stack{t: t, bins: bins}
		t.Run("invoke", func(t *testing.T) {
			if failure == "config-directory" {
				t.Setenv("GOTMPDIR", filepath.Join(bins, missingTemp))
			}
			s.cli(t, ownsand.TransportRelay, nil, "fixture")
		})
		t.Run("sibling", func(t *testing.T) { t.Log(marker) })
		return
	}

	bins := t.TempDir()
	missing := exec.Command(filepath.Join(bins, "ownsand"), "fixture").Run()
	if missing == nil {
		t.Fatal("the empty binary directory contained an executable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		diagnostic []string
	}{
		{"launch", []string{"ownsand [fixture]: " + missing.Error()}},
		{"config-directory", []string{"TempDir: ", filepath.Join(bins, missingTemp)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCLISubtestFailures$", "-test.v")
			cmd.Dir = bins
			cmd.Env = append(cmd.Environ(), failureEnv+"="+tc.name)
			out, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("test subprocess error = %v, want exit status 1\n%s", err, out)
			}

			const testName = "TestCLISubtestFailures"
			output := string(out)
			_, afterInvoke, foundInvoke := strings.Cut(output, "=== RUN   "+testName+"/invoke\n")
			invoke, sibling, foundSibling := strings.Cut(afterInvoke, "=== RUN   "+testName+"/sibling\n")
			if !foundInvoke || !foundSibling || !strings.Contains(sibling, marker) ||
				!strings.Contains(output, "--- FAIL: "+testName+"/invoke ") ||
				!strings.Contains(output, "--- PASS: "+testName+"/sibling ") ||
				strings.Contains(output, "subtest may have called FailNow on a parent test") {
				t.Fatalf("expected only the invoking subtest to fail and its sibling to run\n%s", out)
			}
			if strings.Contains(invoke, "\n=== NAME  "+testName+"\n") {
				t.Fatalf("the diagnostic was attributed to the parent test\n%s", out)
			}
			for _, diagnostic := range tc.diagnostic {
				if !strings.Contains(invoke, diagnostic) {
					t.Errorf("invoking subtest did not report %q\n%s", diagnostic, out)
				}
			}
		})
	}
}

func TestCLISubtestConfigDirectories(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	bins := t.TempDir()
	name := "ownsand"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bins, name), binary, 0o700); err != nil {
		t.Fatal(err)
	}

	s := &stack{t: t, bins: bins}
	var dirs []string
	t.Run("invoke", func(t *testing.T) {
		for _, code := range []int{0, 5} {
			r := s.cli(t, ownsand.TransportRelay, nil, "-test.run=^TestCLIStandInProcess$", "--", "stand-in", strconv.Itoa(code))
			dir, found := strings.CutPrefix(string(r.stdout), "cli stdout\n")
			dir = strings.TrimSuffix(dir, "\n")
			if r.code != code || r.stderr != "cli stderr\n" || !found || dir == "" {
				t.Fatalf("CLI exit, stdout, stderr = (%d, %q, %q), want (%d, stdout marker and config directory, stderr marker)", r.code, r.stdout, r.stderr, code)
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("CLI config path %q is not a directory: %v", dir, err)
			}
			dirs = append(dirs, dir)
		}
		if dirs[0] == dirs[1] {
			t.Fatalf("both CLI invocations used config directory %q", dirs[0])
		}
	})
	t.Run("sibling", func(t *testing.T) {
		for _, dir := range dirs {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("CLI config directory %q remains after subtest cleanup: %v", dir, err)
			}
		}
	})
}

// TestCLIStandInProcess runs in the test binary copied into the CLI's path.
func TestCLIStandInProcess(t *testing.T) {
	if flag.NArg() != 2 || flag.Arg(0) != "stand-in" {
		t.Skip("only runs as the CLI stand-in")
	}
	code, err := strconv.Atoi(flag.Arg(1))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "cli stdout")
	fmt.Fprintln(os.Stdout, os.Getenv("OWNSAND_CONFIG_DIR"))
	fmt.Fprintln(os.Stderr, "cli stderr")
	os.Exit(code)
}
