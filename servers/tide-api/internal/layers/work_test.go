// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/devsession"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
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
		States: []tidev1.RequestState{tidev1.RequestState_REQUEST_STATE_QUEUED}}))
	if err != nil || len(list.Msg.GetRequests()) != 2 || list.Msg.GetRequests()[0].GetRequestId() != high.GetDevSessionId() {
		t.Fatalf("queue %v %v", list, err)
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
