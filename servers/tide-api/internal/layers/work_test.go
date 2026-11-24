// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// TestMembershipEndWithSessionsAndKeys: removing a member revokes the keys,
// deletes the running session's sandbox (ENDED, MEMBER_REMOVED) and cancels
// the queued one (CANCELED, MEMBER_REMOVED).
func TestMembershipEndWithSessionsAndKeys(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 1 })
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	_, key := w.env.NewAPIKey(w.org.ID, w.org.Member)
	member := w.env.Bearer(key)
	running := w.createSession(t, member, profileID)
	running = w.waitState(t, running.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	queued := w.createSession(t, member, profileID)
	sandboxID := w.runner(t, running.GetRunnerId()).GetOwnsandSandboxId()

	if _, err := w.env.User(w.org.Admin).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: w.org.ID, UserId: w.org.Member})); err != nil {
		t.Fatal(err)
	}
	r := w.session(t, running.GetDevSessionId())
	q := w.session(t, queued.GetDevSessionId())
	if r.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED || r.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED ||
		q.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || q.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED {
		t.Fatalf("sessions %v / %v", r, q)
	}
	eventually(t, "sandbox deleted", func() bool {
		for _, sb := range w.fake.Sandboxes(w.osOrg) {
			if sb.GetSandboxId() == sandboxID {
				return sb.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_DELETED
			}
		}
		return false
	})
	eventually(t, "runner deleted", func() bool {
		return w.runner(t, running.GetRunnerId()).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED
	})
	_, err := member.DevSession.ListDevSessions(ctx, connect.NewRequest(&tidev1.ListDevSessionsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "revoked key", err, connect.CodeUnauthenticated)
}

// TestWarmRunnersAndReclaim: a warm runner serves a session without a new
// sandbox and is replaced; when OwnSand has no capacity, the dispatcher
// deletes the warm runner of the lowest priority profile.
func TestWarmRunnersAndReclaim(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 2 })
	warmID := w.createProfile(t, devProfileProto("warm", 1, tidev1.Priority_PRIORITY_LOW))
	countWarm := func() int {
		resp, err := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&tidev1.ListRunnersRequest{OrganizationId: w.org.ID,
			States: []tidev1.RunnerState{tidev1.RunnerState_RUNNER_STATE_WARM}}))
		if err != nil {
			t.Fatal(err)
		}
		return len(resp.Msg.GetRunners())
	}
	eventually(t, "a warm runner", func() bool { return countWarm() == 1 })
	stays(t, "one warm runner", func() bool { return countWarm() == 1 && w.fake.Active(w.osOrg) == 1 })
	// The session takes the warm runner; a new warm runner replaces it.
	s := w.createSession(t, w.env.User(w.org.Member), warmID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	eventually(t, "a new warm runner", func() bool { return countWarm() == 1 && w.fake.Active(w.osOrg) == 2 })

	// The pool is full (busy session and warm runner). A HIGH priority
	// session reclaims the LOW priority warm runner and starts.
	highID := w.createProfile(t, devProfileProto("high", 0, tidev1.Priority_PRIORITY_HIGH))
	h := w.createSession(t, w.env.User(w.org.Admin), highID)
	h = w.waitState(t, h.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	var reclaimed int
	resp, _ := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&tidev1.ListRunnersRequest{OrganizationId: w.org.ID, IncludeEnded: true}))
	for _, r := range resp.Msg.GetRunners() {
		if r.GetEndReason() == tidev1.RunnerEndReason_RUNNER_END_REASON_WARM_RECLAIMED {
			reclaimed++
		}
	}
	if reclaimed != 1 {
		t.Fatalf("%d reclaimed warm runners", reclaimed)
	}
	// Warm runners wait while a request is uncovered: no warm runner comes
	// back while the pool is full.
	stays(t, "no warm runner while full", func() bool { return w.fake.Active(w.osOrg) <= 2 })
}

func TestWarmReservationDuringProfileDeletion(t *testing.T) {
	t.Run("stale dispatch cannot reserve", func(t *testing.T) {
		w := newWorkWorld(t, devsession.Timing{})
		ctx := context.Background()
		liveID := w.createProfile(t, devProfileProto("live", 0, 0))
		deletedID := w.createProfile(t, devProfileProto("to-delete", 0, 0))
		// The dispatch order is by profile ID. Block its first sandbox call
		// so the pass retains the deleted profile in its snapshot.
		if liveID > deletedID {
			liveID, deletedID = deletedID, liveID
		}
		creating, release := make(chan struct{}, 1), make(chan struct{}, 1)
		defer close(release)
		w.fake.SetCreateHook(func(ctx context.Context) error {
			select {
			case creating <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		enableWarmProfiles(t, w, liveID, deletedID)
		select {
		case <-creating:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch did not reach the first profile")
		}
		if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: deletedID,
		})); err != nil {
			t.Fatal(err)
		}
		w.fake.SetCreateHook(nil)
		// Let the captured call proceed and observe the surviving profile.
		release <- struct{}{}
		eventually(t, "surviving profile warms", func() bool {
			return profileRunnerCount(t, w, liveID, tidev1.RunnerState_RUNNER_STATE_WARM) == 1
		})
		stays(t, "stale profile creates no runner or sandbox", func() bool {
			var n int
			if err := w.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM runners WHERE runner_profile_id = $1", deletedID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n == 0 && w.fake.Calls("CreateSandbox") == 1
		})
	})

	t.Run("deletion catches late reservation", func(t *testing.T) {
		w := newWorkWorld(t, devsession.Timing{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		prepared, continueDelete := make(chan struct{}), make(chan struct{})
		w.l.Profiles.OnDeletion(profile.DeletionHook{
			Name: "test gate after work cleanup",
			Prepare: func(ctx context.Context, _, _ string) error {
				close(prepared)
				select {
				case <-continueDelete:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		profileID := w.createProfile(t, devProfileProto("to-delete", 0, 0))
		done := make(chan error, 1)
		go func() {
			_, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: profileID,
			}))
			done <- err
		}()
		select {
		case <-prepared:
		case <-ctx.Done():
			t.Fatal("deletion did not complete the initial cleanup")
		}
		// OwnSand commits the sandbox but Tide has no sandbox ID until it
		// can recover the result by replaying the stored idempotency key.
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
			o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
		})
		enableWarmProfiles(t, w, profileID)
		eventually(t, "late reservation holds a sandbox", func() bool {
			return w.fake.Active(w.osOrg) == 1
		})
		var runnerID string
		if err := w.env.DB.Pool.QueryRow(ctx, `SELECT runner_id::text FROM runners
			WHERE runner_profile_id = $1 AND request_id IS NULL AND ownsand_sandbox_id IS NULL`, profileID).Scan(&runnerID); err != nil {
			t.Fatal(err)
		}
		close(continueDelete)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if r := w.runner(t, runnerID); r.GetState() != tidev1.RunnerState_RUNNER_STATE_DELETING ||
			r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
			t.Fatalf("late reservation after profile deletion: %v", r)
		}
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
		eventually(t, "late runner and its sandbox are deleted", func() bool {
			return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED && w.fake.Active(w.osOrg) == 0
		})
		if n := len(w.fake.Sandboxes(w.osOrg)); n != 1 {
			t.Fatalf("got %d sandboxes, want one by idempotency key", n)
		}
	})
}

