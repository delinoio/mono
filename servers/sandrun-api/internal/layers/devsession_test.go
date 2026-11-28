// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

// TestDevSessionQueueAndDeletion: a session waits in the queue while the
// device pool is full and starts when capacity is free; the owner (with a
// personal API key) and an admin delete sessions, a member cannot delete
// another member's session.
func TestDevSessionQueueAndDeletion(t *testing.T) {
	t.Parallel()
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
	memberKeyID, memberKey := w.env.NewAPIKey(w.org.ID, w.org.Member)
	memberByKey := w.env.Bearer(memberKey)

	s1 := w.createSession(t, memberByKey, profileID)
	createdAt := w.env.Clock.Now()
	if got := keyLastUsed(t, w.env, memberKeyID); got == nil || !got.Equal(createdAt) {
		t.Fatalf("last use after session creation = %v, want %v", got, createdAt)
	}
	if s1.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED || s1.GetOwnerUserId() != w.org.Member || s1.GetWorkspacePath() != "/home/dev/work" ||
		s1.GetGuestOs() != sandrunv1.GuestOs_GUEST_OS_LINUX || s1.GetRunnerProfileName() != "linux-dev" {
		t.Fatalf("created %v", s1)
	}
	s1 = w.waitState(t, s1.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	r1 := w.runner(t, s1.GetRunnerId())
	if r1.GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY || r1.GetOwnsandSandboxId() == "" || s1.GetStartedAt() == nil || s1.GetOwnsandDeviceId() == "" {
		t.Fatalf("runner %v session %v", r1, s1)
	}
	sb := w.fake.Sandboxes(w.osOrg)[0]
	if sb.GetLabels()["sandrun.runner-id"] != r1.GetRunnerId() || sb.GetLabels()["sandrun.organization-id"] != w.org.ID || sb.GetLabels()["sandrun.runner-profile-id"] != profileID ||
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
		return w.session(t, s2.GetDevSessionId()).GetState() == sandrunv1.RequestState_REQUEST_STATE_QUEUED
	})
	if n := len(w.fake.Sandboxes(w.osOrg)); n != 1 {
		t.Fatalf("%d sandboxes", n)
	}

	// A member cannot delete the admin's session; the owner deletes theirs
	// with the personal API key.
	w.env.Clock.Advance(time.Minute)
	_, err := memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s2.GetDevSessionId()}))
	wantCode(t, "member deletes another session", err, connect.CodePermissionDenied)
	if got := keyLastUsed(t, w.env, memberKeyID); got == nil || !got.Equal(createdAt) {
		t.Fatalf("last use after denied deletion = %v, want %v", got, createdAt)
	}
	del, err := memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s1.GetDevSessionId()}))
	if err != nil || del.Msg.GetDevSession().GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		del.Msg.GetDevSession().GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("delete: %v %v", del, err)
	}
	if got := keyLastUsed(t, w.env, memberKeyID); got == nil || !got.Equal(w.env.Clock.Now()) {
		t.Fatalf("last use after authorized deletion = %v, want %v", got, w.env.Clock.Now())
	}
	if r := w.runner(t, r1.GetRunnerId()); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED || r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("deleted runner %v", r)
	}
	if w.fake.Sandboxes(w.osOrg)[0].GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		t.Fatal("the sandbox was not deleted")
	}
	// Capacity is free: the queued session starts.
	s2 = w.waitState(t, s2.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	// An owner deletes the admin's session.
	del, err = w.env.User(w.org.Owner).DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s2.GetDevSessionId()}))
	if err != nil || del.Msg.GetDevSession().GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("owner deletes: %v %v", del, err)
	}
	// A queued session is canceled.
	s3 := w.createSession(t, memberByKey, profileID)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.MaxSandboxes = 0
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	del, err = memberByKey.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s3.GetDevSessionId()}))
	if err != nil {
		t.Fatal(err)
	}
	if st := del.Msg.GetDevSession(); st.GetState() == sandrunv1.RequestState_REQUEST_STATE_CANCELED {
		if st.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER {
			t.Fatalf("canceled %v", st)
		}
	} else if st.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("deleted third session %v", st)
	}
	// Audit: creations and deletions name the owner and the actor key.
	var created, deleted, byKey int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE action = $2), count(*) FILTER (WHERE action = $3), count(*) FILTER (WHERE actor_api_key_id IS NOT NULL) FROM audit_events WHERE organization_id = $1",
		w.org.ID, int16(sandrunv1.AuditAction_AUDIT_ACTION_DEV_SESSION_CREATED), int16(sandrunv1.AuditAction_AUDIT_ACTION_DEV_SESSION_DELETED)).Scan(&created, &deleted, &byKey)
	if created != 3 || deleted != 3 || byKey < 4 {
		t.Fatalf("audit: created %d deleted %d by key %d", created, deleted, byKey)
	}
	// Listing: by owner and with ended sessions.
	list, err := w.env.User(w.org.Member).DevSession.ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: w.org.ID,
		Owner: &sandrunv1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true}, IncludeEnded: true}))
	if err != nil || len(list.Msg.GetDevSessions()) != 2 {
		t.Fatalf("own sessions %v %v", list, err)
	}
	ended, err := memberByKey.DevSession.ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: w.org.ID,
		States: []sandrunv1.RequestState{sandrunv1.RequestState_REQUEST_STATE_ENDED}}))
	if err != nil || len(ended.Msg.GetDevSessions()) < 2 {
		t.Fatalf("ended sessions %v %v", ended, err)
	}
	unspecified, err := memberByKey.DevSession.ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: w.org.ID,
		States: []sandrunv1.RequestState{sandrunv1.RequestState_REQUEST_STATE_UNSPECIFIED}, IncludeEnded: true}))
	if err != nil || len(unspecified.Msg.GetDevSessions()) != 0 {
		t.Fatalf("sessions with unspecified state %v %v", unspecified, err)
	}
	open, _ := w.env.User(w.org.Member).DevSession.ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: w.org.ID}))
	if len(open.Msg.GetDevSessions()) != 0 {
		t.Fatalf("open sessions %v", open)
	}
	profiles, err := memberByKey.DevSession.ListDevSessionProfiles(ctx, connect.NewRequest(&sandrunv1.ListDevSessionProfilesRequest{OrganizationId: w.org.ID}))
	if err != nil || len(profiles.Msg.GetProfiles()) != 1 || profiles.Msg.GetProfiles()[0].GetWorkspacePath() != "/home/dev/work" {
		t.Fatalf("profiles %v %v", profiles, err)
	}
}

