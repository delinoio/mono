// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

// TestDevSessionQueueAndDeletion: a session waits in the queue while the
// device pool is full and starts when capacity is free; the owner (with a
// personal API key) and an admin delete sessions, a member cannot delete
// another member's session.
func TestDevSessionQueueAndDeletion(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 1 })
	var mu sync.Mutex
	var commands [][]string
	w.fake.SetExec(func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		mu.Lock()
		commands = append(commands, p.Command)
		mu.Unlock()
		code := int64(0)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, nil)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	_, memberKey := w.env.NewAPIKey(w.org.ID, w.org.Member)
	memberByKey := w.env.Bearer(memberKey)

	s1 := w.createSession(t, memberByKey, profileID)
	if s1.GetState() != tidev1.RequestState_REQUEST_STATE_QUEUED || s1.GetOwnerUserId() != w.org.Member || s1.GetWorkspacePath() != "/home/dev/work" ||
		s1.GetGuestOs() != tidev1.GuestOs_GUEST_OS_LINUX || s1.GetRunnerProfileName() != "linux-dev" {
		t.Fatalf("created %v", s1)
	}
	s1 = w.waitState(t, s1.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	r1 := w.runner(t, s1.GetRunnerId())
	if r1.GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY || r1.GetOwnsandSandboxId() == "" || s1.GetStartedAt() == nil || s1.GetOwnsandDeviceId() == "" {
		t.Fatalf("runner %v session %v", r1, s1)
	}
	sb := w.fake.Sandboxes(w.osOrg)[0]
	if sb.GetLabels()["tide.runner-id"] != r1.GetRunnerId() || sb.GetLabels()["tide.organization-id"] != w.org.ID || sb.GetLabels()["tide.runner-profile-id"] != profileID ||
		sb.GetResources().GetVcpuCount() != 2 || sb.GetIdleTtl().AsDuration() != time.Hour {
		t.Fatalf("sandbox %v", sb)
	}
	mu.Lock()
	if len(commands) != 1 || strings.Join(commands[0], " ") != "mkdir -p -- /home/dev/work" {
		t.Fatalf("prepare commands %v", commands)
	}
	mu.Unlock()

	// The pool is full: the admin's session waits.
	s2 := w.createSession(t, w.env.User(w.org.Admin), profileID)
	stays(t, "queued session", func() bool {
		return w.session(t, s2.GetDevSessionId()).GetState() == tidev1.RequestState_REQUEST_STATE_QUEUED
	})
	if n := len(w.fake.Sandboxes(w.osOrg)); n != 1 {
		t.Fatalf("%d sandboxes", n)
	}

	// A member cannot delete the admin's session; the owner deletes theirs
	// with the personal API key.
	_, err := memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s2.GetDevSessionId()}))
	wantCode(t, "member deletes another session", err, connect.CodePermissionDenied)
	del, err := memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s1.GetDevSessionId()}))
	if err != nil || del.Msg.GetDevSession().GetState() != tidev1.RequestState_REQUEST_STATE_ENDED ||
		del.Msg.GetDevSession().GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("delete: %v %v", del, err)
	}
	if r := w.runner(t, r1.GetRunnerId()); r.GetState() != tidev1.RunnerState_RUNNER_STATE_DELETED || r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("deleted runner %v", r)
	}
	if w.fake.Sandboxes(w.osOrg)[0].GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatal("the sandbox was not deleted")
	}
	// Capacity is free: the queued session starts.
	s2 = w.waitState(t, s2.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	// An owner deletes the admin's session.
	del, err = w.env.User(w.org.Owner).DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s2.GetDevSessionId()}))
	if err != nil || del.Msg.GetDevSession().GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("owner deletes: %v %v", del, err)
	}
	// A queued session is canceled.
	s3 := w.createSession(t, memberByKey, profileID)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.MaxSandboxes = 0
		o.CreateError = connect.NewError(connect.CodeUnavailable, errors.New("full"))
	})
	del, err = memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s3.GetDevSessionId()}))
	if err != nil {
		t.Fatal(err)
	}
	if st := del.Msg.GetDevSession(); st.GetState() == tidev1.RequestState_REQUEST_STATE_CANCELED {
		if st.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER {
			t.Fatalf("canceled %v", st)
		}
	} else if st.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("deleted third session %v", st)
	}
	// Audit: creations and deletions name the owner and the actor key.
	var created, deleted, byKey int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action = $2), count(*) FILTER (WHERE action = $3), count(*) FILTER (WHERE actor_api_key_id IS NOT NULL) FROM audit_events WHERE organization_id = $1",
		w.org.ID, int16(tidev1.AuditAction_AUDIT_ACTION_DEV_SESSION_CREATED), int16(tidev1.AuditAction_AUDIT_ACTION_DEV_SESSION_DELETED)).Scan(&created, &deleted, &byKey)
	if created != 3 || deleted != 3 || byKey < 4 {
		t.Fatalf("audit: created %d deleted %d by key %d", created, deleted, byKey)
	}
	// Listing: by owner and with ended sessions.
	list, err := w.env.User(w.org.Member).DevSession.ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: w.org.ID,
		Owner: &tidev1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true}, IncludeEnded: true}))
	if err != nil || len(list.Msg.GetDevSessions()) != 2 {
		t.Fatalf("own sessions %v %v", list, err)
	}
	open, _ := w.env.User(w.org.Member).DevSession.ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: w.org.ID}))
	if len(open.Msg.GetDevSessions()) != 0 {
		t.Fatalf("open sessions %v", open)
	}
	profiles, err := memberByKey.DevSession.ListDevSessionProfiles(ctx, connect.NewRequest(&tidev1.ListDevSessionProfilesRequest{OrganizationId: w.org.ID}))
	if err != nil || len(profiles.Msg.GetProfiles()) != 1 || profiles.Msg.GetProfiles()[0].GetWorkspacePath() != "/home/dev/work" {
		t.Fatalf("profiles %v %v", profiles, err)
	}
}