func enableWarmProfiles(t *testing.T, w *workWorld, ids ...string) {
	t.Helper()
	// Make all profiles eligible in the same dispatch snapshot.
	if _, err := w.env.DB.Pool.Exec(context.Background(), "UPDATE runner_profiles SET warm_count = 1 WHERE runner_profile_id::text = ANY($1)", ids); err != nil {
		t.Fatal(err)
	}
	w.l.Engine.Kick(w.org.ID)
}

func TestWarmCreateRejectionDoesNotSpin(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodePermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			w := newWorkWorld(t, devsession.Timing{})
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = connect.NewWireError(code, errors.New("rejected"))
			})
			profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
			attempts := func(want int) bool {
				return w.fake.Calls("CreateSandbox") == want &&
					profileRunnerCount(t, w, profileID, tidev1.RunnerState_RUNNER_STATE_FAILED) == want
			}

			cooldown := testenv.FastWorkTiming().PassInterval
			for want := 1; want <= 3; want++ {
				eventually(t, "one failed runner for the next attempt", func() bool { return attempts(want) })
				if want < 3 {
					w.env.Clock.Advance(cooldown - time.Millisecond)
					w.l.Engine.Kick(w.org.ID)
					stays(t, "no retry during the cooldown", func() bool { return attempts(want) })
					w.env.Clock.Advance(time.Millisecond)
					w.l.Engine.Kick(w.org.ID)
					cooldown *= 2
				}
			}

			if code == connect.CodeInvalidArgument {
				// Demand bypasses the stopped warm fill on the same profile.
				ctx := context.Background()
				session := w.createSession(t, w.env.User(w.org.Member), profileID)
				session = w.waitState(t, session.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_FAILED)
				req, err := w.env.User(w.org.Member).Work.GetRequest(ctx, connect.NewRequest(&tidev1.GetRequestRequest{
					OrganizationId: w.org.ID, RequestId: session.GetDevSessionId(),
				}))
				if err != nil {
					t.Fatal(err)
				}
				if session.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED ||
					req.Msg.GetRequest().GetStartAttempts() != 3 || !attempts(6) {
					t.Fatalf("session %v, request %v, CreateSandbox calls %d", session, req.Msg.GetRequest(), w.fake.Calls("CreateSandbox"))
				}
			}

			before := w.fake.Calls("CreateSandbox")
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
			w.env.Clock.Advance(time.Hour)
			w.l.Engine.Kick(w.org.ID)
			stays(t, "no retries after the hard stop", func() bool { return attempts(before) })
			if n := strings.Count(w.env.Logs.String(), `"msg":"warm_runner_fill_stopped"`); n != 1 {
				t.Fatalf("warm fill stop logs = %d, want 1", n)
			}

			if code == connect.CodePermissionDenied {
				if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
					t.Fatal(err)
				}
			} else {
				renameWarmProfile(t, w, profileID)
			}
			eventually(t, "the warm runner after a configuration change", func() bool {
				return profileRunnerCount(t, w, profileID, tidev1.RunnerState_RUNNER_STATE_WARM) == 1
			})
			if got := w.fake.Calls("CreateSandbox"); got != before+1 {
				t.Fatalf("CreateSandbox calls = %d after recovery, want %d", got, before+1)
			}
		})
	}
}

func TestWarmPreparationFailureBacksOffAndReadyRunnerClearsRetry(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		exitCode := int64(1)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &exitCode}, nil
	}, nil)
	profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
	eventually(t, "a failed preparation and sandbox deletion", func() bool {
		return profileRunnerCount(t, w, profileID, tidev1.RunnerState_RUNNER_STATE_FAILED) == 1 && w.fake.Active(w.osOrg) == 0
	})
	w.fake.SetExec(nil, nil)
	w.l.Engine.Kick(w.org.ID)
	stays(t, "no retry during preparation cooldown", func() bool { return w.fake.Calls("CreateSandbox") == 1 })
	w.env.Clock.Advance(testenv.FastWorkTiming().PassInterval)
	eventually(t, "a warm runner after preparation recovers", func() bool {
		return profileRunnerCount(t, w, profileID, tidev1.RunnerState_RUNNER_STATE_WARM) == 1
	})
	var failures int
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT warm_failures FROM runner_profiles WHERE runner_profile_id = $1", profileID).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("warm failures = %d, want 0 (%v)", failures, err)
	}
}

