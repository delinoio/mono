// SPDX-License-Identifier: Apache-2.0

package sandruntest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestWriteDevSessionFileRejectsInvalidOffsets(t *testing.T) {
	const writeID, fileName = "write-offset", "out.txt"
	workspace := t.TempDir()
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("write test").GetOrganizationId()
	profile := s.AddDevSessionProfile(org, "write", sandrunv1.GuestOs_GUEST_OS_LINUX, workspace)
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	ctx := t.Context()
	session, err := client.CreateDevSession(ctx, listTestRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	sessionID := session.Msg.GetDevSession().GetDevSessionId()
	write := func(offset int64, data string, final bool) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		return client.WriteDevSessionFile(ctx, listTestRequest(&sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: org, DevSessionId: sessionID, WriteId: writeID, Path: fileName,
			Offset: offset, Data: []byte(data), Final: final,
		}))
	}
	requireInvalid := func(t *testing.T, offset int64, err error, want string) {
		t.Helper()
		var rpcErr *connect.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeInvalidArgument || rpcErr.Message() != want {
			t.Errorf("offset %d: got %v, want InvalidArgument (%s)", offset, err, want)
		}
	}
	checkInvalidOffsets := func(t *testing.T) {
		t.Helper()
		const negative = "invalid offset: must not be negative"
		const overflow = "invalid offset: the chunk end offset exceeds the maximum int64 value"
		for _, chunk := range []struct {
			offset int64
			data   string
			final  bool
			want   string
		}{
			{-1, "x", false, negative},
			{-3, "xyz", true, negative},
			{-5, "0123456789", false, negative},
			{math.MinInt64, "", true, negative},
			{math.MaxInt64, "x", false, overflow},
			{math.MaxInt64 - 1, "xy", true, overflow},
		} {
			_, err := write(chunk.offset, chunk.data, chunk.final)
			requireInvalid(t, chunk.offset, err, chunk.want)
		}
	}
	requireWrite := func(offset int64, data string, final bool) {
		t.Helper()
		res, err := write(offset, data, final)
		if err != nil {
			t.Fatalf("write at offset %d: %v", offset, err)
		}
		if res.Msg.GetFinished() != final || (final && res.Msg.GetSizeBytes() != offset+int64(len(data))) {
			t.Fatalf("unexpected response to write at offset %d: %v", offset, res.Msg)
		}
	}

	t.Run("new write", func(t *testing.T) {
		checkInvalidOffsets(t)
		s.guest.mu.Lock()
		_, exists := s.guest.writes[writeKey{organizationID: org, devSessionID: sessionID, writeID: writeID}]
		s.guest.mu.Unlock()
		if exists {
			t.Error("a rejected request created a pending write")
		}
	})
	requireWrite(0, "abc", false)
	t.Run("pending write", checkInvalidOffsets)
	_, err = write(math.MaxInt64-1, "x", false)
	requireInvalid(t, math.MaxInt64-1, err, fmt.Sprintf("chunk at offset %d does not continue the write at 3", math.MaxInt64-1))
	_, err = write(4, "x", false)
	requireInvalid(t, 4, err, "chunk at offset 4 does not continue the write at 3")
	_, err = write(0, "xyz", false)
	requireInvalid(t, 0, err, "chunk at offset 0 does not continue the write at 3")
	requireWrite(3, "def", false)
	requireWrite(0, "abc", false)
	requireWrite(6, "ghi", true)
	t.Run("finished write", checkInvalidOffsets)
	requireWrite(6, "ghi", true)

	data, err := os.ReadFile(filepath.Join(workspace, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abcdefghi" {
		t.Errorf("got file contents %q, want abcdefghi", data)
	}
}

