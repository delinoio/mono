// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestFakeReadBinding(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("read binding").GetOrganizationId()
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	ctx := t.Context()
	newSession := func() *sandrunv1.DevSession {
		t.Helper()
		profile := s.AddDevSessionProfile(org, "read", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
		res, err := client.CreateDevSession(ctx, listTestRequest(&sandrunv1.CreateDevSessionRequest{
			OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDevSession()
	}
	writeFixture := func(session *sandrunv1.DevSession, path, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(session.GetWorkspacePath(), path), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	type readResult struct {
		data   string
		events int
		size   int64
		err    error
	}
	read := func(session *sandrunv1.DevSession, id, path string, offset int64) readResult {
		result := readResult{size: -1}
		stream, err := client.ReadDevSessionFile(ctx, listTestRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: org, DevSessionId: session.GetDevSessionId(), ReadId: id, Path: path, Offset: offset,
		}))
		if err != nil {
			result.err = err
			return result
		}
		defer stream.Close()
		for stream.Receive() {
			result.events++
			event := stream.Msg()
			if chunk := event.GetChunk(); chunk != nil {
				result.data += string(chunk.GetData())
			}
			if end := event.GetEnd(); end != nil {
				result.size = end.GetSizeBytes()
			}
		}
		result.err = stream.Err()
		return result
	}
	checkRead := func(t *testing.T, got readResult, want string, size int64) {
		t.Helper()
		if got.err != nil || got.data != want || got.size != size {
			t.Fatalf("read returned %+v, want %q and end size %d", got, want, size)
		}
	}
	checkRejected := func(t *testing.T, got readResult, code connect.Code) {
		t.Helper()
		if connect.CodeOf(got.err) != code || got.events != 0 {
			t.Fatalf("read returned %+v, want %v and zero events", got, code)
		}
	}

	const (
		path, otherPath = "fixture.txt", "other.txt"
		original        = "original snapshot"
		replacement     = "replaced file"
		independent     = "second session"
	)
	first, second := newSession(), newSession()
	writeFixture(first, path, original)
	writeFixture(first, otherPath, "different path")
	writeFixture(second, path, independent)
	const retainedID = "retained-read"
	checkRead(t, read(first, retainedID, path, 0), original, int64(len(original)))

	t.Run("different session", func(t *testing.T) {
		checkRejected(t, read(second, retainedID, path, 0), connect.CodeAlreadyExists)
		checkRead(t, read(second, "independent-read", path, 0), independent, int64(len(independent)))
	})
	t.Run("different path", func(t *testing.T) {
		checkRejected(t, read(first, retainedID, otherPath, 0), connect.CodeFailedPrecondition)
	})
	writeFixture(first, path, replacement)
	t.Run("resume original snapshot", func(t *testing.T) {
		const offset = 4
		absolute := filepath.Join(first.GetWorkspacePath(), path)
		checkRead(t, read(first, retainedID, absolute, offset), original[offset:], int64(len(original)))
	})
	t.Run("snapshot size boundary", func(t *testing.T) {
		size := int64(len(original))
		atEnd := read(first, retainedID, path, size)
		checkRead(t, atEnd, "", size)
		if atEnd.events != 1 {
			t.Fatalf("read at size sent %d events, want one end event", atEnd.events)
		}
		checkRejected(t, read(first, retainedID, path, size+1), connect.CodeInvalidArgument)
	})

	t.Run("concurrent first reads", func(t *testing.T) {
		const id = "concurrent-read"
		sessions := [2]*sandrunv1.DevSession{first, second}
		var results [2]readResult
		var requests sync.WaitGroup
		start := make(chan struct{})
		for i, session := range sessions {
			requests.Go(func() {
				<-start
				results[i] = read(session, id, path, 0)
			})
		}
		close(start)
		requests.Wait()

		// Until the minimum Go version includes the fix for golang/go#80127,
		// select the expected string directly to avoid an ARM64 stack slot overlap.
		winner, loser := 0, 1
		want := replacement
		if results[0].err != nil {
			winner, loser = 1, 0
			want = independent
		}
		size := int64(len(want))
		checkRead(t, results[winner], want, size)
		checkRejected(t, results[loser], connect.CodeAlreadyExists)
		checkRead(t, read(sessions[winner], id, path, 1), want[1:], size)
	})

	t.Run("negative offsets", func(t *testing.T) {
		checkNegative := func(t *testing.T, id, path string) {
			t.Helper()
			before := len(s.ReadRequests())
			checkRejected(t, read(first, id, path, -1), connect.CodeInvalidArgument)
			requests := s.ReadRequests()[before:]
			if len(requests) != 1 {
				t.Fatalf("recorded %d requests, want one", len(requests))
			}
			got := requests[0]
			if got.GetReadId() != id || got.GetPath() != path || got.GetOffset() != -1 {
				t.Fatalf("recorded request %v, want read ID %q, path %q and offset -1", got, id, path)
			}
		}
		t.Run("retry on another path", func(t *testing.T) {
			const id = "negative-read-other-path"
			checkNegative(t, id, path)
			checkRead(t, read(first, id, otherPath, 0), "different path", int64(len("different path")))
		})
		t.Run("retry after file changes", func(t *testing.T) {
			const id = "negative-read-edited-file"
			checkNegative(t, id, path)
			writeFixture(first, path, "new")
			checkRead(t, read(first, id, path, 0), "new", 3)
		})
		t.Run("missing path", func(t *testing.T) {
			checkNegative(t, "negative-read-missing-path", "missing.txt")
		})
	})
}