func TestWarmInFlightRejectionDoesNotBackOffNewProfileRevision(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	started, release, duplicate := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var first sync.Once
	w.fake.SetCreateHook(func(ctx context.Context) error {
		initial := false
		first.Do(func() { initial = true })
		if !initial {
			select {
			case duplicate <- struct{}{}:
			default:
			}
			return nil
		}
		close(started)
		select {
		case <-release:
			return connect.NewWireError(connect.CodeNotFound, errors.New("old profile rejected"))
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("warm create did not start")
	}
	renameWarmProfile(t, w, profileID)
	// Let the reconciler see the open row while the dispatcher still owns it.
	select {
	case <-duplicate:
		t.Fatal("reconciler duplicated the in-flight CreateSandbox call")
	case <-time.After(3 * testenv.FastWorkTiming().ReconcileInterval):
	}
	close(release)
	eventually(t, "warm runner under the new profile revision without advancing the clock", func() bool {
		return profileRunnerCount(t, w, profileID, tidev1.RunnerState_RUNNER_STATE_WARM) == 1
	})
}

func renameWarmProfile(t *testing.T, w *workWorld, profileID string) {
	t.Helper()
	_, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(context.Background(), connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{
		OrganizationId:  w.org.ID,
		RunnerProfileId: profileID,
		Profile:         &tidev1.RunnerProfile{Name: "warm-updated"},
		UpdateMask:      &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func profileRunnerCount(t *testing.T, w *workWorld, profileID string, state tidev1.RunnerState) int {
	t.Helper()
	var n int
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM runners WHERE runner_profile_id = $1 AND state = $2", profileID, int16(state)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGetCapacityAllocatedVCPU(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	work := w.env.User(w.org.Member).Work
	for _, tc := range []struct {
		name       string
		allocation *ownsandv1.DeviceAllocation
		want       int64
	}{
		{"above int32", &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32, VcpuTotalCount: proto.Int64(4294967294)}, 4294967294},
		{"legacy count", &ownsandv1.DeviceAllocation{VcpuCount: 12}, 12},
		{"explicit zero", &ownsandv1.DeviceAllocation{VcpuCount: 12, VcpuTotalCount: proto.Int64(0)}, 0},
		{"missing allocation", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const effectiveLimit int64 = 8589934588
			w.fake.SetDevices(w.osOrg, &ownsandv1.Device{
				DeviceId:                "device-0",
				EffectiveVcpuLimitCount: effectiveLimit,
				Allocation:              tc.allocation,
			})
			resp, err := work.GetCapacity(t.Context(), connect.NewRequest(&tidev1.GetCapacityRequest{OrganizationId: w.org.ID}))
			if err != nil {
				t.Fatal(err)
			}
			devices := resp.Msg.GetDevices()
			if len(devices) != 1 || devices[0].GetOwnsandDeviceId() != "device-0" {
				t.Fatalf("devices = %v, want device-0", devices)
			}
			if got := devices[0].GetAllocation().GetVcpuCount(); got != tc.want {
				t.Errorf("allocated vCPUs = %d, want %d", got, tc.want)
			}
			if got := devices[0].GetEffectiveVcpuLimit(); got != effectiveLimit {
				t.Errorf("effective vCPU limit = %d, want %d", got, effectiveLimit)
			}
		})
	}
}

func TestWorkService(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.SetDevices(w.osOrg, &ownsandv1.Device{DeviceId: "device-0", Name: "linux-box", State: ownsandv1.DeviceState_DEVICE_STATE_ONLINE,
		HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, EffectiveVcpuLimitCount: 16,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 500, VcpuOvercommitPercent: 100}})
	lowID := w.createProfile(t, devProfileProto("low", 0, tidev1.Priority_PRIORITY_LOW))
	highID := w.createProfile(t, devProfileProto("high", 0, tidev1.Priority_PRIORITY_HIGH))
	running := w.createSession(t, w.env.User(w.org.Member), lowID)
	running = w.waitState(t, running.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	low := w.createSession(t, w.env.User(w.org.Member), lowID)
	w.env.Clock.Advance(time.Second)
	high := w.createSession(t, w.env.User(w.org.Admin), highID)
	owner := w.env.User(w.org.Owner)

	// Watch: the snapshot has every open request and runner.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(wctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]bool{}
	for watch.Receive() {
		m := watch.Msg()
		if m.GetSnapshotComplete() != nil {
			break
		}
		if r := m.GetRequest(); r != nil {
			snapshot[r.GetRequestId()] = true
		}
		if r := m.GetRunner(); r != nil {
			snapshot[r.GetRunnerId()] = true
		}
	}
	if !snapshot[running.GetDevSessionId()] || !snapshot[low.GetDevSessionId()] || !snapshot[high.GetDevSessionId()] || !snapshot[running.GetRunnerId()] {
		t.Fatalf("snapshot %v", snapshot)
	}

	// Queue order: HIGH first, then by queue time; open requests first.
	list, err := w.env.User(w.org.Member).Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID,
		Kind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, States: []tidev1.RequestState{tidev1.RequestState_REQUEST_STATE_QUEUED}}))
	if err != nil || len(list.Msg.GetRequests()) != 2 || list.Msg.GetRequests()[0].GetRequestId() != high.GetDevSessionId() {
		t.Fatalf("queue %v %v", list, err)
	}
	runners, err := owner.Work.ListRunners(ctx, connect.NewRequest(&tidev1.ListRunnersRequest{OrganizationId: w.org.ID,
		States: []tidev1.RunnerState{tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK, tidev1.RunnerState_RUNNER_STATE_BUSY}}))
	if err != nil || len(runners.Msg.GetRunners()) != 1 || runners.Msg.GetRunners()[0].GetRunnerId() != running.GetRunnerId() {
		t.Fatalf("active runners %v %v", runners, err)
	}
	// Cancel: admins only, QUEUED only.
	_, err = w.env.User(w.org.Member).Work.CancelRequest(ctx, connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: low.GetDevSessionId()}))
	wantCode(t, "member cancels", err, connect.CodePermissionDenied)
	_, err = owner.Work.CancelRequest(ctx, connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: running.GetDevSessionId()}))
	wantCode(t, "cancel a running request", err, connect.CodeFailedPrecondition)
	c, err := owner.Work.CancelRequest(ctx, connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: low.GetDevSessionId()}))
	if err != nil || c.Msg.GetRequest().GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || c.Msg.GetRequest().GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER {
		t.Fatalf("cancel %v %v", c, err)
	}
	// The watch stream sends the change.
	gotCancel := false
	for !gotCancel && watch.Receive() {
		if r := watch.Msg().GetRequest(); r != nil && r.GetRequestId() == low.GetDevSessionId() && r.GetState() == tidev1.RequestState_REQUEST_STATE_CANCELED {
			gotCancel = true
		}
	}
	if !gotCancel {
		t.Fatalf("watch did not send the cancellation: %v", watch.Err())
	}
	cancel()

	// Pagination with ended requests: open first, then ended.
	all, err := owner.Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true, PageSize: 2}))
	if err != nil || len(all.Msg.GetRequests()) != 2 || all.Msg.GetNextPageToken() == "" {
		t.Fatalf("page 1 %v %v", all, err)
	}
	page2, err := owner.Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true, PageSize: 2, PageToken: all.Msg.GetNextPageToken()}))
	if err != nil || len(page2.Msg.GetRequests()) != 1 || page2.Msg.GetRequests()[0].GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED {
		t.Fatalf("page 2 %v %v", page2, err)
	}

	// Capacity: the device with the tide runner on it.
	capResp, err := w.env.User(w.org.Member).Work.GetCapacity(ctx, connect.NewRequest(&tidev1.GetCapacityRequest{OrganizationId: w.org.ID}))
	if err != nil || len(capResp.Msg.GetDevices()) != 1 {
		t.Fatalf("capacity %v %v", capResp, err)
	}
	d := capResp.Msg.GetDevices()[0]
	if d.GetName() != "linux-box" || d.GetHostOs() != "LINUX" || d.GetArch() != "AMD64" || d.GetEffectiveVcpuLimit() != 16 || d.GetState() != tidev1.DeviceState_DEVICE_STATE_ONLINE ||
		len(d.GetRunners()) != 1 || d.GetRunners()[0].GetState() != tidev1.RunnerState_RUNNER_STATE_BUSY || d.GetRunners()[0].GetCount() != 1 {
		t.Fatalf("device %v", d)
	}

	// DeleteRunner: admins only; the session of the runner ends.
	_, err = w.env.User(w.org.Member).Work.DeleteRunner(ctx, connect.NewRequest(&tidev1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: running.GetRunnerId()}))
	wantCode(t, "member deletes a runner", err, connect.CodePermissionDenied)
	del, err := w.env.User(w.org.Admin).Work.DeleteRunner(ctx, connect.NewRequest(&tidev1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: running.GetRunnerId()}))
	if err != nil || del.Msg.GetRunner().GetState() != tidev1.RunnerState_RUNNER_STATE_DELETED || del.Msg.GetRunner().GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("delete runner %v %v", del, err)
	}
	if s := w.session(t, running.GetDevSessionId()); s.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED || s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("session of the deleted runner %v", s)
	}
	if countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_RUNNER_DELETED) != 1 || countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED) != 1 {
		t.Fatal("audit events")
	}
	// Personal API keys are rejected on WorkService.
	_, key := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	_, err = w.env.Bearer(key).Work.ListRequests(ctx, connect.NewRequest(&tidev1.ListRequestsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "personal API key", err, connect.CodePermissionDenied)
	_, err = w.env.User("outsider").Work.GetRequest(ctx, connect.NewRequest(&tidev1.GetRequestRequest{OrganizationId: w.org.ID, RequestId: high.GetDevSessionId()}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
}

func TestDeleteRunnerUsesLockedState(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := w.env.User(w.org.Admin)
	member := w.env.User(w.org.Member)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	session := w.createSession(t, member, profileID)
	session = w.waitState(t, session.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	runnerID := session.GetRunnerId()
	deleteRequest := &tidev1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: runnerID}
	_, err := member.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
	wantCode(t, "member deletes a runner", err, connect.CodePermissionDenied)
	if got := w.fake.Active(w.osOrg); got != 1 {
		t.Fatalf("active sandboxes before deletion: %d, want 1", got)
	}

	// Keep the runner DELETING long enough to retry the RPC in that state.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("sandbox deletion unavailable"))
	})
	const updateState = "UPDATE runners SET state = $2 WHERE runner_id = $1"
	if _, err := w.env.DB.Pool.Exec(ctx, updateState, runnerID, int16(tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)); err != nil {
		t.Fatal(err)
	}
	commit := holdTx(t, w.env, updateState, runnerID, int16(tidev1.RunnerState_RUNNER_STATE_BUSY))
	type deleteResult struct {
		response *connect.Response[tidev1.DeleteRunnerResponse]
		err      error
	}
	result := make(chan deleteResult, 1)
	go func() {
		response, err := admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
		result <- deleteResult{response, err}
	}()
	waitForLockWaiter(t, w.env)
	commit()
	first := <-result
	if first.err != nil || first.response.Msg.GetRunner().GetState() != tidev1.RunnerState_RUNNER_STATE_DELETING ||
		first.response.Msg.GetRunner().GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("delete after concurrent transition: %v %v", first.response, first.err)
	}
	if s := w.session(t, session.GetDevSessionId()); s.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED ||
		s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("session after runner deletion: %v", s)
	}
	action := tidev1.AuditAction_AUDIT_ACTION_RUNNER_DELETED
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after deletion: %d, want 1", got)
	}
	retry, err := admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
	if err != nil || retry.Msg.GetRunner().GetState() != tidev1.RunnerState_RUNNER_STATE_DELETING {
		t.Fatalf("delete a DELETING runner: %v %v", retry, err)
	}
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after DELETING retry: %d, want 1", got)
	}

	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	eventually(t, "runner deleted", func() bool {
		return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED
	})
	retry, err = admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
	if err != nil || retry.Msg.GetRunner().GetState() != tidev1.RunnerState_RUNNER_STATE_DELETED ||
		retry.Msg.GetRunner().GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("delete a DELETED runner: %v %v", retry, err)
	}
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after DELETED retry: %d, want 1", got)
	}
	if got := w.fake.Active(w.osOrg); got != 0 {
		t.Fatalf("active sandboxes after deletion: %d, want 0", got)
	}
}