func TestListDevSessionsFilters(t *testing.T) {
	const (
		running    = sandrunv1.RequestState_REQUEST_STATE_RUNNING
		ended      = sandrunv1.RequestState_REQUEST_STATE_ENDED
		otherOwner = "user-2"
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	s.mu.Lock()
	s.sessions = []*sandrunv1.DevSession{
		{DevSessionId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "second-ended", OrganizationId: org, RunnerProfileId: secondListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "other-owner", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: otherOwner, State: ended},
		{DevSessionId: "first-running", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: running},
		{DevSessionId: "first-ended", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "unspecified", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID},
	}
	s.mu.Unlock()
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *sandrunv1.ListDevSessionsRequest
		want []string
	}{
		{"default open states", &sandrunv1.ListDevSessionsRequest{}, []string{"first-running"}},
		{"profile", &sandrunv1.ListDevSessionsRequest{RunnerProfileId: strings.ToUpper(secondListProfile), IncludeEnded: true}, []string{"second-ended"}},
		{"ended", &sandrunv1.ListDevSessionsRequest{States: []sandrunv1.RequestState{ended}}, []string{"second-ended", "other-owner", "first-ended"}},
		{"ended with include_ended", &sandrunv1.ListDevSessionsRequest{States: []sandrunv1.RequestState{ended}, IncludeEnded: true}, []string{"second-ended", "other-owner", "first-ended"}},
		{
			"caller ownership intersection before pagination",
			&sandrunv1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				States:          []sandrunv1.RequestState{ended},
				Owner:           &sandrunv1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true},
				PageSize:        1,
			},
			[]string{"first-ended"},
		},
		{
			"explicit owner intersection",
			&sandrunv1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				Owner:           &sandrunv1.ListDevSessionsRequest_OwnerUserId{OwnerUserId: otherOwner},
				IncludeEnded:    true,
			},
			[]string{"other-owner"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.OrganizationId = org
			res, err := client.ListDevSessions(t.Context(), listTestRequest(tt.req))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range res.Msg.GetDevSessions() {
				ids = append(ids, row.GetDevSessionId())
			}
			if !slices.Equal(ids, tt.want) || res.Msg.GetNextPageToken() != "" {
				t.Errorf("got IDs %v and next token %q, want IDs %v and no next token", ids, res.Msg.GetNextPageToken(), tt.want)
			}
		})
	}
	_, err := client.ListDevSessions(t.Context(), listTestRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: org, RunnerProfileId: " "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("whitespace-only profile: got %v, want InvalidArgument", err)
	}
}

const streamTestExecID = "018f0000-0000-7000-8000-00000000000a"

func TestCloseDuringExecStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sleep")
	}
	workspace := t.TempDir()
	for round := range 300 {
		testCloseDuringExecStart(t, workspace, round)
	}
}

func testCloseDuringExecStart(t *testing.T, workspace string, round int) {
	t.Helper()
	const execs = 8
	s := New()
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var calls sync.WaitGroup
	defer func() {
		cancel()
		calls.Wait()
	}()
	ready := make(chan struct{}, execs)
	start := make(chan struct{})
	s.BeforeExec = func(*sandrunv1.ExecDevSessionRequest) {
		ready <- struct{}{}
		select {
		case <-start:
		case <-ctx.Done():
		}
	}
	client, ds := createExecTestSession(t, ctx, s, workspace)
	for range execs {
		calls.Go(func() {
			stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
				OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(),
				ExecId: s.newID(), Command: []string{"sleep", "30"},
			}))
			if err == nil {
				defer stream.Close()
				for stream.Receive() {
				}
			}
		})
	}
	for range execs {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatalf("round %d: execs did not reach the start gate: %v", round, ctx.Err())
		}
	}
	close(start)
	time.Sleep(time.Duration(rand.IntN(500)) * time.Microsecond)
	s.Close()

	s.guest.mu.Lock()
	defer s.guest.mu.Unlock()
	for id, p := range s.guest.execs {
		p.mu.Lock()
		finished := p.final != nil
		p.mu.Unlock()
		if !finished {
			t.Errorf("round %d: Close returned before exec %s finished", round, id)
		}
	}
}

func TestCloseReapsRunningExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, ds := createExecTestSession(t, ctx, s, t.TempDir())
	stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
		OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(),
		ExecId: streamTestExecID, Command: []string{"sh", "-c", "echo $$; exec sleep 30"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var stdout strings.Builder
	for stream.Receive() {
		stdout.Write(stream.Msg().GetEvent().GetStdout().GetData())
		if strings.HasSuffix(stdout.String(), "\n") {
			break
		}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil || pid <= 0 {
		t.Fatalf("expected a process ID, got %q (stream error: %v)", stdout.String(), stream.Err())
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("exec was not running before Close: %v", err)
	}

	s.Close()
	if err := process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("signal after Close returned %v, want os.ErrProcessDone", err)
	}
}

func TestExecRejectedAfterClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	s := New()
	workspace := t.TempDir()
	s.Close()
	ds := &sandrunv1.DevSession{WorkspacePath: workspace}
	p, err := s.guest.start(ds, &sandrunv1.ExecDevSessionRequest{
		ExecId: streamTestExecID, Command: []string{"sh", "-c", "touch started"},
	})
	if p != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || len(s.ExecRequests()) != 0 {
		t.Fatalf("start after Close returned (%v, %v), with %d admitted execs", p, err, len(s.ExecRequests()))
	}
	if _, err := os.Stat(filepath.Join(workspace, "started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat process marker after Close returned %v, want os.ErrNotExist", err)
	}
}

