// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestDownloadProgress(t *testing.T) {
	const contents = "xxx"
	archive, err := io.ReadAll(tarOf(t, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(contents))}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		probe         string
		data          []byte
		endSize       int64
		directory     bool
		wantReadError bool
		wantContents  string
	}{
		{name: "growing", probe: "file 1", data: []byte(contents), endSize: 3, wantContents: contents},
		{name: "shrinking", probe: "file 5", data: []byte(contents), endSize: 3, wantContents: contents},
		{name: "unchanged", probe: "file 3", data: []byte(contents), endSize: 3, wantContents: contents},
		{name: "empty", probe: "file 1"},
		{name: "directory", probe: "dir 1", data: archive, endSize: int64(len(archive)), directory: true, wantContents: contents},
		{name: "mismatched end", probe: "file 3", data: []byte(contents), endSize: 4, wantReadError: true, wantContents: "previous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunks [][]byte
			want := []TransferProgress{{Phase: TransferPreparing}}
			if len(tc.data) != 0 {
				split := len(tc.data) / 2
				chunks = [][]byte{tc.data[:split], tc.data[split:]}
				want = append(want,
					TransferProgress{Phase: TransferDownloading, BytesDone: int64(split)},
					TransferProgress{Phase: TransferDownloading, BytesDone: int64(len(tc.data))},
				)
			}
			if tc.directory {
				want = append(want, TransferProgress{Phase: TransferApplying, BytesDone: tc.endSize, BytesTotal: tc.endSize})
			}
			if !tc.wantReadError {
				want = append(want, TransferProgress{Phase: TransferDone, BytesDone: tc.endSize, BytesTotal: tc.endSize})
			}
			client := newScriptedDownloadClient(t, &scriptedDownloadSession{probe: tc.probe, chunks: chunks, endSize: tc.endSize})

			dest := filepath.Join(t.TempDir(), "result")
			if err := os.WriteFile(dest, []byte("previous"), 0o644); err != nil {
				t.Fatal(err)
			}
			var got []TransferProgress
			result, err := client.Download(t.Context(), DownloadParams{
				OrganizationID: "org", DevSessionID: "session", GuestPath: "source", LocalPath: dest,
				Progress: func(p TransferProgress) { got = append(got, p) },
			})
			if tc.wantReadError {
				if result != nil || err == nil || !strings.Contains(err.Error(), "ended at byte 3 of 4") {
					t.Fatalf("download = %+v, %v", result, err)
				}
			} else if err != nil || result == nil || result.Directory != tc.directory || result.Files != 1 || result.Bytes != int64(len(tc.wantContents)) {
				t.Fatalf("download = %+v, %v", result, err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("progress = %+v, want %+v", got, want)
			}

			if tc.directory {
				dest = filepath.Join(dest, "file")
			}
			if data, err := os.ReadFile(dest); err != nil || string(data) != tc.wantContents {
				t.Errorf("destination = %q, %v, want %q", data, err, tc.wantContents)
			}
		})
	}
}

func newScriptedDownloadClient(t *testing.T, fake *scriptedDownloadSession) *Client {
	t.Helper()
	_, handler := sandrunv1connect.NewDevSessionServiceHandler(fake)
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	client, err := New(Options{BaseURL: server.URL, APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type scriptedDownloadSession struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	probe   string
	chunks  [][]byte
	endSize int64
	mu      sync.Mutex
	path    string
}

func (s *scriptedDownloadSession) GetDevSession(context.Context, *connect.Request[sandrunv1.GetDevSessionRequest]) (*connect.Response[sandrunv1.GetDevSessionResponse], error) {
	return connect.NewResponse(&sandrunv1.GetDevSessionResponse{DevSession: &sandrunv1.DevSession{
		State: sandrunv1.RequestState_REQUEST_STATE_RUNNING, GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/fixture",
	}}), nil
}

func (s *scriptedDownloadSession) ExecDevSession(_ context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	send := func(event *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.ExecDevSessionResponse{Event: event})
	}
	if err := send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Started{
		Started: &sandrunv1.DevSessionExecStarted{ExecId: req.Msg.GetExecId()},
	}}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if env := req.Msg.GetEnv(); env[guestEnvPath] != "" {
		s.path = env[guestEnvPath]
		if strings.HasPrefix(s.probe, "dir ") {
			s.path = env[guestEnvDLArchive]
		}
		if err := send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
			Stdout: &sandrunv1.DevSessionExecOutput{Data: []byte(s.probe + "\n")},
		}}); err != nil {
			return err
		}
	} else if env[guestEnvDLArchive] == s.path {
		s.path = ""
	}
	return send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
		Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: 0}},
	}})
}

func (s *scriptedDownloadSession) ReadDevSessionFile(_ context.Context, req *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || s.path != req.Msg.GetPath() {
		return connect.NewError(connect.CodeNotFound, errors.New("fixture download path not found"))
	}
	var offset int64
	for _, data := range s.chunks {
		if err := stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_Chunk{
			Chunk: &sandrunv1.DevSessionFileChunk{Offset: offset, Data: data},
		}}); err != nil {
			return err
		}
		offset += int64(len(data))
	}
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_End{
		End: &sandrunv1.DevSessionFileEnd{SizeBytes: s.endSize},
	}})
}