func TestDevSessionPerUserLimit(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	if _, err := w.env.User(w.org.Admin).Organization.UpdateOrganizationSettings(ctx, connect.NewRequest(&tidev1.UpdateOrganizationSettingsRequest{
		OrganizationId: w.org.ID, Settings: &tidev1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(1)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}}})); err != nil {
		t.Fatal(err)
	}
	member := w.env.User(w.org.Member)
	s1 := w.createSession(t, member, profileID)
	_, err := member.DevSession.CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID}))
	wantCode(t, "second session", err, connect.CodeResourceExhausted)
	// Other users have their own count.
	w.createSession(t, w.env.User(w.org.Admin), profileID)
	if _, err := member.DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s1.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	w.createSession(t, member, profileID)
	for name, req := range map[string]*tidev1.CreateDevSessionRequest{
		"long label":   {OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: strings.Repeat("l", 101)},
		"bad profile":  {OrganizationId: w.org.ID, RunnerProfileId: "x"},
		"control char": {OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: "a\x01"},
	} {
		_, err := w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(req))
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	_, err = w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: api.NewID()}))
	wantCode(t, "unknown profile", err, connect.CodeNotFound)
	gh := devProfileProto("linux-ci", 0, 0)
	gh.WorkloadKind, gh.WorkspacePath = tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, ""
	ghID := w.createProfile(t, gh)
	_, err = w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: ghID}))
	wantCode(t, "GITHUB_JOB profile", err, connect.CodeInvalidArgument)
}