func keyLastUsed(t *testing.T, env *testenv.Env, keyID string) *time.Time {
	t.Helper()
	var used *time.Time
	if err := env.DB.Pool.QueryRow(t.Context(), "SELECT last_used_time FROM api_keys WHERE api_key_id = $1", keyID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func TestDevSessionAPIKeyUseWriteFailure(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	keyID, key := w.env.NewAPIKey(w.org.ID, w.org.Member)
	_, err := w.env.DB.Pool.Exec(t.Context(), `CREATE FUNCTION fail_last_use() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test last use write failure'; END; $$;
		CREATE TRIGGER fail_last_use BEFORE UPDATE OF last_used_time ON api_keys
		FOR EACH ROW EXECUTE FUNCTION fail_last_use();`)
	if err != nil {
		t.Fatal(err)
	}

	session := w.createSession(t, w.env.Bearer(key), profileID)
	if got := w.session(t, session.GetDevSessionId()).GetOwnerUserId(); got != w.org.Member {
		t.Fatalf("persisted session owner = %q, want %q", got, w.org.Member)
	}
	if got := keyLastUsed(t, w.env, keyID); got != nil {
		t.Fatalf("last use after a failed write = %v, want nil", got)
	}
	if !strings.Contains(w.env.Logs.String(), "api_key_last_use_write_failed") {
		t.Fatal("missing last use write failure log")
	}
}

func TestDevSessionPerUserLimit(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	if _, err := w.env.User(w.org.Admin).Organization.UpdateOrganizationSettings(ctx, connect.NewRequest(&sandrunv1.UpdateOrganizationSettingsRequest{
		OrganizationId: w.org.ID, Settings: &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: proto.Int32(1)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"max_dev_sessions_per_user"}}})); err != nil {
		t.Fatal(err)
	}
	member := w.env.User(w.org.Member)
	s1 := w.createSession(t, member, profileID)
	_, err := member.DevSession.CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID}))
	wantCode(t, "second session", err, connect.CodeResourceExhausted)
	// Other users have their own count.
	w.createSession(t, w.env.User(w.org.Admin), profileID)
	if _, err := member.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s1.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	w.createSession(t, member, profileID)
	for name, req := range map[string]*sandrunv1.CreateDevSessionRequest{
		"long label":   {OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: strings.Repeat("l", 101)},
		"bad profile":  {OrganizationId: w.org.ID, RunnerProfileId: "x"},
		"control char": {OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: "a\x01"},
	} {
		_, err := w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(req))
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	_, err = w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: api.NewID()}))
	wantCode(t, "unknown profile", err, connect.CodeNotFound)
	gh := devProfileProto("linux-ci", 0, 0)
	gh.WorkloadKind, gh.WorkspacePath = sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, ""
	ghID := w.createProfile(t, gh)
	_, err = w.env.User(w.org.Owner).DevSession.CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: ghID}))
	wantCode(t, "GITHUB_JOB profile", err, connect.CodeInvalidArgument)
}

// TestDevSessionEnds covers the end reasons of the runner lifecycle.
func TestDevSessionEnds(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	member := w.env.User(w.org.Member)
	sandboxOf := func(s *sandrunv1.DevSession) string { return w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId() }

	// An OwnSand TTL ends the session normally.
	s := w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.EndSandbox(w.osOrg, sandboxOf(s), ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_ENDED)
	r := w.runner(t, s.GetRunnerId())
	if s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_TTL_EXPIRED || r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED ||
		r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED || r.GetOwnsandEndReason() != "IDLE_TTL_EXPIRED" {
		t.Fatalf("ttl end %v %v", s, r)
	}

	// Another OwnSand end loses the runner.
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.EndSandbox(w.osOrg, sandboxOf(s), ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE, ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST || w.runner(t, s.GetRunnerId()).GetState() != sandrunv1.RunnerState_RUNNER_STATE_FAILED {
		t.Fatalf("lost %v", s)
	}

	// No device can run the profile.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
	})
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE {
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
	before := make(map[string]struct{})
	for _, sb := range w.fake.Sandboxes(w.osOrg) {
		before[sb.GetSandboxId()] = struct{}{}
	}
	s = w.createSession(t, member, profileID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_FAILED)
	req, err := w.env.User(w.org.Owner).Work.GetRequest(context.Background(), connect.NewRequest(&sandrunv1.GetRequestRequest{OrganizationId: w.org.ID, RequestId: s.GetDevSessionId()}))
	if err != nil || s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED || req.Msg.GetRequest().GetStartAttempts() != 3 ||
		!req.Msg.GetRequest().GetQueuedAt().AsTime().Equal(s.GetQueuedAt().AsTime()) {
		t.Fatalf("start failures %v %v %v", s, req, err)
	}
	pending := make(map[string]ownsandv1.SandboxState)
	for _, sb := range w.fake.Sandboxes(w.osOrg) {
		if _, exists := before[sb.GetSandboxId()]; !exists {
			pending[sb.GetSandboxId()] = sb.GetState()
		}
	}
	if len(pending) != 3 {
		t.Fatalf("%d new sandboxes for 3 attempts", len(pending))
	}
	defer func() {
		if len(pending) != 0 {
			t.Errorf("sandboxes awaiting deletion with their last observed states: %v", pending)
		}
	}()
	// The driver commits FAILED before it deletes the final sandbox.
	eventually(t, "all failed-start sandboxes to be deleted", func() bool {
		for _, sb := range w.fake.Sandboxes(w.osOrg) {
			id := sb.GetSandboxId()
			if _, tracked := pending[id]; !tracked {
				continue
			}
			if state := sb.GetState(); state == ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
				delete(pending, id)
			} else {
				pending[id] = state
			}
		}
		return len(pending) == 0
	})
	w.fake.SetExec(nil, nil)

	// The maximum queue wait ends a session that never got capacity.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	s = w.createSession(t, member, profileID)
	stays(t, "queued", func() bool {
		return w.session(t, s.GetDevSessionId()).GetState() == sandrunv1.RequestState_REQUEST_STATE_QUEUED
	})
	w.env.Clock.Advance(time.Hour)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_EXPIRED)
	if s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_MAX_QUEUE_WAIT {
		t.Fatalf("expired %v", s)
	}
}

