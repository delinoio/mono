// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestDownloadDestinationCaseCollision(t *testing.T) {
	const first, second = "Report.txt", "report.txt"
	const firstContent, secondContent = "first", "second"
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, file := range []struct{ name, content string }{{first, firstContent}, {second, secondContent}} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	fake := &downloadCaseSession{archive: archive.Bytes()}
	_, handler := sandrunv1connect.NewDevSessionServiceHandler(fake)
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetHTTP1(true)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	defer server.Close()
	client, err := New(Options{BaseURL: server.URL, APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	probeDir := filepath.Join(parent, "probe")
	if err := os.Mkdir(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, first), []byte(firstContent), 0o644); err != nil {
		t.Fatal(err)
	}
	aliasContents, err := os.ReadFile(filepath.Join(probeDir, second))
	aliases := err == nil
	if aliases && string(aliasContents) != firstContent || !aliases && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filesystem case probe = %q, %v", aliasContents, err)
	}
	t.Logf("the destination volume aliases %q and %q: %t", first, second, aliases)

	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dest := filepath.Join(parent, fmt.Sprintf("result-%t", existing))
			if existing {
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, "keep.txt"), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res, err := client.Download(t.Context(), DownloadParams{OrganizationID: "org", DevSessionID: "session", GuestPath: "out", LocalPath: dest})
			if aliases {
				if res != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition ||
					!strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
					t.Fatalf("case collision = %+v, %v", res, err)
				}
				if existing {
					checkDownloadedFiles(t, dest, []string{"keep.txt"}, []string{"old"})
				} else if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed download changed absent destination: %v", err)
				}
			} else {
				if err != nil || res == nil || !res.Directory || res.Files != 2 || res.Bytes != 11 {
					t.Fatalf("case-distinct download = %+v, %v", res, err)
				}
				checkDownloadedFiles(t, dest, []string{first, second}, []string{firstContent, secondContent})
			}

			if matches, err := filepath.Glob(filepath.Join(parent, ".sandrun-download-*")); err != nil || len(matches) != 0 {
				t.Fatalf("local staging remains: %v, %v", matches, err)
			}
			fake.mu.Lock()
			guestArchive := fake.path
			fake.mu.Unlock()
			if guestArchive != "" {
				t.Fatalf("guest archive remains: %s", guestArchive)
			}
		})
	}
}

func checkDownloadedFiles(t *testing.T, dir string, names, contents []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, names) {
		t.Fatalf("destination entries = %v, want %v", got, names)
	}
	for i, name := range names {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != contents[i] {
			t.Errorf("%s = %q, %v, want %q", name, data, err, contents[i])
		}
	}
}

type downloadCaseSession struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	archive []byte
	mu      sync.Mutex
	path    string
}

func (s *downloadCaseSession) GetDevSession(context.Context, *connect.Request[sandrunv1.GetDevSessionRequest]) (*connect.Response[sandrunv1.GetDevSessionResponse], error) {
	return connect.NewResponse(&sandrunv1.GetDevSessionResponse{DevSession: &sandrunv1.DevSession{
		State: sandrunv1.RequestState_REQUEST_STATE_RUNNING, GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/fixture",
	}}), nil
}

func (s *downloadCaseSession) ExecDevSession(_ context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
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
		s.path = env[guestEnvDLArchive]
		if err := send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
			Stdout: &sandrunv1.DevSessionExecOutput{Data: fmt.Appendf(nil, "dir %d\n", len(s.archive))},
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

func (s *downloadCaseSession) ReadDevSessionFile(_ context.Context, req *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || s.path != req.Msg.GetPath() {
		return connect.NewError(connect.CodeNotFound, errors.New("fixture archive not found"))
	}
	if err := stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_Chunk{
		Chunk: &sandrunv1.DevSessionFileChunk{Data: s.archive},
	}}); err != nil {
		return err
	}
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_End{
		End: &sandrunv1.DevSessionFileEnd{SizeBytes: int64(len(s.archive))},
	}})
}

func TestDownloadArchiveRepeatedName(t *testing.T) {
	dir := t.TempDir()
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Name: "./file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, dir, []string{"file"}, []string{"xxx"})
}

func TestDownloadArchiveDistinctHardLink(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if !caseSensitiveDir(t, probe) {
		t.Skip("this destination volume does not store both names separately")
	}
	if err := os.Link(filepath.Join(probe, "case"), filepath.Join(probe, "CASE")); err != nil {
		t.Skipf("this destination volume does not support hard links: %v", err)
	}

	// The second regular entry must replace just the distinct hard link,
	// even though it shares the original file's identity before replacement.
	result := filepath.Join(dir, "result")
	if err := os.Mkdir(result, 0o755); err != nil {
		t.Fatal(err)
	}
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	_, err := extractor.extractArchive(t.Context(), tarOf(t,
		&tar.Header{Name: "Report.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
		&tar.Header{Name: "report.txt", Typeflag: tar.TypeLink, Linkname: "Report.txt"},
		&tar.Header{Name: "report.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	), result)
	if err != nil {
		t.Fatal(err)
	}
	checkDownloadedFiles(t, result, []string{"Report.txt", "report.txt"}, []string{"xx", "xxx"})
}