// TestDevSessionEnds covers the end reasons of the runner lifecycle.
func TestDevSessionEnds(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	member := w.env.User(w.org.Member)
	sandboxOf := func(s *tidev1.DevSession) string { return w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId() }

	// An OwnSand TTL ends the session normally.
	s := w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.EndSandbox(w.osOrg, sandboxOf(s), ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_ENDED)
	r := w.runner(t, s.GetRunnerId())
	if s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_TTL_EXPIRED || r.GetState() != tidev1.RunnerState_RUNNER_STATE_DELETED ||
		r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED || r.GetOwnsandEndReason() != "IDLE_TTL_EXPIRED" {
		t.Fatalf("ttl end %v %v", s, r)
	}

	// Another OwnSand end loses the runner.
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.EndSandbox(w.osOrg, sandboxOf(s), ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST || w.runner(t, s.GetRunnerId()).GetState() != tidev1.RunnerState_RUNNER_STATE_FAILED {
		t.Fatalf("lost %v", s)
	}

	// No device can run the profile.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
	})
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE {
		t.Fatalf("no device %v", s)
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })

	// Start failures: the workspace cannot be created. Each failure deletes
	// the sandbox and returns the session to the queue; the third one fails
	// it.
	w.fake.SetExec(func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		code := int64(1)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, nil)
	before := len(w.fake.Sandboxes(w.osOrg))
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	req, err := w.env.User(w.org.Owner).Work.GetRequest(context.Background(), connect.NewRequest(&tidev1.GetRequestRequest{OrganizationId: w.org.ID, RequestId: s.GetDevSessionId()}))
	if err != nil || s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED || req.Msg.GetRequest().GetStartAttempts() != 3 ||
		!req.Msg.GetRequest().GetQueuedAt().AsTime().Equal(s.GetQueuedAt().AsTime()) {
		t.Fatalf("start failures %v %v %v", s, req, err)
	}
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes)-before != 3 {
		t.Fatalf("%d sandboxes for 3 attempts", len(sandboxes)-before)
	}
	for _, sb := range sandboxes[before:] {
		if sb.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
			t.Fatal("a failed start left its sandbox")
		}
	}
	w.fake.SetExec(nil, nil)

	// The maximum queue wait ends a session that never got capacity.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewError(connect.CodeUnavailable, errors.New("full"))
	})
	s = w.createSession(t, member, profileID)
	stays(t, "queued", func() bool {
		return w.session(t, s.GetDevSessionId()).GetState() == tidev1.RequestState_REQUEST_STATE_QUEUED
	})
	w.env.Clock.Advance(time.Hour)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_EXPIRED)
	if s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT {
		t.Fatalf("expired %v", s)
	}
}

func (w *workWorld) runningSession(t *testing.T) (*tidev1.DevSession, string) {
	t.Helper()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	return s, w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId()
}

func collectExec(t *testing.T, stream interface {
	Receive() bool
	Err() error
}, msg func() *tidev1.DevSessionExecEvent) ([]*tidev1.DevSessionExecEvent, error) {
	var out []*tidev1.DevSessionExecEvent
	for stream.Receive() {
		out = append(out, msg())
	}
	return out, stream.Err()
}