// TestCleanUps: deleting a profile cancels its queued sessions; removing
// the OwnSand link deletes the runners and ends the sessions; deleting the
// organization deletes the remaining sandboxes.
func TestCleanUps(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	aID := w.createProfile(t, devProfileProto("a", 0, 0))
	bID := w.createProfile(t, devProfileProto("b", 1, 0))
	running := w.createSession(t, w.env.User(w.org.Member), aID)
	running = w.waitState(t, running.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	eventually(t, "warm runner of b", func() bool { return w.fake.Active(w.osOrg) == 2 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	queuedA := w.createSession(t, w.env.User(w.org.Member), aID)

	// Profile b: its warm runner goes.
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: bID})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "warm runner deleted", func() bool { return w.fake.Active(w.osOrg) == 1 })
	// Profile a: its queued session ends; the running one stays.
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&tidev1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: aID})); err != nil {
		t.Fatal(err)
	}
	if s := w.session(t, queuedA.GetDevSessionId()); s.GetState() != tidev1.RequestState_REQUEST_STATE_CANCELED || s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED {
		t.Fatalf("queued session of a deleted profile %v", s)
	}
	if s := w.session(t, running.GetDevSessionId()); s.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("busy session of a deleted profile %v", s)
	}
	// RemoveOwnSandLink deletes the running session's sandbox.
	if _, err := admin.OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&tidev1.RemoveOwnSandLinkRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if s := w.session(t, running.GetDevSessionId()); !isEnded(s.GetState()) {
		t.Fatalf("session after the link removal %v", s)
	}
	if w.fake.Active(w.osOrg) != 0 {
		t.Fatalf("%d sandboxes after the link removal", w.fake.Active(w.osOrg))
	}

	// Organization deletion deletes the sandboxes first.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	cID := w.createProfile(t, devProfileProto("c", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), cID)
	w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if w.fake.Active(w.osOrg) != 0 {
		t.Fatal("a sandbox survived the organization deletion")
	}
	var rows int
	_ = w.env.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM requests WHERE organization_id = $1) + (SELECT count(*) FROM runners WHERE organization_id = $1)", w.org.ID).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d work rows left", rows)
	}
}