func (w *workWorld) runningSession(t *testing.T) (*sandrunv1.DevSession, string) {
	t.Helper()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	return s, w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId()
}

func collectExec(t *testing.T, stream interface {
	Receive() bool
	Err() error
}, msg func() *sandrunv1.DevSessionExecEvent) ([]*sandrunv1.DevSessionExecEvent, error) {
	var out []*sandrunv1.DevSessionExecEvent
	for stream.Receive() {
		out = append(out, msg())
	}
	return out, stream.Err()
}

func TestDevSessionExecResumeOffsets(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	s, _ := w.runningSession(t)
	ctx := t.Context()
	client := w.env.User(w.org.Member).DevSession
	execID := api.NewID()
	code := int64(3)
	emit := func(onEvent func(ownsand.ExecEvent) error, events ...ownsand.ExecEvent) (*ownsand.ExecResult, error) {
		for _, ev := range events {
			if err := onEvent(ev); err != nil {
				return nil, err
			}
		}
		return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}
	w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		return emit(p.OnEvent,
			ownsand.ExecEvent{Kind: ownsand.ExecEventStderr, Data: []byte("warn")},
			ownsand.ExecEvent{Kind: ownsand.ExecEventOutputTruncated, MissingStdout: ownsand.ByteRange{To: 20}},
		)
	}, func(_ context.Context, _, id string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		if id != execID {
			return nil, fmt.Errorf("attach exec ID = %s, want %s", id, execID)
		}
		switch {
		case p.StdoutOffset == 10 && p.StderrOffset == 30:
			return emit(p.OnEvent,
				ownsand.ExecEvent{Kind: ownsand.ExecEventOutputTruncated, MissingStdout: ownsand.ByteRange{From: 10, To: 20}},
				ownsand.ExecEvent{Kind: ownsand.ExecEventStdout, Offset: 20, Data: []byte("data")},
				ownsand.ExecEvent{Kind: ownsand.ExecEventOutputTruncated, MissingStderr: ownsand.ByteRange{From: 30, To: 40}},
			)
		case p.StdoutOffset == 20 && p.StderrOffset == 30:
			return emit(p.OnEvent, ownsand.ExecEvent{Kind: ownsand.ExecEventStderr, Offset: p.StderrOffset, Data: []byte("next")})
		case p.StdoutOffset == 100 && p.StderrOffset == 300:
			return emit(p.OnEvent, ownsand.ExecEvent{
				Kind:          ownsand.ExecEventOutputTruncated,
				MissingStdout: ownsand.ByteRange{From: 100, To: 200},
				MissingStderr: ownsand.ByteRange{From: 300, To: 400},
			})
		default:
			return nil, fmt.Errorf("unexpected attach offsets (%d, %d)", p.StdoutOffset, p.StderrOffset)
		}
	})

	checkEvents := func(t *testing.T, events []*sandrunv1.DevSessionExecEvent, err error, count int) {
		t.Helper()
		if err != nil || len(events) != count || events[count-1].GetExited().GetExitCode() != code {
			t.Fatalf("events = %v, err = %v, want %d events ending with exit code %d", events, err, count, code)
		}
	}
	checkGap := func(t *testing.T, ev *sandrunv1.DevSessionExecEvent, stdout, stderr int64) *sandrunv1.DevSessionExecOutputTruncated {
		t.Helper()
		gap := ev.GetOutputTruncated()
		if gap == nil || gap.GetStdoutOffset() != stdout || gap.GetStderrOffset() != stderr {
			t.Errorf("truncation = %v, want offsets (%d, %d)", gap, stdout, stderr)
		}
		return gap
	}
	checkOutput := func(t *testing.T, output *sandrunv1.DevSessionExecOutput, offset int64, data string) {
		t.Helper()
		if output.GetOffset() != offset || string(output.GetData()) != data {
			t.Errorf("output = %v, want %q at offset %d", output, data, offset)
		}
	}
	attach := func(t *testing.T, stdout, stderr int64, count int) []*sandrunv1.DevSessionExecEvent {
		t.Helper()
		stream, err := client.AttachDevSessionExec(ctx, connect.NewRequest(&sandrunv1.AttachDevSessionExecRequest{
			OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ExecId: execID,
			StdoutOffset: stdout, StderrOffset: stderr,
		}))
		if err != nil {
			t.Fatal(err)
		}
		events, err := collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
		checkEvents(t, events, err, count)
		return events
	}

	t.Run("fresh exec", func(t *testing.T) {
		stream, err := client.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{
			OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ExecId: execID, Command: []string{"make", "test"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		events, err := collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
		checkEvents(t, events, err, 3)
		checkOutput(t, events[0].GetStderr(), 0, "warn")
		checkGap(t, events[1], 20, 4)
	})
	t.Run("attach and resume", func(t *testing.T) {
		events := attach(t, 10, 30, 4)
		firstGap := checkGap(t, events[0], 20, 30)
		checkOutput(t, events[1].GetStdout(), 20, "data")
		checkGap(t, events[2], 24, 40)

		resumed := attach(t, firstGap.GetStdoutOffset(), firstGap.GetStderrOffset(), 2)
		checkOutput(t, resumed[0].GetStderr(), 30, "next")
	})
	t.Run("both streams", func(t *testing.T) {
		events := attach(t, 100, 300, 2)
		checkGap(t, events[0], 200, 400)
	})
}

func TestDevSessionExecGuestTimestamps(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	s, _ := w.runningSession(t)
	start := time.Date(2024, time.January, 1, 0, 0, 0, 123456789, time.UTC)
	end := time.Date(2024, time.January, 1, 0, 0, 2, 987654321, time.UTC)
	epoch := time.Unix(0, 0).UTC()
	for _, tc := range []struct {
		name               string
		start, end         *time.Time
		wantStart, wantEnd *timestamppb.Timestamp
	}{
		{"nanoseconds", &start, &end, &timestamppb.Timestamp{Seconds: 1704067200, Nanos: 123456789}, &timestamppb.Timestamp{Seconds: 1704067202, Nanos: 987654321}},
		{"epoch-without-end", &epoch, nil, &timestamppb.Timestamp{}, nil},
		{"epoch-without-start", nil, &epoch, nil, &timestamppb.Timestamp{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execID := api.NewID()
			const stdout, stderr = "hello", "late"
			started := ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: execID, StartTime: tc.start}
			code := int64(3)
			exited := ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, ExitCode: &code, EndTime: tc.end, TimedOut: true}
			w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
				for _, ev := range []ownsand.ExecEvent{
					started,
					{Kind: ownsand.ExecEventStdout, ExecID: execID, Data: []byte(stdout)},
					{Kind: ownsand.ExecEventStderr, ExecID: execID, Data: []byte(stderr)},
				} {
					if err := p.OnEvent(ev); err != nil {
						return nil, err
					}
				}
				return &exited, nil
			}, func(_ context.Context, _, id string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
				if id != execID || p.StdoutOffset != int64(len(stdout)) || p.StderrOffset != int64(len(stderr)) {
					return nil, errors.New("attach did not resume the completed exec at the consumed offsets")
				}
				if err := p.OnEvent(started); err != nil {
					return nil, err
				}
				return &exited, nil
			})
			check := func(events []*sandrunv1.DevSessionExecEvent) {
				t.Helper()
				gotStart := events[0].GetStarted()
				gotEnd := events[len(events)-1].GetExited()
				if gotStart.GetExecId() != execID || !proto.Equal(gotStart.GetStartTime(), tc.wantStart) {
					t.Errorf("started = %v, want exec %s and time %v", gotStart, execID, tc.wantStart)
				}
				if gotEnd.GetExitCode() != code || !gotEnd.GetTimedOut() || !proto.Equal(gotEnd.GetEndTime(), tc.wantEnd) {
					t.Errorf("exited = %v, want exit code %d, timed_out and time %v", gotEnd, code, tc.wantEnd)
				}
			}

			ctx := t.Context()
			owner := w.env.User(w.org.Member)
			stream, err := owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{
				OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ExecId: execID, Command: []string{"make", "test"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			events, err := collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
			if err != nil || len(events) != 4 || string(events[1].GetStdout().GetData()) != stdout || string(events[2].GetStderr().GetData()) != stderr {
				t.Fatalf("exec events = %v, err = %v", events, err)
			}
			check(events)

			attach, err := w.env.User(w.org.Member).DevSession.AttachDevSessionExec(ctx, connect.NewRequest(&sandrunv1.AttachDevSessionExecRequest{
				OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ExecId: execID,
				StdoutOffset: int64(len(events[1].GetStdout().GetData())), StderrOffset: int64(len(events[2].GetStderr().GetData())),
			}))
			if err != nil {
				t.Fatal(err)
			}
			events, err = collectExec(t, attach, func() *sandrunv1.DevSessionExecEvent { return attach.Msg().GetEvent() })
			if err != nil || len(events) != 2 {
				t.Fatalf("attach events = %v, err = %v", events, err)
			}
			check(events)
		})
	}
}