func TestDevSessionExec(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{StreamLimit: 600 * time.Millisecond, KeepaliveInterval: 150 * time.Millisecond})
	ctx := context.Background()
	s, _ := w.runningSession(t)
	var mu sync.Mutex
	var got ownsand.ExecParams
	w.fake.SetExec(func(ctx context.Context, id string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		mu.Lock()
		got = p
		mu.Unlock()
		if p.Command[0] == "mkdir" {
			code := int64(0)
			return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
		}
		if p.Command[0] == "sleep" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_ = p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: p.ExecID})
		_ = p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStdout, ExecID: p.ExecID, Offset: 0, Data: []byte("hello")})
		code := int64(3)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, func(ctx context.Context, id, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		_ = p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStderr, ExecID: execID, Offset: p.StderrOffset, Data: []byte("late")})
		sig := int32(9)
		return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, Signal: &sig}, nil
	})
	owner := w.env.User(w.org.Member)
	execID := api.NewID()
	stream, err := owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&tidev1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: strings.ToUpper(execID), Command: []string{"make", "test"}, Env: map[string]string{"TOKEN": "SECRET-ENV-VALUE"}, WorkingDirectory: "src"}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectExec(t, stream, func() *tidev1.DevSessionExecEvent { return stream.Msg().GetEvent() })
	if err != nil || len(events) != 3 || events[0].GetStarted() == nil || string(events[1].GetStdout().GetData()) != "hello" || events[2].GetExited().GetExitCode() != 3 {
		t.Fatalf("events %v %v", events, err)
	}
	mu.Lock()
	if got.ExecID != execID || got.WorkingDirectory != "/home/dev/work/src" || got.Env["TOKEN"] != "SECRET-ENV-VALUE" {
		t.Fatalf("params %+v", got)
	}
	mu.Unlock()
	if strings.Contains(w.env.Logs.String(), "SECRET-ENV-VALUE") || strings.Contains(w.env.Logs.String(), `"make"`) {
		t.Fatal("the exec environment or command reached the logs")
	}
	if !strings.Contains(w.env.Logs.String(), `"dev_session_exec_ended"`) {
		t.Fatal("no exec end log")
	}

	// A long command: the stream ends with OK and no final event before the
	// limit, with keepalives meanwhile.
	start := time.Now()
	stream, err = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&tidev1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: api.NewID(), Command: []string{"sleep", "100"}}))
	if err != nil {
		t.Fatal(err)
	}
	events, err = collectExec(t, stream, func() *tidev1.DevSessionExecEvent { return stream.Msg().GetEvent() })
	if err != nil || time.Since(start) > 5*time.Second || len(events) < 2 {
		t.Fatalf("rotated stream %v %v", events, err)
	}
	for _, ev := range events {
		if ev.GetKeepalive() == nil {
			t.Fatalf("non-keepalive event %v", ev)
		}
	}

	// Attach continues from offsets.
	astream, err := owner.DevSession.AttachDevSessionExec(ctx, connect.NewRequest(&tidev1.AttachDevSessionExecRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: execID, StdoutOffset: 5, StderrOffset: 2}))
	if err != nil {
		t.Fatal(err)
	}
	events, err = collectExec(t, astream, func() *tidev1.DevSessionExecEvent { return astream.Msg().GetEvent() })
	if err != nil || len(events) != 2 || events[0].GetStderr().GetOffset() != 2 || events[1].GetExited().GetSignal() != 9 {
		t.Fatalf("attach %v %v", events, err)
	}

	// Only the owner runs commands, also an organization owner cannot.
	stream, _ = w.env.User(w.org.Owner).DevSession.ExecDevSession(ctx, connect.NewRequest(&tidev1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: api.NewID(), Command: []string{"id"}}))
	_, err = collectExec(t, stream, func() *tidev1.DevSessionExecEvent { return nil })
	wantCode(t, "organization owner execs", err, connect.CodePermissionDenied)
	for name, req := range map[string]*tidev1.ExecDevSessionRequest{
		"bad exec id":   {ExecId: "x", Command: []string{"id"}},
		"empty command": {ExecId: api.NewID()},
	} {
		req.OrganizationId, req.DevSessionId = w.org.ID, s.GetDevSessionId()
		stream, _ = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(req))
		_, err = collectExec(t, stream, func() *tidev1.DevSessionExecEvent { return nil })
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	// A session that is not RUNNING accepts no command.
	if _, err := owner.DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	stream, _ = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&tidev1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: api.NewID(), Command: []string{"id"}}))
	_, err = collectExec(t, stream, func() *tidev1.DevSessionExecEvent { return nil })
	wantCode(t, "ended session", err, connect.CodeFailedPrecondition)
}