func isEnded(s tidev1.RequestState) bool {
	return s == tidev1.RequestState_REQUEST_STATE_ENDED || s == tidev1.RequestState_REQUEST_STATE_FAILED || s == tidev1.RequestState_REQUEST_STATE_CANCELED
}

// TestOrganizationDeletionFailureKeepsDispatch: when OwnSand cannot delete a
// sandbox, DeleteOrganization fails, the organization stays and its
// dispatcher still starts new work.
func TestOrganizationDeletionFailureKeepsDispatch(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
	})
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err == nil {
		t.Fatal("the organization deletion succeeded without the sandbox deletion")
	}
	var removing bool
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT removing FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&removing); err != nil || removing {
		t.Fatalf("link still marked as being removed: %v %v", removing, err)
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	next := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, next.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
}

// TestConcurrentCancelRequest: parallel cancellations of one queued request
// cancel it once; the others get failed_precondition.
func TestConcurrentCancelRequest(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	owner := w.env.User(w.org.Owner)
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := owner.Work.CancelRequest(ctx, connect.NewRequest(&tidev1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: s.GetDevSessionId()}))
			errs <- err
		}()
	}
	ok := 0
	for range n {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case connect.CodeOf(err) != connect.CodeFailedPrecondition:
			t.Fatalf("cancel: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d successful cancellations", ok)
	}
	if countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED) != 1 {
		t.Fatal("audit events")
	}
}

// TestCreateRetryAfterProfileChange: when the CreateSandbox response is
// lost and an admin then changes the profile, the retry sends the original
// parameters with the key and recovers the created sandbox.
func TestCreateRetryAfterProfileChange(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	if _, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&tidev1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID,
		RunnerProfileId: profileID, Profile: &tidev1.RunnerProfile{Resources: &tidev1.Resources{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"resources"}}})); err != nil {
		t.Fatal(err)
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes) != 1 || w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId() != sandboxes[0].GetSandboxId() || sandboxes[0].GetResources().GetVcpuCount() != 2 {
		t.Fatalf("sandboxes %v", sandboxes)
	}
}

// TestLostCreateResponseAsUnavailable: an unavailable error that OwnSand did
// not send (a transport failure or an intermediary's 503) may hide a
// created sandbox. The runner keeps its key and recovers that sandbox; no
// second sandbox is created.
func TestLostCreateResponseAsUnavailable(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeUnavailable, errors.New("503 Service Unavailable"))
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	stays(t, "one sandbox", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes) != 1 || w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId() != sandboxes[0].GetSandboxId() {
		t.Fatalf("sandboxes %v", sandboxes)
	}
}

// TestDeleteAfterLostCreateResponse: a runner whose CreateSandbox response
// was lost stays DELETING until tide found its sandbox with the stored key,
// and the sandbox is deleted.
func TestDeleteAfterLostCreateResponse(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	list, err := w.env.User(w.org.Owner).Work.ListRunners(ctx, connect.NewRequest(&tidev1.ListRunnersRequest{OrganizationId: w.org.ID}))
	if err != nil || len(list.Msg.GetRunners()) != 1 || list.Msg.GetRunners()[0].GetOwnsandSandboxId() != "" {
		t.Fatalf("runners %v %v", list, err)
	}
	runnerID := list.Msg.GetRunners()[0].GetRunnerId()
	if _, err := w.env.User(w.org.Member).DevSession.DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "runner deleting", func() bool { return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETING })
	stays(t, "runner deleting while the creation is uncertain", func() bool {
		return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETING
	})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	eventually(t, "runner deleted", func() bool { return w.runner(t, runnerID).GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED })
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes) != 1 || sandboxes[0].GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || w.runner(t, runnerID).GetOwnsandSandboxId() != sandboxes[0].GetSandboxId() {
		t.Fatalf("sandboxes %v", sandboxes)
	}
}