func TestDevSessionExec(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{StreamLimit: 600 * time.Millisecond, KeepaliveInterval: 150 * time.Millisecond})
	ctx := context.Background()
	s, _ := w.runningSession(t)
	var mu sync.Mutex
	var got ownsand.ExecParams
	rotatedID := api.NewID()
	w.fake.SetExec(func(ctx context.Context, id string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		mu.Lock()
		got = p
		mu.Unlock()
		if p.Command[0] == "mkdir" {
			code := int64(0)
			return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
		}
		if p.Command[0] == "sleep" {
			for _, ev := range []ownsand.ExecEvent{
				{Kind: ownsand.ExecEventStdout, Offset: 0, Data: []byte("hello")},
				{Kind: ownsand.ExecEventStderr, Offset: 0, Data: []byte("ok")},
			} {
				if err := p.OnEvent(ev); err != nil {
					return nil, err
				}
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_ = p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted, ExecID: p.ExecID})
		_ = p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStdout, ExecID: p.ExecID, Offset: 0, Data: []byte("hello")})
		code := int64(3)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, func(ctx context.Context, id, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		if execID != rotatedID || p.StdoutOffset != 5 || p.StderrOffset != 2 {
			return nil, fmt.Errorf("attach got exec %s at offsets (%d, %d), want exec %s at (5, 2)", execID, p.StdoutOffset, p.StderrOffset, rotatedID)
		}
		if err := p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStderr, ExecID: execID, Offset: p.StderrOffset, Data: []byte("late")}); err != nil {
			return nil, err
		}
		sig := int32(9)
		return &ownsand.ExecResult{ExecID: execID, Kind: ownsand.ExecResultExited, Signal: &sig}, nil
	})
	ownerKeyID, ownerKey := w.env.NewAPIKey(w.org.ID, w.org.Member)
	owner := w.env.Bearer(ownerKey)
	execID := api.NewID()
	stream, err := owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: strings.ToUpper(execID), Command: []string{"make", "test"}, Env: map[string]string{"TOKEN": "SECRET-ENV-VALUE"}, WorkingDirectory: "src"}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
	if err != nil || len(events) != 3 || events[0].GetStarted() == nil || string(events[1].GetStdout().GetData()) != "hello" || events[2].GetExited().GetExitCode() != 3 {
		t.Fatalf("events %v %v", events, err)
	}
	firstUse := w.env.Clock.Now()
	if got := keyLastUsed(t, w.env, ownerKeyID); got == nil || !got.Equal(firstUse) {
		t.Fatalf("last use after exec admission = %v, want %v", got, firstUse)
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
	w.env.Clock.Advance(30 * time.Second)
	start := time.Now()
	stream, err = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: rotatedID, Command: []string{"sleep", "100"}}))
	if err != nil {
		t.Fatal(err)
	}
	events, err = collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
	if err != nil || time.Since(start) > 5*time.Second || len(events) < 3 {
		t.Fatalf("rotated stream %v %v", events, err)
	}
	if got := keyLastUsed(t, w.env, ownerKeyID); got == nil || !got.Equal(firstUse) {
		t.Fatalf("last use after repeated exec = %v, want %v", got, firstUse)
	}
	stdout, stderr := events[0].GetStdout(), events[1].GetStderr()
	if string(stdout.GetData()) != "hello" || string(stderr.GetData()) != "ok" {
		t.Fatalf("unexpected output before rotation: %v", events[:2])
	}
	for _, ev := range events[2:] {
		if ev.GetKeepalive() == nil {
			t.Fatalf("non-keepalive event %v", ev)
		}
	}

	// Attach continues from offsets.
	astream, err := owner.DevSession.AttachDevSessionExec(ctx, connect.NewRequest(&sandrunv1.AttachDevSessionExecRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId:       rotatedID,
		StdoutOffset: stdout.GetOffset() + int64(len(stdout.GetData())),
		StderrOffset: stderr.GetOffset() + int64(len(stderr.GetData()))}))
	if err != nil {
		t.Fatal(err)
	}
	events, err = collectExec(t, astream, func() *sandrunv1.DevSessionExecEvent { return astream.Msg().GetEvent() })
	if err != nil || len(events) != 2 || events[0].GetStderr().GetOffset() != 2 || events[1].GetExited().GetSignal() != 9 {
		t.Fatalf("attach %v %v", events, err)
	}

	// Only the owner runs commands, also an organization owner cannot.
	otherKeyID, otherKey := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	stream, _ = w.env.Bearer(otherKey).DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: api.NewID(), Command: []string{"id"}}))
	_, err = collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return nil })
	wantCode(t, "organization owner execs", err, connect.CodePermissionDenied)
	if got := keyLastUsed(t, w.env, otherKeyID); got != nil {
		t.Fatalf("last use after denied exec = %v, want nil", got)
	}
	for name, req := range map[string]*sandrunv1.ExecDevSessionRequest{
		"bad exec id":   {ExecId: "x", Command: []string{"id"}},
		"empty command": {ExecId: api.NewID()},
	} {
		req.OrganizationId, req.DevSessionId = w.org.ID, s.GetDevSessionId()
		stream, _ = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(req))
		_, err = collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return nil })
		wantCode(t, name, err, connect.CodeInvalidArgument)
	}
	// A session that is not RUNNING accepts no command.
	if _, err := owner.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	stream, _ = owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		ExecId: api.NewID(), Command: []string{"id"}}))
	_, err = collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return nil })
	wantCode(t, "ended session", err, connect.CodeFailedPrecondition)
}

