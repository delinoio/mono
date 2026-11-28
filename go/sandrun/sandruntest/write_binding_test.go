// SPDX-License-Identifier: Apache-2.0

package sandruntest_test

import (
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/sandrun/sandruntest"
	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestFakeWriteBinding(t *testing.T) {
	server := sandruntest.New()
	t.Cleanup(server.Close)
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, server.URL)
	orgID := server.AddOrganization("test").GetOrganizationId()

	newSession := func(t *testing.T) *sandrunv1.DevSession {
		t.Helper()
		profile := server.AddDevSessionProfile(orgID, "test", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
		req := connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
			OrganizationId:  orgID,
			RunnerProfileId: profile.GetRunnerProfileId(),
		})
		req.Header().Set("Authorization", "Bearer "+server.APIKey)
		res, err := client.CreateDevSession(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDevSession()
	}
	sendWrite := func(t *testing.T, message *sandrunv1.WriteDevSessionFileRequest) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		t.Helper()
		req := connect.NewRequest(message)
		req.Header().Set("Authorization", "Bearer "+server.APIKey)
		return client.WriteDevSessionFile(t.Context(), req)
	}
	write := func(t *testing.T, session *sandrunv1.DevSession, writeID, path string, offset int64, data string, final bool) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		t.Helper()
		return sendWrite(t, &sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: orgID,
			DevSessionId:   session.GetDevSessionId(),
			WriteId:        writeID,
			Path:           path,
			Offset:         offset,
			Data:           []byte(data),
			Final:          final,
		})
	}
	checkChunk := func(t *testing.T, session *sandrunv1.DevSession, writeID, path string, offset int64, data string, final bool) {
		t.Helper()
		res, err := write(t, session, writeID, path, offset, data, final)
		if err != nil {
			t.Fatalf("write %s at offset %d: %v", path, offset, err)
		}
		wantSize := int64(0)
		if final {
			wantSize = offset + int64(len(data))
		}
		m := res.Msg
		if m.GetWriteId() != writeID || m.GetOffset() != offset || m.GetLength() != int64(len(data)) ||
			m.GetFinished() != final || m.GetSizeBytes() != wantSize {
			t.Fatalf("write %s at offset %d returned %v, want length %d, finished %t, size %d",
				path, offset, m, len(data), final, wantSize)
		}
	}
	checkFile := func(t *testing.T, session *sandrunv1.DevSession, path, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(session.GetWorkspacePath(), path))
		if err != nil || string(got) != want {
			t.Fatalf("read %s = %q, %v, want %q", path, got, err, want)
		}
	}
	checkAbsent := func(t *testing.T, session *sandrunv1.DevSession, path string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(session.GetWorkspacePath(), path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: got %v, want file absent", path, err)
		}
	}

	t.Run("staged ranges", func(t *testing.T) {
		type chunk struct {
			offset int64
			data   string
			final  bool
			fail   bool
			file   string
		}
		for _, tc := range []struct {
			name  string
			start string
			steps []chunk
		}{
			{
				name: "out of order",
				steps: []chunk{
					{offset: 1, data: "B"},
					{offset: 0, data: "A"},
					{offset: 2, final: true, file: "AB"},
				},
			},
			{
				name: "matching overlap extends coverage and retries",
				steps: []chunk{
					{data: "AB"},
					{offset: 1, data: "BC"},
					{offset: 1, data: "BC"},
					{offset: 3, final: true, file: "ABC"},
				},
			},
			{
				name: "conflicting overlap leaves data intact",
				steps: []chunk{
					{data: "AB"},
					{offset: 1, data: "XC", fail: true},
					{offset: 1, data: "BC"},
					{offset: 3, final: true, file: "ABC"},
				},
			},
			{
				name:  "failed final chunk stays staged",
				start: "old",
				steps: []chunk{
					{offset: 1, data: "B", final: true, fail: true, file: "old"},
					{data: "A", file: "old"},
					{offset: 2, final: true, file: "AB"},
				},
			},
			{
				name: "final offset excludes staged bytes",
				steps: []chunk{
					{data: "ABC"},
					{offset: 2, final: true, fail: true},
					{offset: 3, final: true, file: "ABC"},
				},
			},
			{
				name: "chunk bridges multiple ranges only when all overlaps match",
				steps: []chunk{
					{data: "AB"},
					{offset: 4, data: "EF"},
					{offset: 8, data: "IJK"},
					{offset: 1, data: "BCDEFGHX", fail: true},
					{offset: 11, final: true, fail: true},
					{offset: 1, data: "BCDEFGHI"},
					{offset: 11, final: true, file: "ABCDEFGHIJK"},
				},
			},
			{
				name: "sparse chunk ends at maximum offset",
				steps: []chunk{
					{offset: math.MaxInt64 - 1, data: "X"},
					{offset: math.MaxInt64 - 1, data: "X"},
					{offset: math.MaxInt64, final: true, fail: true},
				},
			},
			{
				name: "empty chunks add no coverage",
				steps: []chunk{
					{offset: math.MaxInt64},
					{offset: 1, final: true, fail: true},
					{final: true},
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				session := newSession(t)
				id, err := uuidv7.NewString()
				if err != nil {
					t.Fatal(err)
				}
				const path = "out.txt"
				if tc.start != "" {
					if err := os.WriteFile(filepath.Join(session.GetWorkspacePath(), path), []byte(tc.start), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for _, step := range tc.steps {
					if step.fail {
						if _, err := write(t, session, id, path, step.offset, step.data, step.final); connect.CodeOf(err) != connect.CodeFailedPrecondition {
							t.Fatalf("write at offset %d returned %v, want failed_precondition", step.offset, err)
						}
					} else {
						checkChunk(t, session, id, path, step.offset, step.data, step.final)
					}
					if step.file == "" && (!step.final || step.fail) {
						checkAbsent(t, session, path)
					} else {
						checkFile(t, session, path, step.file)
					}
				}
			})
		}
	})

	t.Run("completed write in another session", func(t *testing.T) {
		first, second := newSession(t), newSession(t)
		const id, path = "shared-completed", "fixture.txt"

		checkChunk(t, first, id, path, 0, "a", true)
		checkChunk(t, second, id, path, 0, "xyz", true)
		checkFile(t, first, path, "a")
		checkFile(t, second, path, "xyz")
	})

	t.Run("pending writes in both sessions", func(t *testing.T) {
		first, second := newSession(t), newSession(t)
		const id, path = "shared-pending", "fixture.txt"

		checkChunk(t, first, id, path, 0, "a", false)
		checkChunk(t, second, id, path, 0, "xy", false)
		checkChunk(t, first, id, path, 1, "b", true)
		checkChunk(t, second, id, path, 2, "z", true)
		checkFile(t, first, path, "ab")
		checkFile(t, second, path, "xyz")
	})

	t.Run("changed destination", func(t *testing.T) {
		session := newSession(t)
		const id, original, changed = "path-bound", "first.txt", "second.txt"
		checkRejected := func() {
			t.Helper()
			if _, err := write(t, session, id, changed, 1, "x", true); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("write to changed path returned %v, want invalid_argument", err)
			}
		}

		checkChunk(t, session, id, original, 0, "a", false)
		checkRejected()
		checkAbsent(t, session, original)
		checkAbsent(t, session, changed)

		absolute := filepath.Join(session.GetWorkspacePath(), original)
		checkChunk(t, session, id, absolute, 1, "b", true)
		checkChunk(t, session, id, absolute, 1, "b", true)
		checkRejected()
		checkFile(t, session, original, "ab")
		checkAbsent(t, session, changed)
	})

	t.Run("completed retry", func(t *testing.T) {
		session := newSession(t)
		const id, path = "retry-completed", "fixture.txt"
		checkChunk(t, session, id, path, 0, "ab", false)
		final := &sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: orgID,
			DevSessionId:   session.GetDevSessionId(),
			WriteId:        id,
			Path:           path,
			Offset:         2,
			Data:           []byte("cd"),
			Mode:           0o644,
			Final:          true,
		}
		completed, err := sendWrite(t, final)
		if err != nil {
			t.Fatal(err)
		}
		want := &sandrunv1.WriteDevSessionFileResponse{WriteId: id, Offset: 2, Length: 2, Finished: true, SizeBytes: 4}
		if !proto.Equal(completed.Msg, want) {
			t.Fatalf("final response = %v, want %v", completed.Msg, want)
		}
		checkFile(t, session, path, "abcd")

		for _, tc := range []struct {
			name   string
			change func(*sandrunv1.WriteDevSessionFileRequest)
		}{
			{"non-final", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Final = false }},
			{"mode", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Mode = 0o600 }},
			{"raw mode with same permissions", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Mode = 0o10644 }},
			{"offset", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Offset = 1 }},
			{"length", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Data = []byte("c") }},
			{"data", func(m *sandrunv1.WriteDevSessionFileRequest) { m.Data = []byte("xy") }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				retry := proto.CloneOf(final)
				tc.change(retry)
				if _, err := sendWrite(t, retry); connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Errorf("retry error = %v, want failed_precondition", err)
				}
				checkFile(t, session, path, "abcd")
			})
		}

		if err := os.WriteFile(filepath.Join(session.GetWorkspacePath(), path), []byte("edited"), 0o600); err != nil {
			t.Fatal(err)
		}
		replayed, err := sendWrite(t, final)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(replayed.Msg, completed.Msg) {
			t.Fatalf("replayed response = %v, want %v", replayed.Msg, completed.Msg)
		}
		checkFile(t, session, path, "edited")
	})
}
