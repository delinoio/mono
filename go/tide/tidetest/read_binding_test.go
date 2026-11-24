// SPDX-License-Identifier: Apache-2.0

package tidetest

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

func TestFakeReadBinding(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("read binding").GetOrganizationId()
	client := tidev1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	ctx := t.Context()
	newSession := func() *tidev1.DevSession {
		t.Helper()
		profile := s.AddDevSessionProfile(org, "read", tidev1.GuestOs_GUEST_OS_LINUX, t.TempDir())
		res, err := client.CreateDevSession(ctx, listTestRequest(&tidev1.CreateDevSessionRequest{
			OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
		}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetDevSession()
	}
	writeFixture := func(session *tidev1.DevSession, path, contents string) {
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
	read := func(session *tidev1.DevSession, id, path string, offset int64) readResult {
		result := readResult{size: -1}
		stream, err := client.ReadDevSessionFile(ctx, listTestRequest(&tidev1.ReadDevSessionFileRequest{
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

	t.Run("concurrent first reads", func(t *testing.T) {
		const id = "concurrent-read"
		sessions := [2]*tidev1.DevSession{first, second}
		want := [2]string{replacement, independent}
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

		winner, loser := 0, 1
		if results[0].err != nil {
			winner, loser = 1, 0
		}
		checkRead(t, results[winner], want[winner], int64(len(want[winner])))
		checkRejected(t, results[loser], connect.CodeAlreadyExists)
		checkRead(t, read(sessions[winner], id, path, 1), want[winner][1:], int64(len(want[winner])))
	})
}