func TestDevSessionFiles(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	s, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	writeID := api.NewID()
	data := []byte(strings.Repeat("0123456789", 300))
	write := func(offset int, chunk []byte, final bool) (*sandrunv1.WriteDevSessionFileResponse, error) {
		resp, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
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
	_, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
		OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), WriteId: writeID,
		Path: "out/other.txt", Data: data[:1000],
	}))
	wantCode(t, "changed path for the same write ID", err, connect.CodeInvalidArgument)
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
	_, err = owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		WriteId: api.NewID(), Path: "/x", Data: make([]byte, 1<<20+1)}))
	wantCode(t, "chunk above 1 MiB", err, connect.CodeInvalidArgument)
	_, err = owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		WriteId: api.NewID(), Path: "/x", Offset: math.MaxInt64, Data: []byte("x"), Final: true}))
	wantCode(t, "chunk end offset overflows", err, connect.CodeInvalidArgument)
	_, err = w.env.User(w.org.Admin).DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
		WriteId: api.NewID(), Path: "/x", Data: []byte("x"), Final: true}))
	wantCode(t, "admin writes", err, connect.CodePermissionDenied)

	// Read from an offset with the same read ID: the second call reads the
	// same snapshot.
	readID := api.NewID()
	read := func(offset int64) ([]byte, int64, error) {
		stream, err := owner.DevSession.ReadDevSessionFile(ctx, connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(),
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

func TestDevSessionFilesWriteRanges(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	session, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)

	type chunk struct {
		offset int64
		data   string
		final  bool
		code   connect.Code
	}
	for _, tc := range []struct {
		name     string
		chunks   []chunk
		existing string
		want     string
	}{
		{
			name: "equal overlap",
			chunks: []chunk{
				{offset: 0, data: "abc"},
				{offset: 1, data: "bc"},
				{offset: 3, final: true},
			},
			want: "abc",
		},
		{
			name: "equal overlap at finish",
			chunks: []chunk{
				{offset: 0, data: "abc"},
				{offset: 1, data: "bc", final: true},
			},
			want: "abc",
		},
		{
			name: "same start extension",
			chunks: []chunk{
				{offset: 0, data: "ab"},
				{offset: 0, data: "abc", final: true},
			},
			want: "abc",
		},
		{
			name: "overlap bridges ranges",
			chunks: []chunk{
				{offset: 4, data: "efg"},
				{offset: 0, data: "ab"},
				{offset: 1, data: "bcde"},
				{offset: 7, final: true},
			},
			want: "abcdefg",
		},
		{
			name: "conflict with unpublished target",
			chunks: []chunk{
				{offset: 0, data: "abc"},
				{offset: 1, data: "XY", code: connect.CodeFailedPrecondition},
				{offset: 3, final: true},
			},
			want: "abc",
		},
		{
			name: "conflict in second range preserves target and staged bytes",
			chunks: []chunk{
				{offset: 0, data: "ab"},
				{offset: 4, data: "ef"},
				{offset: 1, data: "bCDXfg", code: connect.CodeFailedPrecondition},
				{offset: 2, data: "cd"},
				{offset: 6, final: true},
			},
			existing: "previous file",
			want:     "abcdef",
		},
		{
			name: "gap",
			chunks: []chunk{
				{offset: 0, data: "a"},
				{offset: 2, data: "c"},
				{offset: 3, final: true, code: connect.CodeFailedPrecondition},
			},
		},
		{
			name: "range exceeds final size",
			chunks: []chunk{
				{offset: 0, data: "abc"},
				{offset: 2, final: true, code: connect.CodeFailedPrecondition},
			},
		},
		{
			name: "empty file",
			chunks: []chunk{
				{offset: 42},
				{offset: 0, final: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeID := api.NewID()
			path := "/home/dev/work/out/" + writeID
			content, exists := tc.existing, tc.existing != ""
			if exists {
				w.fake.SetFile(w.osOrg, sandboxID, path, []byte(content))
			}
			for i, c := range tc.chunks {
				res, err := owner.DevSession.WriteDevSessionFile(t.Context(), connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
					OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(), WriteId: writeID,
					Path: path, Offset: c.offset, Data: []byte(c.data), Final: c.final,
				}))
				if c.code != 0 {
					wantCode(t, fmt.Sprintf("chunk %d", i), err, c.code)
				} else {
					if err != nil {
						t.Fatalf("chunk %d: %v", i, err)
					}
					ack := res.Msg
					if ack.GetOffset() != c.offset || ack.GetLength() != int64(len(c.data)) || ack.GetFinished() != c.final {
						t.Fatalf("chunk %d acknowledgment: %v", i, ack)
					}
					if c.final {
						if ack.GetSizeBytes() != int64(len(tc.want)) {
							t.Fatalf("finished size %d, want %d", ack.GetSizeBytes(), len(tc.want))
						}
						content, exists = tc.want, true
					}
				}
				got, found := w.fake.File(w.osOrg, sandboxID, path)
				if found != exists || string(got) != content {
					t.Fatalf("after chunk %d: file exists %t with bytes %q, want exists %t with bytes %q", i, found, got, exists, content)
				}
			}
		})
	}
}