// TestStartFailureCleanupRetry: when the sandbox deletion after a start
// failure fails, tide retries it until OwnSand deletes the sandbox.
func TestStartFailureCleanupRetry(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.SetExec(func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		code := int64(1)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, nil)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	s = w.waitState(t, s.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED || w.fake.Active(w.osOrg) != 3 {
		t.Fatalf("session %v with %d active sandboxes", s, w.fake.Active(w.osOrg))
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	eventually(t, "the failed starts' sandboxes deleted and cleanup marks cleared", func() bool {
		var pending int
		if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM runners WHERE organization_id = $1 AND sandbox_cleanup", w.org.ID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		return pending == 0 && w.fake.Active(w.osOrg) == 0
	})
}

// TestStalledCreateSandbox: a CreateSandbox call that OwnSand never
// answers ends at CreateTimeout, so the dispatcher goes on; the runner keeps
// its key and its driver creates the sandbox.
func TestStalledCreateSandbox(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	var stalled sync.Once
	w.fake.SetCreateHook(func(ctx context.Context) error {
		first := false
		stalled.Do(func() { first = true })
		if !first {
			return nil
		}
		<-ctx.Done()
		return connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	})
	a := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "the stalled call", func() bool { return w.fake.Calls("CreateSandbox") >= 1 })
	b := w.createSession(t, w.env.User(w.org.Admin), profileID)
	w.waitState(t, b.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	w.waitState(t, a.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)
	if n := len(w.fake.Sandboxes(w.osOrg)); n != 2 {
		t.Fatalf("%d sandboxes", n)
	}
}

func TestWatchQueueSnapshotReplay(t *testing.T) {
	for _, test := range []struct {
		name      string
		openFirst bool
		openLast  bool
		endedRows int
	}{
		{name: "open oldest", openFirst: true, endedRows: 300},
		{name: "open newest", openLast: true, endedRows: 300},
		{name: "no open rows", endedRows: 300},
		{name: "open oldest with large history", openFirst: true, endedRows: 1500},
		{name: "open newest with large history", openLast: true, endedRows: 1500},
		{name: "no open rows with large history", endedRows: 1500},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			q := w.env.DB.Pool
			other := w.env.NewOrg("other")
			seedWatchRequests(t, q, other.ID, "other", tidev1.RequestState_REQUEST_STATE_RUNNING, 1)

			if test.openFirst {
				seedWatchRequests(t, q, w.org.ID, "open", tidev1.RequestState_REQUEST_STATE_RUNNING, 1)
			}
			seedWatchRequests(t, q, w.org.ID, "historical", tidev1.RequestState_REQUEST_STATE_ENDED, test.endedRows)
			if _, err := q.Exec(t.Context(), `INSERT INTO runners
				(runner_id, organization_id, runner_profile_id, kind, guest_os, state, idempotency_key,
				 create_request, create_time, update_time, end_time)
				SELECT gen_random_uuid(), $1, gen_random_uuid(), $2, $3, $4, gen_random_uuid(), '',
				 now(), now(), now() FROM generate_series(1, 3)`,
				w.org.ID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION),
				int16(tidev1.GuestOs_GUEST_OS_LINUX), int16(tidev1.RunnerState_RUNNER_STATE_DELETED)); err != nil {
				t.Fatal(err)
			}
			if test.openLast {
				seedWatchRequests(t, q, w.org.ID, "open", tidev1.RequestState_REQUEST_STATE_RUNNING, 1)
			}

			var keys []string
			if test.openFirst || test.openLast {
				keys = []string{"open-1"}
			}
			watch, cancel := openQueueWatch(t, w, keys...)
			assertQueueWatchIdle(t, watch, cancel)
		})
	}
}

func TestWatchQueueSnapshotReplayChanges(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	q := w.env.DB.Pool
	seedWatchRequests(t, q, w.org.ID, "open", tidev1.RequestState_REQUEST_STATE_RUNNING, 2)
	watch, cancel := openQueueWatch(t, w, "open-1", "open-2")

	endWatchRequest(t, q, w.org.ID, "open-1")
	expectWatchEnd(t, watch, "open-1")
	seedWatchRequests(t, q, w.org.ID, "new-terminal", tidev1.RequestState_REQUEST_STATE_ENDED, 1)
	expectWatchEnd(t, watch, "new-terminal-1")

	// Advance the change sequence by more than the previous replay window.
	if _, err := q.Exec(t.Context(), "SELECT nextval('work_change_seq') FROM generate_series(1, 1100)"); err != nil {
		t.Fatal(err)
	}
	seedWatchRequests(t, q, w.org.ID, "advance", tidev1.RequestState_REQUEST_STATE_ENDED, 1)
	expectWatchEnd(t, watch, "advance-1")
	endWatchRequest(t, q, w.org.ID, "open-2")
	expectWatchEnd(t, watch, "open-2")
	assertQueueWatchIdle(t, watch, cancel)
}

func TestWatchQueueLateCommit(t *testing.T) {
	for _, test := range []struct {
		name           string
		beforeSnapshot bool
	}{
		{name: "in flight at snapshot", beforeSnapshot: true},
		{name: "in flight during poll"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			q := w.env.DB.Pool
			seedWatchRequests(t, q, w.org.ID, "open", tidev1.RequestState_REQUEST_STATE_RUNNING, 1)
			ctx := t.Context()
			tx, err := q.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if test.beforeSnapshot {
				endWatchRequest(t, tx, w.org.ID, "open-1")
				seedWatchRequests(t, q, w.org.ID, "historical", tidev1.RequestState_REQUEST_STATE_ENDED, 1)
			}
			watch, cancel := openQueueWatch(t, w, "open-1")
			if !test.beforeSnapshot {
				endWatchRequest(t, tx, w.org.ID, "open-1")
				seedWatchRequests(t, q, w.org.ID, "advance", tidev1.RequestState_REQUEST_STATE_ENDED, 1)
				expectWatchEnd(t, watch, "advance-1")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			expectWatchEnd(t, watch, "open-1")
			assertQueueWatchIdle(t, watch, cancel)
		})
	}
}

// Running requests with no runner stay open until the test changes them.
func seedWatchRequests(t *testing.T, q db.Querier, orgID, prefix string, state tidev1.RequestState, count int) {
	t.Helper()
	if _, err := q.Exec(t.Context(), `INSERT INTO requests
		(request_id, organization_id, runner_profile_id, kind, source, source_key, state, priority,
		 queue_time, queue_deadline, create_time, update_time, end_time)
		SELECT gen_random_uuid(), $1, gen_random_uuid(), $2, 99, $3 || '-' || n, $4, $5,
		 now(), now() + interval '1 hour', now(), now(), CASE WHEN $4::smallint = $6 THEN now() END
		FROM generate_series(1, $7) AS n`, orgID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION),
		prefix, int16(state), int16(tidev1.Priority_PRIORITY_NORMAL), int16(tidev1.RequestState_REQUEST_STATE_ENDED), count); err != nil {
		t.Fatal(err)
	}
}

