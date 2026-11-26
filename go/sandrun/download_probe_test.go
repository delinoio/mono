// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

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
