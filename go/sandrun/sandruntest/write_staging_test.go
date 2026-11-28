// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestWriteDevSessionFileAllocatesDistinctStages(t *testing.T) {
	parent := t.TempDir()
	name := strings.Repeat("a", 200)
	destination := filepath.Join(parent, name)
	unrelated := filepath.Join(parent, "keep.txt")
	writeFixtureFile(t, unrelated, "keep me")

	first, err := stageWrite(destination, []byte("first"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	second, err := stageWrite(destination, []byte("second"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both attempts returned the same staging path %q", first)
	}
	for _, stage := range []string{first, second} {
		if filepath.Dir(stage) != parent || len(filepath.Base(stage)) >= len(name) {
			t.Errorf("staging path %q must have a short name in %q", stage, parent)
		}
	}
	requireFixtureFile(t, first, "first", 0o600)
	requireFixtureFile(t, second, "second", 0o600)
	requireFixtureFile(t, unrelated, "keep me", 0o600)
}

func TestWriteDevSessionFileFinalization(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("write test").GetOrganizationId()
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)

	for _, test := range []struct {
		name        string
		mode        uint32
		wantMode    os.FileMode
		blockRename bool
	}{
		{name: "sticky mode", mode: 0o1640, wantMode: fs.ModeSticky | 0o640},
		{name: "sticky-only mode", mode: 0o1000, wantMode: fs.ModeSticky},
		{name: "explicit mode", mode: 0o640, wantMode: 0o640},
		{name: "default mode", wantMode: 0o644},
		{name: "retry after rename failure", wantMode: 0o644, blockRename: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			parent := filepath.Join(workspace, "nested")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.wantMode&fs.ModeSticky != 0 {
				requireStickyModeSupport(t, parent, test.wantMode)
			}
			profile := s.AddDevSessionProfile(org, "write", sandrunv1.GuestOs_GUEST_OS_LINUX, workspace)
			session, err := client.CreateDevSession(t.Context(), listTestRequest(&sandrunv1.CreateDevSessionRequest{
				OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
			}))
			if err != nil {
				t.Fatal(err)
			}

			const name, first, last = "fixture.txt", "abc", "def"
			const siblingName = name + ".sandruntest-write"
			destination := filepath.Join(parent, name)
			sibling := filepath.Join(parent, siblingName)
			writeFixtureFile(t, sibling, "keep sibling")
			oldFile := destination
			if test.blockRename {
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				oldFile = filepath.Join(destination, "keep.txt")
			}
			writeFixtureFile(t, oldFile, "old bytes")
			requireNoStage := func() {
				t.Helper()
				entries, err := os.ReadDir(parent)
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				if !slices.Equal(names, []string{name, siblingName}) {
					t.Errorf("destination directory contains %q, want only the destination and its original sibling", names)
				}
			}
			write := func(offset int64, data string, final bool) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
				return client.WriteDevSessionFile(t.Context(), listTestRequest(&sandrunv1.WriteDevSessionFileRequest{
					OrganizationId: org, DevSessionId: session.Msg.GetDevSession().GetDevSessionId(),
					WriteId: "write", Path: filepath.Join("nested", name),
					Offset: offset, Data: []byte(data), Mode: test.mode, Final: final,
				}))
			}

			res, err := write(0, first, false)
			if err != nil || res.Msg.GetFinished() {
				t.Fatalf("first chunk returned %v, %v, want an unfinished write", res, err)
			}
			requireFixtureFile(t, oldFile, "old bytes", 0o600)
			requireFixtureFile(t, sibling, "keep sibling", 0o600)
			requireNoStage()

			res, err = write(int64(len(first)), last, true)
			if test.blockRename {
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("blocked finalization returned %v, want failed_precondition", err)
				}
				requireFixtureFile(t, oldFile, "old bytes", 0o600)
				requireFixtureFile(t, sibling, "keep sibling", 0o600)
				requireNoStage()
				if err := os.RemoveAll(destination); err != nil {
					t.Fatal(err)
				}
				res, err = write(int64(len(first)), last, true)
			}
			if err != nil || !res.Msg.GetFinished() || res.Msg.GetSizeBytes() != int64(len(first+last)) {
				t.Fatalf("final chunk returned %v, %v, want a finished write of %d bytes", res, err, len(first+last))
			}
			replay, err := write(int64(len(first)), last, true)
			if err != nil || !proto.Equal(replay.Msg, res.Msg) {
				t.Fatalf("final retry returned %v, %v, want %v", replay, err, res.Msg)
			}
			readableMode := test.wantMode
			if readableMode == fs.ModeSticky {
				requireFixtureMode(t, destination, readableMode)
				// Grant owner read permission only after checking the published mode.
				readableMode |= 0o400
				if err := os.Chmod(destination, readableMode); err != nil {
					t.Fatal(err)
				}
			}
			requireFixtureFile(t, destination, first+last, readableMode)
			requireFixtureFile(t, sibling, "keep sibling", 0o600)
			requireNoStage()
		})
	}
}

func writeFixtureFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireFixtureFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != contents {
		t.Fatalf("read %q returned %q, %v, want %q", path, data, err, contents)
	}
	if runtime.GOOS != "windows" {
		requireFixtureMode(t, path, mode)
	}
}

func requireFixtureMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode() & (fs.ModePerm | fs.ModeSticky); mode != want {
		t.Fatalf("%q has mode %v, want %v", path, mode, want)
	}
}

func requireStickyModeSupport(t *testing.T, parent string, mode fs.FileMode) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the fixture applies the sticky bit on Linux and macOS")
	}
	control := filepath.Join(parent, "control")
	writeFixtureFile(t, control, "")
	if err := os.Chmod(control, mode); err != nil {
		t.Skipf("control chmod: %v", err)
	}
	info, err := os.Stat(control)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(fs.ModePerm|fs.ModeSticky) != mode {
		t.Skipf("filesystem does not retain mode %v on a regular file", mode)
	}
	if err := os.Remove(control); err != nil {
		t.Fatal(err)
	}
}
