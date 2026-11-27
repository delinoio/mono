// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestDownloadProbeCleanup(t *testing.T) {
	needPOSIX(t)
	for _, tc := range []struct {
		name    string
		unknown bool
		fail    bool
	}{
		{name: "canceled probe reaches exit"},
		{name: "canceled probe outcome unknown", unknown: true},
		{name: "probe exits with failure", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
			dest := filepath.Join(t.TempDir(), "destination")
			if err := os.WriteFile(dest, []byte("previous"), 0o644); err != nil {
				t.Fatal(err)
			}
			tarBin, err := exec.LookPath("tar")
			if err != nil {
				t.Fatal(err)
			}
			gate := t.TempDir()
			// Hold the real probe just before tar opens the archive. In the
			// failure case, write the archive and then return a nonzero status.
			script := `#!/bin/sh
set -eu
if [ "${SANDRUN_TEST_FAIL_TAR:-}" != 1 ]; then
  : > "$SANDRUN_TEST_GATE/started"
  while [ ! -f "$SANDRUN_TEST_GATE/release" ]; do sleep 0.02; done
fi
"$SANDRUN_TEST_REAL_TAR" "$@"
if [ "${SANDRUN_TEST_FAIL_TAR:-}" = 1 ]; then
  printf 'fixture tar failure\n' >&2
  exit 1
fi
`
			if err := os.WriteFile(filepath.Join(gate, "tar"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			release := func() error { return os.WriteFile(filepath.Join(gate, "release"), nil, 0o644) }
			t.Cleanup(func() {
				if err := release(); err != nil {
					t.Error(err)
				}
			})
			t.Setenv("SANDRUN_TEST_GATE", gate)
			t.Setenv("SANDRUN_TEST_REAL_TAR", tarBin)
			t.Setenv("PATH", gate+string(os.PathListSeparator)+os.Getenv("PATH"))
			if tc.fail {
				t.Setenv("SANDRUN_TEST_FAIL_TAR", "1")
			} else {
				t.Setenv("SANDRUN_TEST_FAIL_TAR", "")
			}
			srv.BeforeExec = func(req *sandrunv1.ExecDevSessionRequest) {
				env := req.GetEnv()
				if env[guestEnvPath] == "" && env[guestEnvDLArchive] != "" {
					if _, err := os.Stat(env[guestEnvDLArchive]); err != nil {
						t.Errorf("archive when cleanup begins: %v", err)
					}
				}
			}

			downloadCtx, cancelDownload := context.WithCancel(ctx)
			defer cancelDownload()
			downloadErr := make(chan error, 1)
			go func() {
				_, err := c.Download(downloadCtx, DownloadParams{
					OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
					GuestPath: ds.GetWorkspacePath(), LocalPath: dest,
				})
				downloadErr <- err
			}()
			waitFor := func(description string, ready func() bool) {
				t.Helper()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for !ready() {
					select {
					case err := <-downloadErr:
						t.Fatalf("download returned before %s: %v", description, err)
					case <-ctx.Done():
						t.Fatalf("waiting for %s: %v", description, ctx.Err())
					case <-ticker.C:
					}
				}
			}
			attachProc := sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure
			var probeID string
			if !tc.fail {
				waitFor("probe to reach tar", func() bool {
					_, err := os.Stat(filepath.Join(gate, "started"))
					return err == nil
				})
				probe := srv.ExecRequests()[0]
				probeID = probe.GetExecId()
				if _, err := os.Stat(probe.GetEnv()[guestEnvDLArchive]); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("archive exists before releasing tar: %v", err)
				}
				if tc.unknown {
					srv.Fail(attachProc, connect.NewError(connect.CodeNotFound, errors.New("exec unknown")))
				}
				cancelDownload()
				if !tc.unknown {
					waitFor("SDK attachment to the probe", func() bool { return srv.Calls(attachProc) > 0 })
					if err := release(); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case err := <-downloadErr:
				if tc.fail {
					if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "fixture tar failure") {
						t.Fatalf("download returned %v, want the probe failure", err)
					}
				} else {
					var streamErr *ExecStreamError
					if !errors.Is(err, context.Canceled) || !errors.As(err, &streamErr) || streamErr.ExecID != probeID {
						t.Fatalf("download returned %v, want original cancellation for probe %s", err, probeID)
					}
				}
			case <-ctx.Done():
				t.Fatalf("waiting for download to return: %v", ctx.Err())
			}
			requests := srv.ExecRequests()
			wantExecs := 2 // One probe and one removal.
			if tc.unknown {
				wantExecs = 1
				// Once the SDK returns, let the probe run to completion to
				// show why removing the archive earlier was unsafe.
				if err := release(); err != nil {
					t.Fatal(err)
				}
				result, err := c.AttachExec(ctx, AttachParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: probeID})
				if err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
					t.Fatalf("observe original probe = %+v, %v", result, err)
				}
			}
			if len(requests) != wantExecs {
				t.Fatalf("got %d exec requests, want %d", len(requests), wantExecs)
			}
			archive := requests[0].GetEnv()[guestEnvDLArchive]
			if !tc.unknown && (requests[1].GetEnv()[guestEnvDLArchive] != archive || requests[1].GetEnv()[guestEnvPath] != "") {
				t.Fatal("second exec must remove the probe archive")
			}
			_, err = os.Stat(archive)
			if tc.unknown && err != nil || !tc.unknown && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("archive after %s: %v", tc.name, err)
			}
			if body, err := os.ReadFile(dest); err != nil || string(body) != "previous" {
				t.Errorf("destination after %s = %q, %v", tc.name, body, err)
			}
		})
	}
}