func endWatchRequest(t *testing.T, q db.Querier, orgID, key string) {
	t.Helper()
	if tag, err := q.Exec(t.Context(), `UPDATE requests SET state = $3, end_time = now()
		WHERE organization_id = $1 AND source_key = $2`, orgID, key, int16(tidev1.RequestState_REQUEST_STATE_ENDED)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("end request %s: %v, %v", key, tag, err)
	}
}

func openQueueWatch(t *testing.T, w *workWorld, requestKeys ...string) (*connect.ServerStreamForClient[tidev1.WatchQueueResponse], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(ctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for watch.Receive() {
		msg := watch.Msg()
		if msg.GetSnapshotComplete() != nil {
			if !slices.Equal(keys, requestKeys) {
				t.Fatalf("snapshot keys %v, want %v", keys, requestKeys)
			}
			return watch, cancel
		}
		req := msg.GetRequest()
		if req == nil || req.GetState() != tidev1.RequestState_REQUEST_STATE_RUNNING {
			t.Fatalf("unexpected snapshot event: %v", msg)
		}
		keys = append(keys, req.GetSourceKey())
	}
	t.Fatalf("snapshot did not complete: %v", watch.Err())
	return nil, nil
}

func expectWatchEnd(t *testing.T, watch *connect.ServerStreamForClient[tidev1.WatchQueueResponse], key string) {
	t.Helper()
	if !watch.Receive() {
		t.Fatalf("no terminal event for %s: %v", key, watch.Err())
	}
	got := watch.Msg().GetRequest()
	if got.GetSourceKey() != key || got.GetState() != tidev1.RequestState_REQUEST_STATE_ENDED {
		t.Fatalf("want terminal event for %s, got %v", key, watch.Msg())
	}
}

func assertQueueWatchIdle(t *testing.T, watch *connect.ServerStreamForClient[tidev1.WatchQueueResponse], cancel context.CancelFunc) {
	t.Helper()
	// Read across three default poll intervals to catch delayed replays.
	timer := time.AfterFunc(3200*time.Millisecond, cancel)
	defer timer.Stop()
	if watch.Receive() {
		t.Fatalf("unexpected event after snapshot_complete: %v", watch.Msg())
	}
	wantCode(t, "idle watch at cancellation", watch.Err(), connect.CodeCanceled)
}

// TestWatchQueueRemovedRunner: a CREATING runner that the dispatcher removes
// after OwnSand had no capacity is sent once more as DELETED, so a watch
// does not keep it.
func TestWatchQueueRemovedRunner(t *testing.T) {
	w := newWorkWorld(t, devsession.Timing{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	// OwnSand answers only after the watch sent the runner (not at
	// CreateTimeout, which could come before the watch reads the change).
	release := make(chan struct{})
	w.fake.SetCreateHook(func(context.Context) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return connect.NewWireError(connect.CodeUnavailable, errors.New("no device has free capacity"))
	})
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(ctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	for watch.Receive() && watch.Msg().GetSnapshotComplete() == nil {
	}
	w.createSession(t, w.env.User(w.org.Member), profileID)
	runnerID := ""
	for runnerID == "" && watch.Receive() {
		if r := watch.Msg().GetRunner(); r != nil && r.GetState() == tidev1.RunnerState_RUNNER_STATE_CREATING {
			runnerID = r.GetRunnerId()
		}
	}
	if runnerID == "" {
		t.Fatalf("no CREATING runner: %v", watch.Err())
	}
	close(release)
	for watch.Receive() {
		if r := watch.Msg().GetRunner(); r != nil && r.GetRunnerId() == runnerID && r.GetState() == tidev1.RunnerState_RUNNER_STATE_DELETED {
			if r.GetEndReason() != tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
				t.Fatalf("removed runner end reason = %v, want CLEAN_UP", r.GetEndReason())
			}
			return
		}
	}
	t.Fatalf("the removed runner was not sent as DELETED: %v", watch.Err())
}

const (
	watchRequestOpen = tidev1.RequestState_REQUEST_STATE_RUNNING
	watchRequestEnd  = tidev1.RequestState_REQUEST_STATE_CANCELED
	watchRunnerOpen  = tidev1.RunnerState_RUNNER_STATE_BUSY
	watchRunnerEnd   = tidev1.RunnerState_RUNNER_STATE_DELETED
)

func TestWatchQueueCommitOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runner   bool
		rollBack bool
	}{
		{name: "late request"},
		{name: "late runner", runner: true},
		{name: "rolled back request", rollBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.New(t, testenv.WithLayers(ownsandfake.New(), &layers.Layers{}))
			org, otherOrg := env.NewOrg("alice"), env.NewOrg("bob")
			target := seedQueueWatchRow(t, env, org.ID, tc.runner)
			later := seedQueueWatchRow(t, env, org.ID, false)
			foreign := seedQueueWatchRow(t, env, otherOrg.ID, tc.runner)

			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			watch := startQueueWatch(t, ctx, env, org)
			expectQueueWatchState(t, nextQueueWatchEvent(t, watch), target, watchRequestOpen, watchRunnerOpen)
			expectQueueWatchState(t, nextQueueWatchEvent(t, watch), later, watchRequestOpen, watchRunnerOpen)
			if msg := nextQueueWatchEvent(t, watch); msg.GetSnapshotComplete() == nil {
				t.Fatalf("expected snapshot_complete, got %v", msg)
			}

			tx, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			// The trigger must record the top-level ID even inside a savepoint.
			savepoint, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			earlySeq := endQueueWatchRow(t, ctx, savepoint, target, tc.runner)
			if err := savepoint.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			other, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Rollback(t.Context())
			if _, err := other.Exec(ctx, "SELECT nextval('work_change_seq') FROM generate_series(1, 1500)"); err != nil {
				t.Fatal(err)
			}
			laterSeq := endQueueWatchRow(t, ctx, other, later, false)
			endQueueWatchRow(t, ctx, other, foreign, tc.runner)
			if err := other.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if laterSeq-earlySeq < 1000 {
				t.Fatalf("sequence gap = %d, want at least 1000", laterSeq-earlySeq)
			}
			expectQueueWatchState(t, nextQueueWatchEvent(t, watch), later, watchRequestEnd, watchRunnerEnd)

			if tc.rollBack {
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				expectQueueWatchState(t, nextQueueWatchEvent(t, watch), target, watchRequestEnd, watchRunnerEnd)
			}
			// A new commit must still arrive after the late commit or rollback.
			next := seedQueueWatchRow(t, env, org.ID, true)
			expectQueueWatchState(t, nextQueueWatchEvent(t, watch), next, watchRequestOpen, watchRunnerOpen)

			received := make(chan bool, 1)
			go func() { received <- watch.Receive() }()
			select {
			case hasEvent := <-received:
				if hasEvent {
					t.Fatalf("idle watch emitted an unexpected event: %v", watch.Msg())
				}
				t.Fatalf("watch ended during idle polls: %v", watch.Err())
			case <-time.After(2200 * time.Millisecond):
				cancel()
				if <-received {
					t.Fatalf("idle watch emitted an unexpected event: %v", watch.Msg())
				}
			}
		})
	}
}