func TestCloseAfterExecStartFailure(t *testing.T) {
	workspace := t.TempDir()
	s := New()
	t.Cleanup(s.srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, ds := createExecTestSession(t, ctx, s, workspace)
	for _, tc := range []struct {
		dir    string
		reason sandrunv1.ExecFailureReason
	}{
		{dir: "missing", reason: sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_WORKING_DIRECTORY_INVALID},
		{reason: sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND},
	} {
		stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
			OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: s.newID(),
			Command: []string{filepath.Join(workspace, "missing-program")}, WorkingDirectory: tc.dir,
		}))
		events := receiveExecEvents(t, stream, err)
		if len(events) != 2 || events[1].GetFailed().GetReason() != tc.reason {
			t.Fatalf("exec in %q returned %v, want Started and Failed (%s)", tc.dir, events, tc.reason)
		}
	}
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("Close did not return after exec start failures")
	}
}

func TestExecRequestPath(t *testing.T) {
	const tool = "sandruntest-path-tool"
	requestDir, inheritedDir := t.TempDir(), t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	install := func(dir, name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, image, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pathKey, suffix := "PATH", ""
	if runtime.GOOS == "windows" {
		pathKey, suffix = "Path", ".exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	requestTool := install(requestDir, tool+suffix)
	requestOnlyTool := install(requestDir, tool+"-request-only"+suffix)
	inheritedTool := install(inheritedDir, tool+suffix)
	t.Setenv("PATH", inheritedDir)

	s := New()
	t.Cleanup(s.Close)
	client, ds := createExecTestSession(t, t.Context(), s, t.TempDir())
	run := func(t *testing.T, name string, env map[string]string, wantPath string) {
		t.Helper()
		stream, err := client.ExecDevSession(t.Context(), listTestRequest(&sandrunv1.ExecDevSessionRequest{
			OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: s.newID(),
			Command: []string{name, "-test.run=^TestExecRequestPathProcess$"},
			Env:     env,
		}))
		events := receiveExecEvents(t, stream, err)
		if len(events) == 0 {
			t.Fatal("exec returned no events")
		}
		final := events[len(events)-1]
		if wantPath == "" {
			want := sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND
			if final.GetFailed().GetReason() != want {
				t.Fatalf("got %v, want %v", final, want)
			}
			return
		}
		if exited := final.GetExited(); exited == nil || exited.GetExitCode() != 0 {
			t.Fatalf("got %v, want exit code 0", final)
		}
		var stdout strings.Builder
		for _, event := range events {
			stdout.Write(event.GetStdout().GetData())
		}
		gotPath, argv0, _ := strings.Cut(stdout.String(), "\n")
		if argv0 != name {
			t.Errorf("got argv[0] %q, want %q", argv0, name)
		}
		gotFile, err := os.Stat(gotPath)
		if err != nil {
			t.Fatal(err)
		}
		wantFile, err := os.Stat(wantPath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(gotFile, wantFile) {
			t.Errorf("ran %q, want %q", gotPath, wantPath)
		}
	}

	t.Run("request-only executable", func(t *testing.T) {
		run(t, tool+"-request-only", map[string]string{pathKey: requestDir}, requestOnlyTool)
	})
	t.Run("request takes precedence", func(t *testing.T) {
		run(t, tool, map[string]string{pathKey: requestDir}, requestTool)
	})
	t.Run("PATH omitted", func(t *testing.T) {
		run(t, tool, nil, inheritedTool)
	})
	t.Run("PATH empty", func(t *testing.T) {
		run(t, tool, map[string]string{pathKey: ""}, "")
	})
	t.Run("explicit path", func(t *testing.T) {
		run(t, inheritedTool, map[string]string{pathKey: requestDir}, inheritedTool)
	})
	if runtime.GOOS == "windows" {
		t.Run("request PATHEXT order", func(t *testing.T) {
			path := install(requestDir, tool+".com")
			run(t, tool, map[string]string{pathKey: requestDir, "PathExt": ".COM;.EXE"}, path)
		})
	}
}

func TestExecRequestPathProcess(t *testing.T) {
	if !strings.HasPrefix(filepath.Base(os.Args[0]), "sandruntest-path-tool") {
		return
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%s\n%s", path, os.Args[0])
	os.Exit(0)
}

func TestExecStreamEventLimit(t *testing.T) {
	const outputSize = 2*chunkLimit + 1
	for _, limit := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			exec, attach := newExecStreams(t, limit, 0)
			events := exec(fmt.Sprintf("printf '%%0%dd' 0; exit 3", outputSize))
			requireStarted(t, events, limit)
			if limit == 1 {
				requireStarted(t, attach(0), limit)
				return
			}
			if len(events) < 2 || events[1].GetStdout() == nil {
				t.Fatal("the first event after Started must be stdout")
			}

			var output strings.Builder
			for {
				for i, ev := range events[1:] {
					switch {
					case ev.GetStdout() != nil:
						chunk := ev.GetStdout()
						if chunk.GetOffset() != int64(output.Len()) || len(chunk.GetData()) == 0 {
							t.Fatalf("stdout chunk at offset %d has %d bytes, want nonempty data at %d",
								chunk.GetOffset(), len(chunk.GetData()), output.Len())
						}
						output.Write(chunk.GetData())
					case ev.GetExited() != nil:
						if i != len(events)-2 || ev.GetExited().GetExitCode() != 3 {
							t.Fatalf("expected exit code 3 as the last event, got %v", ev)
						}
						if output.String() != strings.Repeat("0", outputSize) {
							t.Fatalf("incorrect stdout, got %d bytes and want %d zeros", output.Len(), outputSize)
						}
						return
					default:
						t.Fatalf("unexpected event %v", ev)
					}
				}
				if limit == 0 {
					t.Fatal("the unlimited stream ended before Exited")
				}
				events = attach(int64(output.Len()))
				requireStarted(t, events, limit)
			}
		})
	}
}

func TestExecStreamEventLimitCountsTruncation(t *testing.T) {
	const limit = 2
	exec, attach := newExecStreams(t, limit, 4)
	requireStarted(t, exec("printf 0123456789"), limit)

	// Wait for Exited at the end of stdout so the retained range is stable.
	events := attach(10)
	requireStarted(t, events, limit)
	if events[1].GetExited() == nil {
		t.Fatalf("expected Exited, got %v", events[1])
	}

	events = attach(0)
	requireStarted(t, events, limit)
	truncated := events[1].GetOutputTruncated()
	if truncated == nil || truncated.GetStdoutOffset() != 6 || truncated.GetStderrOffset() != 0 {
		t.Fatalf("expected OutputTruncated with offsets (6, 0), got %v", events[1])
	}

	events = attach(truncated.GetStdoutOffset())
	requireStarted(t, events, limit)
	chunk := events[1].GetStdout()
	if chunk.GetOffset() != 6 || string(chunk.GetData()) != "6789" {
		t.Fatalf("expected retained stdout at offset 6, got %v", events[1])
	}
	events = attach(chunk.GetOffset() + int64(len(chunk.GetData())))
	requireStarted(t, events, limit)
	if exited := events[1].GetExited(); exited == nil || exited.GetExitCode() != 0 {
		t.Fatalf("expected exit code 0, got %v", events[1])
	}
}

func requireStarted(t *testing.T, events []*sandrunv1.DevSessionExecEvent, limit int) {
	t.Helper()
	if limit > 0 && len(events) != limit {
		t.Fatalf("received %d events, want %d", len(events), limit)
	}
	if len(events) == 0 || events[0].GetStarted().GetExecId() != streamTestExecID {
		t.Fatal("expected Started with the exec ID as the first event")
	}
}

func newExecStreams(t *testing.T, limit, keep int) (
	func(string) []*sandrunv1.DevSessionExecEvent,
	func(int64) []*sandrunv1.DevSessionExecEvent,
) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
	workspace := t.TempDir()
	s := New()
	t.Cleanup(s.Close)
	s.ExecStreamEventLimit = limit
	s.OutputKeepBytes = keep
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, ds := createExecTestSession(t, ctx, s, workspace)
	return func(command string) []*sandrunv1.DevSessionExecEvent {
			t.Helper()
			stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
				OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID,
				Command: []string{"sh", "-c", command},
			}))
			return receiveExecEvents(t, stream, err)
		}, func(offset int64) []*sandrunv1.DevSessionExecEvent {
			t.Helper()
			stream, err := client.AttachDevSessionExec(ctx, listTestRequest(&sandrunv1.AttachDevSessionExecRequest{
				OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID, StdoutOffset: offset,
			}))
			return receiveExecEvents(t, stream, err)
		}
}

func createExecTestSession(t *testing.T, ctx context.Context, s *Server, workspace string) (sandrunv1connect.DevSessionServiceClient, *sandrunv1.DevSession) {
	t.Helper()
	org := s.AddOrganization("exec test").GetOrganizationId()
	profile := s.AddDevSessionProfile(org, "exec", sandrunv1.GuestOs_GUEST_OS_LINUX, workspace)
	client := sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	session, err := client.CreateDevSession(ctx, listTestRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client, session.Msg.GetDevSession()
}

func receiveExecEvents[T any, P interface {
	*T
	GetEvent() *sandrunv1.DevSessionExecEvent
}](t *testing.T, stream *connect.ServerStreamForClient[T], err error) []*sandrunv1.DevSessionExecEvent {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var events []*sandrunv1.DevSessionExecEvent
	for stream.Receive() {
		events = append(events, P(stream.Msg()).GetEvent())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	return events
}
