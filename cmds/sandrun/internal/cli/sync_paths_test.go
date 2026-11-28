// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestSyncSourcePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	repoA, repoB := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	initSyncSourceRepo(t, repoA, "a.txt")
	initSyncSourceRepo(t, repoB, "b.txt")
	if err := os.Mkdir(filepath.Join(repoB, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../B/nested", filepath.Join(repoA, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, ".git", "info", "exclude"), []byte("/link\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"cli", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.wd = repoA
			f.env[EnvOrganization] = f.org
			f.env[EnvAPIKey] = f.srv.APIKey
			workspace := t.TempDir()
			f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, workspace)
			created := f.ok("", "--output", "json", "session", "create", "linux")
			var session struct {
				ID string `json:"dev_session_id"`
			}
			if err := json.Unmarshal([]byte(created.stdout), &session); err != nil || session.ID == "" {
				t.Fatalf("decode created session %q: %v", created.stdout, err)
			}

			syncSource := func(source ...string) string {
				t.Helper()
				result := f.run("", append([]string{"session", "sync", session.ID}, source...)...)
				switch result.code {
				case 0:
					return ""
				case 1:
					return result.stderr
				default:
					t.Fatalf("sync exited %d: %s", result.code, result.stderr)
					return ""
				}
			}
			if mode == "mcp" {
				ctx, client := startMCPCommand(t, f)
				syncSource = func(source ...string) string {
					t.Helper()
					args := map[string]any{"session_id": session.ID}
					if len(source) > 0 {
						args["local_path"] = source[0]
					}
					result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "sync", Arguments: args})
					if err != nil {
						t.Fatal(err)
					}
					if result.IsError {
						if len(result.Content) == 1 {
							if diagnostic, ok := result.Content[0].(*mcp.TextContent); ok {
								return diagnostic.Text
							}
						}
						t.Fatalf("sync returned an unexpected tool error: %+v", result)
					}
					return ""
				}
			}

			if diagnostic := syncSource("missing/.."); !strings.Contains(diagnostic, "invalid_argument: sandrun: local directory:") || !strings.Contains(diagnostic, "missing/..") {
				t.Fatalf("sync with a missing component returned %q, want the local directory error", diagnostic)
			}
			for _, procedure := range []string{
				sandrunv1connect.DevSessionServiceReadDevSessionFileProcedure,
				sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure,
				sandrunv1connect.DevSessionServiceExecDevSessionProcedure,
			} {
				if calls := f.srv.Calls(procedure); calls != 0 {
					t.Fatalf("sync with a missing component called %s %d times", procedure, calls)
				}
			}
			for _, check := range []struct {
				source []string
				marker string
			}{
				{[]string{"link/.."}, "b.txt"},
				{nil, "a.txt"},
				{[]string{""}, "a.txt"},
				{[]string{"../B"}, "b.txt"},
				{[]string{"."}, "a.txt"},
				{[]string{repoB}, "b.txt"},
			} {
				if diagnostic := syncSource(check.source...); diagnostic != "" {
					t.Fatalf("sync %q: %s", check.source, diagnostic)
				}
				entries, err := os.ReadDir(workspace)
				if err != nil || len(entries) != 1 || entries[0].Name() != check.marker {
					t.Fatalf("sync %q left %v in workspace, want only %s: %v", check.source, entries, check.marker, err)
				}
			}
		})
	}
}

func initSyncSourceRepo(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "--", marker}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}
