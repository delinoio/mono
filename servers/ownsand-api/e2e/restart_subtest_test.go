// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const envRestartStandIn = "OWNSAND_E2E_RESTART_STAND_IN"

// runRestartStandIn runs before test flag parsing. The real launchers pass
// the API no arguments and pass "run" to the agent.
func runRestartStandIn() {
	switch os.Getenv(envRestartStandIn) {
	case "exit":
		os.Exit(0)
	case "block":
		fmt.Fprintln(os.Stdout, "replacement state dir:", os.Getenv("OWNSAND_AGENT_STATE_DIR"))
		fmt.Fprintln(os.Stderr, "replacement stderr")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func copyRestartStandIn(t *testing.T, dir, name string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	writeExecutable(t, filepath.Join(dir, name), binary, 0o700)
}

func TestLifecycleHyperVRestartDiagnostics(t *testing.T) {
	t.Parallel()
	const testName = "TestLifecycleHyperVRestartDiagnostics"
	const scenarioEnv = "OWNSAND_E2E_RESTART_SCENARIO"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		bins, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		s := &stack{t: t, logs: &logSink{}, bins: bins, agents: make([]agentProc, 1)}
		runHyperVRecoveryCases(t, func(t *testing.T) {
			switch scenario {
			case "stop-timeout":
				bin, args := helperArgs()
				p := startProcess(t, s.logs, "api", []string{envHelperMode + "=exit"}, bin, args...)
				<-p.done
				synctest.Test(t, func(t *testing.T) {
					// The process has been reaped. Hold the completion
					// signal open to simulate a stalled wait for log drain.
					s.api = &proc{name: p.name, cmd: p.cmd, done: make(chan struct{})}
					s.killAPI(t)
				})
			case "api-launch":
				s.restartAPI(t)
			case "api-readiness":
				// A different listener returns 200 while the replacement
				// exits without reporting that it owns the listener.
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(srv.Close)
				s.apiURL = srv.URL
				s.apiEnv = []string{envRestartStandIn + "=exit"}
				s.restartAPI(t)
			}
		}, func(t *testing.T) {
			if scenario == "agent-launch" {
				s.restartAgent(t, 0)
			}
			t.Log("agent case ran")
		}, func(t *testing.T) {
			t.Log("removal case ran")
		})
		t.Run("independent", func(t *testing.T) { t.Log("independent sibling ran") })
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	emptyBins := t.TempDir()
	apiBins := t.TempDir()
	copyRestartStandIn(t, apiBins, "ownsand-api")
	missingAPI := exec.Command(filepath.Join(emptyBins, "ownsand-api")).Run()
	missingAgent := exec.Command(filepath.Join(emptyBins, "ownsand-agent"), "run").Run()
	if missingAPI == nil || missingAgent == nil {
		t.Fatal("the empty binary directory contained a replacement executable")
	}

	const apiCase = "control-plane-restart"
	const agentCase = "agent-crash"
	const removalCase = "device-removal"
	const apiSkip = apiCase + " failed and the shared control plane may be unavailable"
	const agentSkip = agentCase + " failed and the shared agent may be unavailable"
	for _, tc := range []struct {
		name, scenario, dir, selection string
		failed, diagnostic             string
		runMarker                      string
		skipCases                      []string
		skipReason                     string
	}{
		{
			name: "stop-timeout", scenario: "stop-timeout", dir: emptyBins,
			failed: apiCase, diagnostic: "the killed process api did not exit",
			skipCases: []string{agentCase, removalCase}, skipReason: apiSkip,
		},
		{
			name: "api-launch", scenario: "api-launch", dir: emptyBins,
			failed: apiCase, diagnostic: "start api-restarted: " + missingAPI.Error(),
			skipCases: []string{agentCase, removalCase}, skipReason: apiSkip,
		},
		{
			name: "api-readiness", scenario: "api-readiness", dir: apiBins,
			failed: apiCase, diagnostic: "the restarted control plane did not become healthy: the process exited before it became healthy",
			skipCases: []string{agentCase, removalCase}, skipReason: apiSkip,
		},
		{
			name: "agent-launch", scenario: "agent-launch", dir: emptyBins,
			failed: agentCase, diagnostic: "start agent0-restarted: " + missingAgent.Error(),
			skipCases: []string{removalCase}, skipReason: agentSkip,
		},
		{
			name: "direct-agent", scenario: "api-launch", dir: emptyBins,
			selection: agentCase, runMarker: "agent case ran",
		},
		{
			name: "direct-removal", scenario: "agent-launch", dir: emptyBins,
			selection: removalCase, runMarker: "removal case ran",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := "^" + testName + "$"
			if tc.selection != "" {
				run += "/^" + tc.selection + "$"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run="+run, "-test.v")
			cmd.Dir = tc.dir
			cmd.Env = append(cmd.Environ(), scenarioEnv+"="+tc.scenario)
			out, err := cmd.CombinedOutput()
			output := string(out)
			if tc.failed == "" {
				if err != nil || strings.Contains(output, "--- SKIP:") {
					t.Fatalf("direct selection did not pass: %v\n%s", err, out)
				}
				requireRestartCaseOutput(t, output, testName+"/"+tc.selection, "PASS", tc.runMarker)
				return
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 ||
				strings.Contains(output, "subtest may have called FailNow on a parent test") ||
				!strings.Contains(output, "independent sibling ran") ||
				!strings.Contains(output, "--- PASS: "+testName+"/independent ") {
				t.Fatalf("expected a failure with an independent sibling pass: %v\n%s", err, out)
			}
			requireRestartCaseOutput(t, output, testName+"/"+tc.failed, "FAIL", tc.diagnostic)
			for _, name := range tc.skipCases {
				requireRestartCaseOutput(t, output, testName+"/"+name, "SKIP", tc.skipReason)
			}
		})
	}
}

func requireRestartCaseOutput(t *testing.T, output, name, status, diagnostic string) {
	t.Helper()
	_, after, found := strings.Cut(output, "=== RUN   "+name+"\n")
	attributed, _, _ := strings.Cut(after, "\n===")
	if !found || !strings.Contains(attributed, diagnostic) ||
		!strings.Contains(output, "--- "+status+": "+name+" ") {
		t.Fatalf("expected %s to report %q under %s\n%s", status, diagnostic, name, output)
	}
}

func TestLifecycleHyperVReplacementCleanup(t *testing.T) {
	t.Parallel()
	bins := t.TempDir()
	copyRestartStandIn(t, bins, "ownsand-agent")
	stateDir := t.TempDir()
	logs := &logSink{}
	var replacement *proc
	t.Run("owner", func(t *testing.T) {
		s := &stack{t: t, logs: logs, bins: bins, agents: []agentProc{{
			env: []string{envRestartStandIn + "=block", "OWNSAND_AGENT_STATE_DIR=" + stateDir},
		}}}
		if !t.Run("restart", func(t *testing.T) { s.restartAgent(t, 0) }) {
			return
		}
		replacement = s.agents[0].proc
		t.Run("sibling", func(t *testing.T) {
			want := "agent0-restarted: replacement state dir: " + stateDir
			deadline := time.Now().Add(10 * time.Second)
			for !logs.hasLine(want) || !logs.hasLine("agent0-restarted: replacement stderr") {
				if time.Now().After(deadline) {
					t.Fatalf("replacement did not log its state directory and stderr")
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-replacement.done:
				t.Fatal("replacement exited before the owner cleaned up")
			default:
			}
		})
	})
	if replacement == nil {
		t.Fatal("no replacement process was started")
	}
	select {
	case <-replacement.done:
	default:
		t.Fatal("replacement survived owner cleanup")
	}
	assertNoScanners(t, logs)
}