func TestDevSessionFiles(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	s, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	writeID := api.NewID()
	data := []byte(strings.Repeat("0123456789", 300))
	write := func(offset int, chunk []byte, final bool) (*tidev1.WriteDevSessionFileResponse, error) {
		resp, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&tidev1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
			WriteId: writeID, Path: "out/a.txt", Offset: int64(offset), Data: chunk, Final: final, Mode: 0o755}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	if r, err := write(1000, data[1000:2000], false); err != nil || r.GetOffset() != 1000 || r.GetLength() != 1000 || r.GetFinished() {
		t.Fatalf("middle chunk %v %v", r, err)
	}
	if _, ok := w.fake.File(w.osOrg, sandboxID, "/home/dev/work/out/a.txt"); ok {
		t.Fatal("the file appeared before the final chunk")
	}
	if _, err := write(0, data[:1000], false); err != nil {
		t.Fatal(err)
	}
	r, err := write(2000, data[2000:], true)
	if err != nil || !r.GetFinished() || r.GetSizeBytes() != int64(len(data)) {
		t.Fatalf("final %v %v", r, err)
	}
	if got, _ := w.fake.File(w.osOrg, sandboxID, "/home/dev/work/out/a.txt"); string(got) != string(data) {
		t.Fatalf("file %d bytes", len(got))
	}
	_, err = owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&tidev1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		WriteId: api.NewID(), Path: "/x", Data: make([]byte, 1<<20+1)}))
	wantCode(t, "chunk above 1 MiB", err, connect.CodeInvalidArgument)
	_, err = w.env.User(w.org.Admin).DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&tidev1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		WriteId: api.NewID(), Path: "/x", Data: []byte("x"), Final: true}))
	wantCode(t, "admin writes", err, connect.CodePermissionDenied)

	// Read from an offset with the same read ID: the second call reads the
	// same snapshot.
	readID := api.NewID()
	read := func(offset int64) ([]byte, int64, error) {
		stream, err := owner.DevSession.ReadDevSessionFile(ctx, connect.NewRequest(&tidev1.ReadDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
			ReadId: readID, Path: "/home/dev/work/out/a.txt", Offset: offset}))
		if err != nil {
			return nil, 0, err
		}
		var buf []byte
		size := int64(-1)
		next := offset
		for stream.Receive() {
			m := stream.Msg()
			if c := m.GetChunk(); c != nil {
				if c.GetOffset() != next || len(c.GetData()) > 1<<20 {
					return nil, 0, errors.New("chunk offset")
				}
				buf = append(buf, c.GetData()...)
				next += int64(len(c.GetData()))
			}
			if e := m.GetEnd(); e != nil {
				size = e.GetSizeBytes()
			}
		}
		return buf, size, stream.Err()
	}
	got, size, err := read(0)
	if err != nil || string(got) != string(data) || size != int64(len(data)) {
		t.Fatalf("read %d bytes size %d %v", len(got), size, err)
	}
	w.fake.SetFile(w.osOrg, sandboxID, "/home/dev/work/out/a.txt", []byte("changed"))
	got, size, err = read(2500)
	if err != nil || string(got) != string(data[2500:]) || size != int64(len(data)) {
		t.Fatalf("read from offset %q size %d %v", got, size, err)
	}
	if !strings.Contains(w.env.Logs.String(), `"dev_session_file_read"`) || !strings.Contains(w.env.Logs.String(), `"dev_session_file_write"`) ||
		strings.Contains(w.env.Logs.String(), "0123456789") {
		t.Fatal("file transfer logs")
	}
}

func newWorkWorld(t *testing.T, ds devsession.Timing) *workWorld {
	t.Helper()
	w := &world{fake: ownsandfake.New(), l: &layers.Layers{}}
	w.env = testenv.New(t, testenv.WithLayerOptions(layers.Options{OwnSand: w.fake, DevSessionTiming: ds}, w.l))
	w.org = w.env.NewOrg("alice")
	ww := &workWorld{world: w, osOrg: api.NewID()}
	w.fake.AddOrg(ww.osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, ww.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	return ww
}

func (w *workWorld) createSession(t *testing.T, c *testenv.Clients, profileID string) *tidev1.DevSession {
	t.Helper()
	resp, err := c.DevSession.CreateDevSession(context.Background(), connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: "build"}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetDevSession()
}

func (w *workWorld) session(t *testing.T, id string) *tidev1.DevSession {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).DevSession.GetDevSession(context.Background(), connect.NewRequest(&tidev1.GetDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetDevSession()
}

func (w *workWorld) waitState(t *testing.T, id string, state tidev1.RequestState) *tidev1.DevSession {
	t.Helper()
	var last *tidev1.DevSession
	eventually(t, "dev session "+state.String(), func() bool {
		last = w.session(t, id)
		return last.GetState() == state
	})
	return last
}