func TestDevSessionFilesReadPathBinding(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	session, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	const firstPath = "/home/dev/work/out/a.txt"
	w.fake.SetFile(w.osOrg, sandboxID, firstPath, []byte("abcde"))
	w.fake.SetFile(w.osOrg, sandboxID, "/home/dev/work/out/b.txt", []byte("vwxyz"))

	read := func(readID, path string, offset int64) ([]*sandrunv1.ReadDevSessionFileResponse, error) {
		stream, err := owner.DevSession.ReadDevSessionFile(t.Context(), connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(),
			ReadId: readID, Path: path, Offset: offset,
		}))
		if err != nil {
			return nil, err
		}
		var events []*sandrunv1.ReadDevSessionFileResponse
		for stream.Receive() {
			events = append(events, stream.Msg())
		}
		return events, stream.Err()
	}
	wantRead := func(readID, path string, offset int64, data string, size int64) {
		t.Helper()
		events, err := read(readID, path, offset)
		if err != nil || len(events) != 2 {
			t.Fatalf("read %q at %d returned %v, error %v", path, offset, events, err)
		}
		chunk, end := events[0].GetChunk(), events[1].GetEnd()
		if chunk.GetOffset() != offset || string(chunk.GetData()) != data || end.GetSizeBytes() != size {
			t.Fatalf("read %q at %d returned %v", path, offset, events)
		}
	}

	readID := api.NewID()
	wantRead(readID, "out/a.txt", 0, "abcde", 5)

	events, err := read(readID, "out/b.txt", 2)
	if len(events) != 0 {
		t.Errorf("changed path returned events: %v", events)
	}
	wantCode(t, "changed path for the same read ID", err, connect.CodeFailedPrecondition)
	const restart = "Restart the file transfer from the beginning with a new operation ID."
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Message() != restart {
		t.Fatalf("changed path returned %v, want %q", err, restart)
	}

	w.fake.SetFile(w.osOrg, sandboxID, firstPath, []byte("changed"))
	wantRead(readID, firstPath, 2, "cde", 5)
	wantRead(api.NewID(), "out/b.txt", 2, "xyz", 5)
}

func TestDevSessionFileReadSnapshotEOF(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	session, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	const path = "/home/dev/work/read.txt"
	w.fake.SetFile(w.osOrg, sandboxID, path, []byte("abc"))

	read := func(readID string, offset int64) ([]*sandrunv1.ReadDevSessionFileResponse, error) {
		stream, err := owner.DevSession.ReadDevSessionFile(t.Context(), connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(),
			ReadId: readID, Path: path, Offset: offset,
		}))
		if err != nil {
			return nil, err
		}
		var events []*sandrunv1.ReadDevSessionFileResponse
		for stream.Receive() {
			events = append(events, stream.Msg())
		}
		return events, stream.Err()
	}
	wantPastEOF := func(t *testing.T, readID string) {
		t.Helper()
		events, err := read(readID, 4)
		if len(events) != 0 {
			t.Errorf("past EOF returned events: %v", events)
		}
		wantCode(t, "past EOF", err, connect.CodeOutOfRange)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Message() != "OwnSand: offset is beyond the end of the file" {
			t.Fatalf("past EOF returned %v", err)
		}
	}

	t.Run("fresh read past EOF", func(t *testing.T) {
		wantPastEOF(t, api.NewID())
	})
	readID := api.NewID()
	t.Run("read suffix", func(t *testing.T) {
		events, err := read(readID, 2)
		if err != nil || len(events) != 2 {
			t.Fatalf("suffix read returned %v, error %v", events, err)
		}
		chunk, end := events[0].GetChunk(), events[1].GetEnd()
		if chunk.GetOffset() != 2 || string(chunk.GetData()) != "c" || end.GetSizeBytes() != 3 {
			t.Fatalf("suffix read returned %v", events)
		}
	})

	w.fake.SetFile(w.osOrg, sandboxID, path, []byte("replaced"))
	t.Run("resumed read past snapshot EOF", func(t *testing.T) {
		wantPastEOF(t, readID)
	})
	t.Run("resumed read at snapshot EOF", func(t *testing.T) {
		events, err := read(readID, 3)
		if err != nil || len(events) != 1 || events[0].GetEnd().GetSizeBytes() != 3 {
			t.Fatalf("read at EOF returned %v, error %v", events, err)
		}
	})
}