func TestDownloadPreservesWindowsUNCProbePath(t *testing.T) {
	for _, path := range []string{
		`\\buildhost\share\results.txt`,
		"//buildhost/share/results.txt",
		`/\buildhost/share/results.txt`,
		`\/buildhost/share/results.txt`,
	} {
		t.Run(path, func(t *testing.T) {
			fixture := &downloadProbeSession{firstExecPath: make(chan string, 1)}
			_, handler := sandrunv1connect.NewDevSessionServiceHandler(fixture)
			server := httptest.NewUnstartedServer(handler)
			server.Config.Protocols = new(http.Protocols)
			server.Config.Protocols.SetUnencryptedHTTP2(true)
			server.Start()
			defer server.Close()
			client, err := New(Options{BaseURL: server.URL, APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Download(t.Context(), DownloadParams{
				OrganizationID: "org",
				DevSessionID:   "session",
				GuestPath:      path,
				LocalPath:      filepath.Join(t.TempDir(), "results.txt"),
			})
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("Download error = %v, want NotFound from the fixture probe", err)
			}
			select {
			case got := <-fixture.firstExecPath:
				if got != path {
					t.Errorf("first ExecDevSession SANDRUN_DOWNLOAD_PATH = %q, want %q", got, path)
				}
			default:
				t.Fatal("Download sent no ExecDevSession request")
			}
		})
	}
}

type downloadProbeSession struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	firstExecPath chan string
}

func (s *downloadProbeSession) GetDevSession(context.Context, *connect.Request[sandrunv1.GetDevSessionRequest]) (*connect.Response[sandrunv1.GetDevSessionResponse], error) {
	return connect.NewResponse(&sandrunv1.GetDevSessionResponse{DevSession: &sandrunv1.DevSession{
		State: sandrunv1.RequestState_REQUEST_STATE_RUNNING, GuestOs: sandrunv1.GuestOs_GUEST_OS_WINDOWS, WorkspacePath: `C:\work`,
	}}), nil
}

func (s *downloadProbeSession) ExecDevSession(_ context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	path, probing := req.Msg.GetEnv()["SANDRUN_DOWNLOAD_PATH"]
	select {
	case s.firstExecPath <- path:
	default:
	}
	// Report a missing file to stop the download at the probe, then allow cleanup.
	var code int64
	if probing {
		code = exitNotFound
	}
	return stream.Send(&sandrunv1.ExecDevSessionResponse{Event: &sandrunv1.DevSessionExecEvent{
		Event: &sandrunv1.DevSessionExecEvent_Exited{Exited: &sandrunv1.DevSessionExecExited{
			Status: &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: code},
		}},
	}})
}
