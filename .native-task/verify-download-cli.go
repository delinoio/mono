package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type entry struct {
	name, data string
	kind       byte
}

type downloadCase struct {
	label                  string
	entries                []entry
	files                  int
	bytes                  int64
	links, reads           map[string]string
	missingNames, warnings []string
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		log.Fatal("usage: go run verify-download-cli.go CLI [denied]")
	}
	cli, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := run(cli, len(os.Args) == 3 && os.Args[2] == "denied"); err != nil {
		log.Fatal(err)
	}
}

func run(cli string, denied bool) error {
	scratch, err := os.MkdirTemp(filepath.Dir(cli), "download-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	link := entry{"link", "dir", tar.TypeSymlink}
	dir := entry{"dir/", "", tar.TypeDir}
	file := entry{"dir/file.txt", "x", tar.TypeReg}
	var cases []downloadCase
	if denied {
		cases = []downloadCase{{
			label: "denied", entries: []entry{link, dir, file}, files: 1, bytes: 1,
			reads:        map[string]string{"dir/file.txt": "x"},
			missingNames: []string{"link"}, warnings: []string{"link: the symbolic link could not be created"},
		}}
	} else {
		links := map[string]string{"link": "dir"}
		reads := map[string]string{"dir/file.txt": "x", "link/file.txt": "x"}
		cases = []downloadCase{
			{label: "forward", entries: []entry{link, dir, file}, files: 2, bytes: 1, links: links, reads: reads},
			{label: "backward", entries: []entry{dir, link, file}, files: 2, bytes: 1, links: links, reads: reads},
			{label: "chain", files: 7, bytes: 3,
				entries: []entry{
					{"a-link", "z-link/sub", tar.TypeSymlink},
					{"z-link", "dir", tar.TypeSymlink},
					{"file-link", "dir/sub/file.txt", tar.TypeSymlink},
					{"dangling", "dir/missing", tar.TypeSymlink},
					{"replaced", "dir", tar.TypeSymlink},
					{"replaced", "xx", tar.TypeReg},
					{"dir/sub/file.txt", "x", tar.TypeReg},
				},
				links:        map[string]string{"a-link": "z-link/sub", "z-link": "dir", "file-link": "dir/sub/file.txt", "dangling": "dir/missing"},
				reads:        map[string]string{"a-link/file.txt": "x", "z-link/sub/file.txt": "x", "file-link": "x", "replaced": "xx"},
				missingNames: []string{"dangling"},
			},
		}
	}

	for _, tc := range cases {
		if err := download(cli, scratch, tc); err != nil {
			return fmt.Errorf("%s: %w", tc.label, err)
		}
	}
	return nil
}

func download(cli, scratch string, tc downloadCase) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range tc.entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: e.kind}
		switch e.kind {
		case tar.TypeReg:
			h.Size = int64(len(e.data))
		case tar.TypeSymlink:
			h.Linkname = e.data
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if e.kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	fake := &scriptedDownloadSession{
		probe:   fmt.Sprintf("dir %d", buf.Len()),
		chunks:  [][]byte{buf.Bytes()},
		endSize: int64(buf.Len()),
	}
	_, handler := sandrunv1connect.NewDevSessionServiceHandler(fake)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	defer srv.Close()

	dest := filepath.Join(scratch, tc.label)
	if err := os.Mkdir(dest, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, "previous"), []byte("old"), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "--output", "json", "session", "download", "session", "out", dest)
	cmd.Env = append(os.Environ(), "SANDRUN_API_URL="+srv.URL, "SANDRUN_API_KEY=local-fixture", "SANDRUN_ORG=fixture-org", "NO_COLOR=1")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("CLI: %w", err)
	}

	var res struct {
		Directory bool
		Files     int
		Bytes     int64
		Warnings  []string
	}
	if err := json.Unmarshal(output, &res); err != nil {
		return err
	}
	if !res.Directory || res.Files != tc.files || res.Bytes != tc.bytes || !slices.Equal(res.Warnings, tc.warnings) {
		return fmt.Errorf("result = %s", output)
	}
	for name, want := range tc.links {
		target, err := os.Readlink(filepath.Join(dest, name))
		if err != nil || filepath.ToSlash(target) != want {
			return fmt.Errorf("readlink %s = %q, %v, want %q", name, target, err, want)
		}
	}
	for name, want := range tc.reads {
		contents, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(contents) != want {
			return fmt.Errorf("read %s = %q, %v, want %q", name, contents, err, want)
		}
	}
	for _, name := range append(tc.missingNames, "previous") {
		if _, err := os.Stat(filepath.Join(dest, name)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s = %v, want missing", name, err)
		}
	}
	if tc.label == "denied" {
		if _, err := os.Lstat(filepath.Join(dest, "link")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("omitted link = %v, want absent entry", err)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(scratch, ".sandrun-download-*")); err != nil || len(matches) != 0 {
		return fmt.Errorf("staging remains: %v, %v", matches, err)
	}
	fmt.Printf("%s: files=%d bytes=%d warnings=%v relativeLinks=%v nativeReads=%v absentTargets=%v\n",
		tc.label, res.Files, res.Bytes, res.Warnings, tc.links, tc.reads, tc.missingNames)
	return nil
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
	if env := req.Msg.GetEnv(); env["SANDRUN_DOWNLOAD_PATH"] != "" {
		s.path = env["SANDRUN_DOWNLOAD_PATH"]
		if strings.HasPrefix(s.probe, "dir ") {
			s.path = env["SANDRUN_DOWNLOAD_ARCHIVE"]
		}
		if err := send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
			Stdout: &sandrunv1.DevSessionExecOutput{Data: []byte(s.probe + "\n")},
		}}); err != nil {
			return err
		}
	} else if env["SANDRUN_DOWNLOAD_ARCHIVE"] == s.path {
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