func TestDevSessionWindowsUNCPaths(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	const workspace = `C:\work`
	profile := devProfileProto("windows-dev", 0, 0)
	profile.GuestOs = sandrunv1.GuestOs_GUEST_OS_WINDOWS
	profile.Source = &sandrunv1.RunnerProfile_BaseImage{BaseImage: profile.GuestOs}
	profile.WorkspacePath = workspace
	owner := w.env.User(w.org.Member)
	session := w.createSession(t, owner, w.createProfile(t, profile))
	sessionID := session.GetDevSessionId()
	session = w.waitState(t, sessionID, sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	sandboxID := w.runner(t, session.GetRunnerId()).GetOwnsandSandboxId()
	ctx := t.Context()

	exec := func(t *testing.T, input, want string) {
		t.Helper()
		dirs := make(chan string, 1)
		w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
			dirs <- p.WorkingDirectory
			code := int64(0)
			return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
		}, nil)
		stream, err := owner.DevSession.ExecDevSession(ctx, connect.NewRequest(&sandrunv1.ExecDevSessionRequest{
			OrganizationId: w.org.ID, DevSessionId: sessionID, ExecId: api.NewID(),
			Command: []string{"cmd.exe", "/c", "cd"}, WorkingDirectory: input,
		}))
		if err != nil {
			t.Fatal(err)
		}
		events, err := collectExec(t, stream, func() *sandrunv1.DevSessionExecEvent { return stream.Msg().GetEvent() })
		if err != nil || len(events) != 1 || events[0].GetExited() == nil {
			t.Fatalf("exec events %v, error %v", events, err)
		}
		select {
		case got := <-dirs:
			if got != want {
				t.Errorf("forwarded working directory %q, want %q", got, want)
			}
		default:
			t.Error("working directory did not reach OwnSand")
		}
	}
	write := func(path string) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
		return owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: sessionID, WriteId: api.NewID(),
			Path: path, Data: []byte("written"), Final: true,
		}))
	}
	read := func(path string) (*connect.ServerStreamForClient[sandrunv1.ReadDevSessionFileResponse], error) {
		return owner.DevSession.ReadDevSessionFile(ctx, connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: sessionID, ReadId: api.NewID(), Path: path,
		}))
	}

	for _, tc := range []struct{ name, dir, path string }{
		{"forward", "//buildhost/share", "//buildhost/share/results.txt"},
		{"forward then back", `/\buildhost/share`, `/\buildhost/share/results.txt`},
		{"back then forward", `\/buildhost/share`, `\/buildhost/share/results.txt`},
		{"back", `\\buildhost\share`, `\\buildhost\share\results.txt`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec(t, tc.dir, tc.dir)

			resp, err := write(tc.path)
			if err != nil || !resp.Msg.GetFinished() {
				t.Fatalf("write response %v, error %v", resp, err)
			}
			if data, ok := w.fake.File(w.osOrg, sandboxID, tc.path); !ok || string(data) != "written" {
				t.Errorf("file at exact path %q: found %t, contents %q", tc.path, ok, data)
			}

			w.fake.SetFile(w.osOrg, sandboxID, tc.path, []byte("seeded"))
			stream, err := read(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			var data []byte
			var end *sandrunv1.DevSessionFileEnd
			for stream.Receive() {
				data = append(data, stream.Msg().GetChunk().GetData()...)
				if e := stream.Msg().GetEnd(); e != nil {
					end = e
				}
			}
			if err := stream.Err(); err != nil || string(data) != "seeded" || end.GetSizeBytes() != int64(len(data)) {
				t.Fatalf("read at exact path %q: contents %q, end %v, error %v", tc.path, data, end, err)
			}
		})
	}

	t.Run("empty working directory", func(t *testing.T) {
		exec(t, "", workspace)
	})
	_, err := write("")
	wantCode(t, "empty write path", err, connect.CodeInvalidArgument)
	stream, err := read("")
	if err == nil {
		for stream.Receive() {
			t.Error("empty read path returned a message")
		}
		err = stream.Err()
	}
	wantCode(t, "empty read path", err, connect.CodeInvalidArgument)
}

func TestDevSessionExecOwnSandValidation(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	s, _ := w.runningSession(t)
	sdk, err := (ownsandclient.SDKFactory{BaseURL: "http://127.0.0.1:1"}).New(w.osOrg, ownsandKey('a'))
	if err != nil {
		t.Fatal(err)
	}
	w.fake.SetExec(sdk.Exec, nil)

	owner := w.env.User(w.org.Member)
	stream, err := owner.DevSession.ExecDevSession(t.Context(), connect.NewRequest(&sandrunv1.ExecDevSessionRequest{
		OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ExecId: api.NewID(),
		Command: []string{"id"}, Env: map[string]string{"BIG": strings.Repeat("x", 300<<10)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if stream.Receive() {
		t.Fatal("oversized exec returned an event")
	}
	err = stream.Err()
	wantCode(t, "oversized exec", err, connect.CodeInvalidArgument)
	var ce *connect.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message(), "more than 262144") || strings.Contains(ce.Message(), "try again") {
		t.Fatalf("oversized exec returned %v", err)
	}
}

func TestDevSessionFilesOwnSandErrors(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	s, sandboxID := w.runningSession(t)
	owner := w.env.User(w.org.Member)
	upstream, err := w.fake.New(w.osOrg, ownsandKey('a'))
	if err != nil {
		t.Fatal(err)
	}
	const path = "/home/dev/work/transfer.txt"
	w.fake.SetFile(w.osOrg, sandboxID, path, []byte("hello"))
	readID := api.NewID()
	read := func(offset int64) (*connect.ServerStreamForClient[sandrunv1.ReadDevSessionFileResponse], error) {
		return owner.DevSession.ReadDevSessionFile(ctx, connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), ReadId: readID, Path: path, Offset: offset,
		}))
	}
	stream, err := read(0)
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}

	const keyDiagnostic = "OwnSand rejected the organization's OwnSand API key"
	for _, tc := range []struct {
		name    string
		code    connect.Code
		message string
		want    string
	}{
		{"operation mismatch", connect.CodePermissionDenied, "request does not match the recorded operation", "Restart the file transfer from the beginning with a new operation ID."},
		{"other permission error", connect.CodePermissionDenied, "access denied", keyDiagnostic},
		{"rejected API key", connect.CodeUnauthenticated, "invalid API key", keyDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Record both writes upstream so SandRun's first call reopens each write.
			writeIDs := []string{api.NewID(), api.NewID()}
			for _, id := range writeIDs {
				write, err := upstream.OpenFileWrite(sandboxID, ownsand.FileWriteParams{WriteID: id, Path: path})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := write.WriteChunk(ctx, 0, []byte("h")); err != nil {
					t.Fatal(err)
				}
			}
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.FileError = &ownsand.DataPathError{Err: connect.NewWireError(tc.code, errors.New(tc.message))}
			})
			t.Cleanup(func() { w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.FileError = nil }) })
			wantError := func(operation string, err error) {
				t.Helper()
				wantCode(t, operation, err, connect.CodeFailedPrecondition)
				var ce *connect.Error
				if !errors.As(err, &ce) || ce.Message() != tc.want {
					t.Fatalf("%s: got %v, want %q", operation, err, tc.want)
				}
			}
			stream, err := read(1)
			if err != nil {
				t.Fatal(err)
			}
			if stream.Receive() {
				t.Fatalf("continued read returned a result: %v", stream.Msg())
			}
			wantError("continued read", stream.Err())
			for i, final := range []bool{false, true} {
				// A credential rejection invalidates the link. Relink before
				// checking the next RPC's handling of the upstream rejection.
				if tc.code == connect.CodeUnauthenticated {
					if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
						t.Fatal(err)
					}
				}
				resp, err := owner.DevSession.WriteDevSessionFile(ctx, connect.NewRequest(&sandrunv1.WriteDevSessionFileRequest{
					OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId(), WriteId: writeIDs[i],
					Path: path, Offset: 1, Data: []byte("ello"), Final: final,
				}))
				if resp != nil {
					t.Fatalf("write final=%t returned a result: %v", final, resp.Msg)
				}
				operation := "reopened write chunk"
				if final {
					operation = "reopened write finalization"
				}
				wantError(operation, err)
			}
		})
	}
}