func TestWatchQueueSnapshotAcrossTables(t *testing.T) {
	env := testenv.New(t, testenv.WithLayers(ownsandfake.New(), &layers.Layers{}))
	org := env.NewOrg("alice")
	requestID := seedQueueWatchRow(t, env, org.ID, false)
	runnerID := seedQueueWatchRow(t, env, org.ID, true)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writer, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(t.Context())
	if _, err := writer.Exec(ctx, "LOCK TABLE runners IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	type watchResult struct {
		stream *connect.ServerStreamForClient[tidev1.WatchQueueResponse]
		err    error
	}
	started := make(chan watchResult, 1)
	go func() {
		stream, err := env.User(org.Member).Work.WatchQueue(ctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: org.ID}))
		started <- watchResult{stream, err}
	}()

	// Commit only after the watch read requests and is blocked reading runners.
	eventually(t, "watch reading the runner snapshot", func() bool {
		var waiting bool
		err := env.DB.Pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND query LIKE '% FROM runners WHERE organization_id = $1 AND state = ANY($2)%'
		)`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		return waiting
	})
	endQueueWatchRow(t, ctx, writer, requestID, false)
	endQueueWatchRow(t, ctx, writer, runnerID, true)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	watch := <-started
	if watch.err != nil {
		t.Fatal(watch.err)
	}
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), requestID, watchRequestOpen, watchRunnerOpen)
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), runnerID, watchRequestOpen, watchRunnerOpen)
	if msg := nextQueueWatchEvent(t, watch.stream); msg.GetSnapshotComplete() == nil {
		t.Fatalf("expected snapshot_complete, got %v", msg)
	}
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), requestID, watchRequestEnd, watchRunnerEnd)
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), runnerID, watchRequestEnd, watchRunnerEnd)
}

func seedQueueWatchRow(t *testing.T, env *testenv.Env, orgID string, runner bool) string {
	t.Helper()
	kind := int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION)
	var row pgx.Row
	if runner {
		row = env.DB.Pool.QueryRow(t.Context(), `INSERT INTO runners (runner_id, organization_id, runner_profile_id,
			kind, guest_os, state, idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, $3, $4,
				gen_random_uuid(), ''::bytea, gen_random_uuid(), now(), now()) RETURNING runner_id::text`,
			orgID, kind, int16(tidev1.GuestOs_GUEST_OS_LINUX), int16(watchRunnerOpen))
	} else {
		row = env.DB.Pool.QueryRow(t.Context(), `INSERT INTO requests (request_id, organization_id, runner_profile_id,
			kind, source, source_key, state, priority, queue_time, queue_deadline, create_time, update_time)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, $3, gen_random_uuid()::text,
				$4, $5, now(), now() + interval '1 hour', now(), now()) RETURNING request_id::text`,
			orgID, kind, int16(work.SourceDevSession), int16(watchRequestOpen), int16(tidev1.Priority_PRIORITY_NORMAL))
	}
	var id string
	if err := row.Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func endQueueWatchRow(t *testing.T, ctx context.Context, tx pgx.Tx, id string, runner bool) int64 {
	t.Helper()
	sql := "UPDATE requests SET state = $2, end_time = now() WHERE request_id = $1 RETURNING change_seq"
	state := int16(watchRequestEnd)
	if runner {
		sql = "UPDATE runners SET state = $2, end_time = now() WHERE runner_id = $1 RETURNING change_seq"
		state = int16(watchRunnerEnd)
	}
	var seq int64
	if err := tx.QueryRow(ctx, sql, id, state).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func startQueueWatch(t *testing.T, ctx context.Context, env *testenv.Env, org testenv.Org) *connect.ServerStreamForClient[tidev1.WatchQueueResponse] {
	t.Helper()
	watch, err := env.User(org.Member).Work.WatchQueue(ctx, connect.NewRequest(&tidev1.WatchQueueRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

func nextQueueWatchEvent(t *testing.T, watch *connect.ServerStreamForClient[tidev1.WatchQueueResponse]) *tidev1.WatchQueueResponse {
	t.Helper()
	if !watch.Receive() {
		t.Fatalf("watch ended before the expected event: %v", watch.Err())
	}
	return watch.Msg()
}

func expectQueueWatchState(t *testing.T, event *tidev1.WatchQueueResponse, id string, requestState tidev1.RequestState, runnerState tidev1.RunnerState) {
	t.Helper()
	if r := event.GetRequest(); r != nil && r.GetRequestId() == id && r.GetState() == requestState {
		return
	}
	if r := event.GetRunner(); r != nil && r.GetRunnerId() == id && r.GetState() == runnerState {
		return
	}
	t.Fatalf("expected state %v or %v for %s, got %v", requestState, runnerState, id, event)
}
