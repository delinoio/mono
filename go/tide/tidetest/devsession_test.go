// SPDX-License-Identifier: Apache-2.0

package tidetest

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

func TestListDevSessionsFilters(t *testing.T) {
	const (
		running    = tidev1.RequestState_REQUEST_STATE_RUNNING
		ended      = tidev1.RequestState_REQUEST_STATE_ENDED
		otherOwner = "user-2"
	)
	s := New()
	t.Cleanup(s.Close)
	org := s.AddOrganization("first").GetOrganizationId()
	other := s.AddOrganization("second").GetOrganizationId()
	s.mu.Lock()
	s.sessions = []*tidev1.DevSession{
		{DevSessionId: "other-org", OrganizationId: other, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "second-ended", OrganizationId: org, RunnerProfileId: secondListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "other-owner", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: otherOwner, State: ended},
		{DevSessionId: "first-running", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: running},
		{DevSessionId: "first-ended", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID, State: ended},
		{DevSessionId: "unspecified", OrganizationId: org, RunnerProfileId: firstListProfile, OwnerUserId: s.UserID},
	}
	s.mu.Unlock()
	client := tidev1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	for _, tt := range []struct {
		name string
		req  *tidev1.ListDevSessionsRequest
		want []string
	}{
		{"default open states", &tidev1.ListDevSessionsRequest{}, []string{"first-running"}},
		{"profile", &tidev1.ListDevSessionsRequest{RunnerProfileId: strings.ToUpper(secondListProfile), IncludeEnded: true}, []string{"second-ended"}},
		{"ended", &tidev1.ListDevSessionsRequest{States: []tidev1.RequestState{ended}}, []string{"second-ended", "other-owner", "first-ended"}},
		{"ended with include_ended", &tidev1.ListDevSessionsRequest{States: []tidev1.RequestState{ended}, IncludeEnded: true}, []string{"second-ended", "other-owner", "first-ended"}},
		{
			"caller ownership intersection before pagination",
			&tidev1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				States:          []tidev1.RequestState{ended},
				Owner:           &tidev1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true},
				PageSize:        1,
			},
			[]string{"first-ended"},
		},
		{
			"explicit owner intersection",
			&tidev1.ListDevSessionsRequest{
				RunnerProfileId: firstListProfile,
				Owner:           &tidev1.ListDevSessionsRequest_OwnerUserId{OwnerUserId: otherOwner},
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
	_, err := client.ListDevSessions(t.Context(), listTestRequest(&tidev1.ListDevSessionsRequest{OrganizationId: org, RunnerProfileId: " "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("whitespace-only profile: got %v, want InvalidArgument", err)
	}
}

const streamTestExecID = "018f0000-0000-7000-8000-00000000000a"

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

func requireStarted(t *testing.T, events []*tidev1.DevSessionExecEvent, limit int) {
	t.Helper()
	if limit > 0 && len(events) != limit {
		t.Fatalf("received %d events, want %d", len(events), limit)
	}
	if len(events) == 0 || events[0].GetStarted().GetExecId() != streamTestExecID {
		t.Fatal("expected Started with the exec ID as the first event")
	}
}

func newExecStreams(t *testing.T, limit, keep int) (
	func(string) []*tidev1.DevSessionExecEvent,
	func(int64) []*tidev1.DevSessionExecEvent,
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
	org := s.AddOrganization("stream test").GetOrganizationId()
	profile := s.AddDevSessionProfile(org, "stream", tidev1.GuestOs_GUEST_OS_LINUX, workspace)
	client := tidev1connect.NewDevSessionServiceClient(http.DefaultClient, s.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, err := client.CreateDevSession(ctx, listTestRequest(&tidev1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	sessionID := session.Msg.GetDevSession().GetDevSessionId()
	return func(command string) []*tidev1.DevSessionExecEvent {
			t.Helper()
			stream, err := client.ExecDevSession(ctx, listTestRequest(&tidev1.ExecDevSessionRequest{
				OrganizationId: org, DevSessionId: sessionID, ExecId: streamTestExecID,
				Command: []string{"sh", "-c", command},
			}))
			return receiveExecEvents(t, stream, err)
		}, func(offset int64) []*tidev1.DevSessionExecEvent {
			t.Helper()
			stream, err := client.AttachDevSessionExec(ctx, listTestRequest(&tidev1.AttachDevSessionExecRequest{
				OrganizationId: org, DevSessionId: sessionID, ExecId: streamTestExecID, StdoutOffset: offset,
			}))
			return receiveExecEvents(t, stream, err)
		}
}

func receiveExecEvents[T any, P interface {
	*T
	GetEvent() *tidev1.DevSessionExecEvent
}](t *testing.T, stream *connect.ServerStreamForClient[T], err error) []*tidev1.DevSessionExecEvent {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var events []*tidev1.DevSessionExecEvent
	for stream.Receive() {
		events = append(events, P(stream.Msg()).GetEvent())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	return events
}