func newWorkWorld(t *testing.T, ds devsession.Timing) *workWorld {
	t.Helper()
	return newWorkWorldWithTiming(t, ds, nil)
}

func newWorkWorldWithTiming(t *testing.T, ds devsession.Timing, timing *work.Timing) *workWorld {
	t.Helper()
	fake := ownsandfake.New()
	return newWorkWorldWithOptions(t, fake, layers.Options{OwnSand: fake, WorkTiming: timing, DevSessionTiming: ds})
}

func newWorkWorldWithProvider(t *testing.T, ds devsession.Timing, fake *ownsandfake.Fake, provider ownsandclient.Factory) *workWorld {
	t.Helper()
	return newWorkWorldWithOptions(t, fake, layers.Options{OwnSand: provider, DevSessionTiming: ds})
}

func newWorkWorldWithOptions(t *testing.T, fake *ownsandfake.Fake, opts layers.Options) *workWorld {
	t.Helper()
	w := &world{fake: fake, l: &layers.Layers{}}
	w.env = testenv.New(t, testenv.WithLayerOptions(opts, w.l))
	w.org = w.env.NewOrg("alice")
	ww := &workWorld{world: w, osOrg: api.NewID()}
	w.fake.AddOrg(ww.osOrg, ownsandKey('a'))
	if _, err := w.setLink(w.org.Admin, ww.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	return ww
}

func (w *workWorld) createSession(t *testing.T, c *testenv.Clients, profileID string) *sandrunv1.DevSession {
	t.Helper()
	resp, err := c.DevSession.CreateDevSession(context.Background(), connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID, Label: "build"}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetDevSession()
}

func (w *workWorld) session(t *testing.T, id string) *sandrunv1.DevSession {
	t.Helper()
	resp, err := w.env.User(w.org.Owner).DevSession.GetDevSession(context.Background(), connect.NewRequest(&sandrunv1.GetDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetDevSession()
}

func (w *workWorld) waitState(t *testing.T, id string, state sandrunv1.RequestState) *sandrunv1.DevSession {
	t.Helper()
	var last *sandrunv1.DevSession
	eventually(t, "dev session "+state.String(), func() bool {
		last = w.session(t, id)
		return last.GetState() == state
	})
	return last
}

// TestCreateDevSessionDuringProfileDeletion: CreateDevSession waits for a
// profile deletion that holds the profile row and then fails with
// not_found, so no session of a deleted profile stays queued.
func TestCreateDevSessionDuringProfileDeletion(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := w.env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Cancel any pending creation before releasing the lock on failure.
		cancel()
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRollback()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM runner_profiles WHERE runner_profile_id = $1 FOR UPDATE", profileID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.env.User(w.org.Member).DevSession.CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: w.org.ID, RunnerProfileId: profileID}))
		done <- err
	}()
	blockerPID := tx.Conn().PgConn().PID()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var waiting bool
	for !waiting {
		select {
		case err := <-done:
			t.Fatalf("CreateDevSession finished before waiting for the profile lock: %v", err)
		case <-ctx.Done():
			t.Fatalf("waiting for CreateDevSession to block on the profile lock: %v", ctx.Err())
		case <-ticker.C:
		}
		// Check which statement is blocked to verify the shared lock itself.
		if err := w.env.DB.Pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND query LIKE 'SELECT 1 FROM runner_profiles WHERE % FOR SHARE'
			  AND $1 = ANY(pg_blocking_pids(pid))
		)`, blockerPID).Scan(&waiting); err != nil {
			t.Fatalf("observe CreateDevSession waiting for the profile lock: %v", err)
		}
	}
	t.Logf("CreateDevSession waited for the profile lock held by PostgreSQL backend %d", blockerPID)
	if _, err := tx.Exec(ctx, "DELETE FROM runner_profiles WHERE runner_profile_id = $1", profileID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		wantCode(t, "create during deletion", err, connect.CodeNotFound)
	case <-ctx.Done():
		t.Fatalf("waiting for CreateDevSession after profile deletion: %v", ctx.Err())
	}
	var n int
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM requests WHERE runner_profile_id = $1", profileID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d requests of the deleted profile (%v)", n, err)
	}
}
