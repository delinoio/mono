// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/profile"
	"github.com/delinoio/mono/servers/sandrun-api/internal/secrets"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

// TestMembershipEndWithSessionsAndKeys: removing a member revokes the keys,
// deletes the running session's sandbox (ENDED, MEMBER_REMOVED) and cancels
// the queued one (CANCELED, MEMBER_REMOVED).
func TestMembershipEndWithSessionsAndKeys(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 1 })
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	_, key := w.env.NewAPIKey(w.org.ID, w.org.Member)
	member := w.env.Bearer(key)
	running := w.createSession(t, member, profileID)
	running = w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	queued := w.createSession(t, member, profileID)
	sandboxID := w.runner(t, running.GetRunnerId()).GetOwnsandSandboxId()

	if _, err := w.env.User(w.org.Admin).Member.RemoveMember(ctx, connect.NewRequest(&sandrunv1.RemoveMemberRequest{OrganizationId: w.org.ID, UserId: w.org.Member})); err != nil {
		t.Fatal(err)
	}
	r := w.session(t, running.GetDevSessionId())
	q := w.session(t, queued.GetDevSessionId())
	if r.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED || r.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED ||
		q.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED || q.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED {
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
		return w.runner(t, running.GetRunnerId()).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
	})
	_, err := member.DevSession.ListDevSessions(ctx, connect.NewRequest(&sandrunv1.ListDevSessionsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "revoked key", err, connect.CodeUnauthenticated)
}

// TestWarmRunnersAndReclaim: a warm runner serves a session without a new
// sandbox and is replaced; when OwnSand has no capacity, the dispatcher
// deletes the warm runner of the lowest priority profile.
func TestWarmRunnersAndReclaim(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 2 })
	warmID := w.createProfile(t, devProfileProto("warm", 1, sandrunv1.Priority_PRIORITY_LOW))
	countWarm := func() int {
		resp, err := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&sandrunv1.ListRunnersRequest{OrganizationId: w.org.ID,
			States: []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WARM}}))
		if err != nil {
			t.Fatal(err)
		}
		return len(resp.Msg.GetRunners())
	}
	eventually(t, "a warm runner", func() bool { return countWarm() == 1 })
	stays(t, "one warm runner", func() bool { return countWarm() == 1 && w.fake.Active(w.osOrg) == 1 })
	// The session takes the warm runner; a new warm runner replaces it.
	s := w.createSession(t, w.env.User(w.org.Member), warmID)
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	eventually(t, "a new warm runner", func() bool { return countWarm() == 1 && w.fake.Active(w.osOrg) == 2 })

	// The pool is full (busy session and warm runner). A HIGH priority
	// session reclaims the LOW priority warm runner and starts.
	highID := w.createProfile(t, devProfileProto("high", 0, sandrunv1.Priority_PRIORITY_HIGH))
	h := w.createSession(t, w.env.User(w.org.Admin), highID)
	h = w.waitState(t, h.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	var reclaimed int
	resp, _ := w.env.User(w.org.Owner).Work.ListRunners(context.Background(), connect.NewRequest(&sandrunv1.ListRunnersRequest{OrganizationId: w.org.ID, IncludeEnded: true}))
	for _, r := range resp.Msg.GetRunners() {
		if r.GetEndReason() == sandrunv1.RunnerEndReason_RUNNER_END_REASON_WARM_RECLAIMED {
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

func TestDispatchDemandAfterOwnSandLinkReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   sandrunv1.WorkloadKind
		source work.Source
		pool   string
	}{
		{"binding demand", sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, work.SourceDevSession, ""},
		{"pool demand", sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, work.SourceGitHub, "123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			fake := ownsandfake.New()
			factory := &dispatchLinkFactory{Fake: fake}
			w := newWorkWorldWithProvider(t, devsession.Timing{}, fake, factory)
			p := devProfileProto("demand", 0, 0)
			p.WorkloadKind = tc.kind
			if tc.pool != "" {
				p.WorkspacePath = ""
			}
			profileID := w.createProfile(t, p)

			// Stop the background dispatcher before enqueuing demand so each
			// explicit pass can be checked before the next pass starts.
			w.env.Stop()
			database := w.env.DB
			replacement, key := api.NewID(), ownsandKey('b')
			fake.AddOrg(replacement, key)
			ring, err := secrets.NewKeyring(w.env.SecretKey)
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := ring.Seal([]byte(key), secrets.AssociatedData("ownsand_link", w.org.ID))
			if err != nil {
				t.Fatal(err)
			}
			// Ensure the pass has to build a client for its snapshot.
			if _, err := database.Pool.Exec(ctx, "UPDATE ownsand_links SET credential_revision = $2 WHERE organization_id = $1", w.org.ID, api.NewID()); err != nil {
				t.Fatal(err)
			}
			factory.afterNew = func() error {
				// Install the replacement after the pass reads the old link,
				// while keeping the request queued for the reservation check.
				_, err := database.Pool.Exec(ctx, `UPDATE ownsand_links SET ownsand_organization_id = $2, credential_revision = $3, api_key_ciphertext = $4
					WHERE organization_id = $1`, w.org.ID, replacement, api.NewID(), sealed.Ciphertext)
				return err
			}

			storedProfile, err := w.l.Profiles.Get(ctx, database.Pool, w.org.ID, profileID)
			if err != nil {
				t.Fatal(err)
			}
			requestID := api.NewID()
			if err := database.InTx(ctx, func(tx pgx.Tx) error {
				_, _, err := w.l.Engine.CreateRequest(ctx, tx, work.NewRequest{
					Profile: storedProfile, Source: tc.source, PoolKey: tc.pool, RequestID: requestID, OwnerUserID: w.org.Owner,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			w.l.Engine.Pass(ctx, w.org.ID)
			if factory.afterNew != nil {
				t.Fatal("dispatcher did not load the old link client")
			}
			var runnerCount int
			if err := database.Pool.QueryRow(ctx, "SELECT count(*) FROM runners WHERE organization_id = $1", w.org.ID).Scan(&runnerCount); err != nil {
				t.Fatal(err)
			}
			if calls := fake.Calls("CreateSandbox"); runnerCount != 0 || calls != 0 {
				t.Fatalf("stale link: %d runner rows, %d create calls", runnerCount, calls)
			}
			req, err := w.l.Engine.GetRequest(ctx, database.Pool, w.org.ID, requestID)
			if err != nil {
				t.Fatal(err)
			}
			if req.State != sandrunv1.RequestState_REQUEST_STATE_QUEUED || req.RunnerID != "" || req.StartAttempts != 0 {
				t.Fatalf("stale link: state %s, runner %q, attempts %d", req.State, req.RunnerID, req.StartAttempts)
			}

			w.l.Engine.Pass(ctx, w.org.ID)
			if calls, current := fake.Calls("CreateSandbox"), fake.Active(replacement); calls != 1 || current != 1 {
				t.Fatalf("current link: %d total create calls, %d active sandboxes at replacement", calls, current)
			}
			var bound, sponsor, pool string
			if err := database.Pool.QueryRow(ctx, `SELECT coalesce(request_id::text, ''), coalesce(sponsor_request_id::text, ''), coalesce(pool_key, '')
				FROM runners WHERE organization_id = $1`, w.org.ID).Scan(&bound, &sponsor, &pool); err != nil {
				t.Fatal(err)
			}
			if tc.pool == "" {
				if bound != requestID || sponsor != "" || pool != "" {
					t.Fatalf("binding reservation: bound %q, sponsor %q, pool %q", bound, sponsor, pool)
				}
			} else if bound != "" || sponsor != requestID || pool != tc.pool {
				t.Fatalf("pool reservation: bound %q, sponsor %q, pool %q", bound, sponsor, pool)
			}
		})
	}
}

type dispatchLinkFactory struct {
	*ownsandfake.Fake
	afterNew func() error
}

func (f *dispatchLinkFactory) New(orgID, key string) (ownsandclient.Client, error) {
	client, err := f.Fake.New(orgID, key)
	if err != nil || f.afterNew == nil {
		return client, err
	}
	afterNew := f.afterNew
	f.afterNew = nil
	return client, afterNew()
}

func TestWarmReservationDuringProfileDeletion(t *testing.T) {
	t.Parallel()
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
		if _, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{
			OrganizationId: w.org.ID, RunnerProfileId: deletedID,
		})); err != nil {
			t.Fatal(err)
		}
		w.fake.SetCreateHook(nil)
		// Let the captured call proceed and observe the surviving profile.
		release <- struct{}{}
		eventually(t, "surviving profile warms", func() bool {
			return profileRunnerCount(t, w, liveID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
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
			_, err := w.env.User(w.org.Admin).Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{
				OrganizationId: w.org.ID, RunnerProfileId: profileID,
			}))
			done <- err
		}()
		select {
		case <-prepared:
		case <-ctx.Done():
			t.Fatal("deletion did not complete the initial cleanup")
		}
		// OwnSand commits the sandbox but SandRun has no sandbox ID until it
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
		if r := w.runner(t, runnerID); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETING ||
			r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
			t.Fatalf("late reservation after profile deletion: %v", r)
		}
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
		eventually(t, "late runner and its sandbox are deleted", func() bool {
			return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED && w.fake.Active(w.osOrg) == 0
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
	t.Parallel()
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
					profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_FAILED) == want
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
				session = w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_FAILED)
				req, err := w.env.User(w.org.Member).Work.GetRequest(ctx, connect.NewRequest(&sandrunv1.GetRequestRequest{
					OrganizationId: w.org.ID, RequestId: session.GetDevSessionId(),
				}))
				if err != nil {
					t.Fatal(err)
				}
				if session.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED ||
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
				return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
			})
			if got := w.fake.Calls("CreateSandbox"); got != before+1 {
				t.Fatalf("CreateSandbox calls = %d after recovery, want %d", got, before+1)
			}
		})
	}
}

func TestWarmPreparationFailureBacksOffAndReadyRunnerClearsRetry(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		exitCode := int64(1)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &exitCode}, nil
	}, nil)
	profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
	eventually(t, "a failed preparation and sandbox deletion", func() bool {
		return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_FAILED) == 1 && w.fake.Active(w.osOrg) == 0
	})
	w.fake.SetExec(nil, nil)
	w.l.Engine.Kick(w.org.ID)
	stays(t, "no retry during preparation cooldown", func() bool { return w.fake.Calls("CreateSandbox") == 1 })
	w.env.Clock.Advance(testenv.FastWorkTiming().PassInterval)
	eventually(t, "a warm runner after preparation recovers", func() bool {
		return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
	})
	var failures int
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT warm_failures FROM runner_profiles WHERE runner_profile_id = $1", profileID).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("warm failures = %d, want 0 (%v)", failures, err)
	}
}

func TestWarmInFlightRejectionDoesNotBackOffNewProfileRevision(t *testing.T) {
	t.Parallel()
	const observationTimeout = 5 * time.Second
	timing := testenv.FastWorkTiming()
	timing.CreateTimeout = 15 * time.Second
	renamed := make(chan struct{})
	decision := make(chan bool, 1)
	var runnerID string
	fake := ownsandfake.New()
	w := newWorkWorldWithOptions(t, fake, layers.Options{
		OwnSand:    fake,
		WorkTiming: &timing,
		ReconcileDriverObserver: func(id string, registered bool) {
			select {
			case <-renamed:
			default:
				return
			}
			if id == runnerID {
				select {
				case decision <- registered:
				default:
				}
			}
		},
	})

	started := make(chan *ownsandv1.CreateSandboxRequest, 1)
	finished := make(chan error, 1)
	duplicate := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseCreate := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseCreate)
	rejection := connect.NewWireError(connect.CodeNotFound, errors.New("old profile rejected"))
	var first sync.Once
	var firstKey string
	fake.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
		key := req.GetIdempotencyKey()
		initial := false
		first.Do(func() {
			initial = true
			firstKey = key
		})
		if !initial && key != firstKey {
			return nil
		}
		if initial {
			started <- req
		} else {
			select {
			case duplicate <- struct{}{}:
			default:
			}
		}

		// Hold any replay as well, so it cannot change the row before the
		// test observes the reconciler's decision.
		select {
		case <-release:
		case <-ctx.Done():
		}
		err := ctx.Err()
		if err == nil {
			err = rejection
		}
		if initial {
			finished <- err
		}
		return err
	})
	profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
	var firstRequest *ownsandv1.CreateSandboxRequest
	select {
	case firstRequest = <-started:
	case <-time.After(observationTimeout):
		t.Fatal("warm create did not start")
	}
	runnerID = firstRequest.GetLabels()[ownsandclient.LabelRunnerID]
	key := firstRequest.GetIdempotencyKey()
	if runnerID == "" || key == "" {
		t.Fatal("first warm create has no runner ID or idempotency key")
	}
	var storedKey string
	var state sandrunv1.RunnerState
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT idempotency_key::text, state FROM runners WHERE runner_id = $1", runnerID).Scan(&storedKey, &state); err != nil {
		t.Fatalf("load the in-flight runner: %v", err)
	}
	if storedKey != key || state != sandrunv1.RunnerState_RUNNER_STATE_CREATING {
		t.Fatalf("in-flight runner has state %v and stored key %q, want CREATING and %q", state, storedKey, key)
	}

	renameWarmProfile(t, w, profileID)
	close(renamed)
	select {
	case registered := <-decision:
		if registered {
			t.Fatal("reconciler registered a driver for the in-flight warm runner")
		}
	case err := <-finished:
		t.Fatalf("first warm create exited before release: %v", err)
	case <-time.After(observationTimeout):
		t.Fatal("reconciler did not decide whether to register a driver for the in-flight warm runner")
	}
	releaseCreate()
	select {
	case err := <-finished:
		if err != rejection {
			t.Fatalf("first warm create exited before release: %v", err)
		}
	case <-time.After(observationTimeout):
		t.Fatal("first warm create did not exit after release")
	}
	eventually(t, "warm runner under the new profile revision without advancing the clock", func() bool {
		return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
	})
	select {
	case <-duplicate:
		t.Fatal("reconciler replayed the in-flight warm create's idempotency key")
	default:
	}
}

func renameWarmProfile(t *testing.T, w *workWorld, profileID string) {
	t.Helper()
	_, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(context.Background(), connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
		OrganizationId:  w.org.ID,
		RunnerProfileId: profileID,
		Profile:         &sandrunv1.RunnerProfile{Name: "warm-updated"},
		UpdateMask:      &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func profileRunnerCount(t *testing.T, w *workWorld, profileID string, state sandrunv1.RunnerState) int {
	t.Helper()
	var n int
	if err := w.env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM runners WHERE runner_profile_id = $1 AND state = $2", profileID, int16(state)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGetCapacityAllocatedVCPU(t *testing.T) {
	t.Parallel()
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
			resp, err := work.GetCapacity(t.Context(), connect.NewRequest(&sandrunv1.GetCapacityRequest{OrganizationId: w.org.ID}))
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
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	w.fake.SetDevices(w.osOrg, &ownsandv1.Device{DeviceId: "device-0", Name: "linux-box", State: ownsandv1.DeviceState_DEVICE_STATE_ONLINE,
		HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, EffectiveVcpuLimitCount: 16,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 500, VcpuOvercommitPercent: 100}})
	lowID := w.createProfile(t, devProfileProto("low", 0, sandrunv1.Priority_PRIORITY_LOW))
	highID := w.createProfile(t, devProfileProto("high", 0, sandrunv1.Priority_PRIORITY_HIGH))
	running := w.createSession(t, w.env.User(w.org.Member), lowID)
	running = w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	low := w.createSession(t, w.env.User(w.org.Member), lowID)
	w.env.Clock.Advance(time.Second)
	high := w.createSession(t, w.env.User(w.org.Admin), highID)
	owner := w.env.User(w.org.Owner)

	// Watch: the snapshot has every open request and runner.
	wctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(wctx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]bool{}
	for {
		if !watch.Receive() {
			if err := watch.Err(); err != nil {
				t.Fatalf("watch ended before snapshot_complete: %v", err)
			}
			t.Fatal("watch ended before snapshot_complete")
		}
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
	list, err := w.env.User(w.org.Member).Work.ListRequests(ctx, connect.NewRequest(&sandrunv1.ListRequestsRequest{OrganizationId: w.org.ID,
		Kind: sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, States: []sandrunv1.RequestState{sandrunv1.RequestState_REQUEST_STATE_QUEUED}}))
	if err != nil || len(list.Msg.GetRequests()) != 2 || list.Msg.GetRequests()[0].GetRequestId() != high.GetDevSessionId() {
		t.Fatalf("queue %v %v", list, err)
	}
	runners, err := owner.Work.ListRunners(ctx, connect.NewRequest(&sandrunv1.ListRunnersRequest{OrganizationId: w.org.ID,
		States: []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK, sandrunv1.RunnerState_RUNNER_STATE_BUSY}}))
	if err != nil || len(runners.Msg.GetRunners()) != 1 || runners.Msg.GetRunners()[0].GetRunnerId() != running.GetRunnerId() {
		t.Fatalf("active runners %v %v", runners, err)
	}
	// Cancel: admins only, QUEUED only.
	_, err = w.env.User(w.org.Member).Work.CancelRequest(ctx, connect.NewRequest(&sandrunv1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: low.GetDevSessionId()}))
	wantCode(t, "member cancels", err, connect.CodePermissionDenied)
	_, err = owner.Work.CancelRequest(ctx, connect.NewRequest(&sandrunv1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: running.GetDevSessionId()}))
	wantCode(t, "cancel a running request", err, connect.CodeFailedPrecondition)
	c, err := owner.Work.CancelRequest(ctx, connect.NewRequest(&sandrunv1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: low.GetDevSessionId()}))
	if err != nil || c.Msg.GetRequest().GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED || c.Msg.GetRequest().GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER {
		t.Fatalf("cancel %v %v", c, err)
	}
	// The watch stream sends the change.
	gotCancel := false
	for !gotCancel && watch.Receive() {
		if r := watch.Msg().GetRequest(); r != nil && r.GetRequestId() == low.GetDevSessionId() && r.GetState() == sandrunv1.RequestState_REQUEST_STATE_CANCELED {
			gotCancel = true
		}
	}
	if !gotCancel {
		t.Fatalf("watch did not send the cancellation: %v", watch.Err())
	}
	cancel()

	// Pagination with ended requests: open first, then ended.
	all, err := owner.Work.ListRequests(ctx, connect.NewRequest(&sandrunv1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true, PageSize: 2}))
	if err != nil || len(all.Msg.GetRequests()) != 2 || all.Msg.GetNextPageToken() == "" {
		t.Fatalf("page 1 %v %v", all, err)
	}
	page2, err := owner.Work.ListRequests(ctx, connect.NewRequest(&sandrunv1.ListRequestsRequest{OrganizationId: w.org.ID, IncludeEnded: true, PageSize: 2, PageToken: all.Msg.GetNextPageToken()}))
	if err != nil || len(page2.Msg.GetRequests()) != 1 || page2.Msg.GetRequests()[0].GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED {
		t.Fatalf("page 2 %v %v", page2, err)
	}

	// Capacity: the device with the SandRun runner on it.
	capResp, err := w.env.User(w.org.Member).Work.GetCapacity(ctx, connect.NewRequest(&sandrunv1.GetCapacityRequest{OrganizationId: w.org.ID}))
	if err != nil || len(capResp.Msg.GetDevices()) != 1 {
		t.Fatalf("capacity %v %v", capResp, err)
	}
	d := capResp.Msg.GetDevices()[0]
	if d.GetName() != "linux-box" || d.GetHostOs() != "LINUX" || d.GetArch() != "AMD64" || d.GetEffectiveVcpuLimit() != 16 || d.GetState() != sandrunv1.DeviceState_DEVICE_STATE_ONLINE ||
		len(d.GetRunners()) != 1 || d.GetRunners()[0].GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY || d.GetRunners()[0].GetCount() != 1 {
		t.Fatalf("device %v", d)
	}

	// DeleteRunner: admins only; the session of the runner ends.
	_, err = w.env.User(w.org.Member).Work.DeleteRunner(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: running.GetRunnerId()}))
	wantCode(t, "member deletes a runner", err, connect.CodePermissionDenied)
	del, err := w.env.User(w.org.Admin).Work.DeleteRunner(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: running.GetRunnerId()}))
	if err != nil || del.Msg.GetRunner().GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED || del.Msg.GetRunner().GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("delete runner %v %v", del, err)
	}
	if s := w.session(t, running.GetDevSessionId()); s.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED || s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("session of the deleted runner %v", s)
	}
	if countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_DELETED) != 1 || countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED) != 1 {
		t.Fatal("audit events")
	}
	// Personal API keys are rejected on WorkService.
	_, key := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	_, err = w.env.Bearer(key).Work.ListRequests(ctx, connect.NewRequest(&sandrunv1.ListRequestsRequest{OrganizationId: w.org.ID}))
	wantCode(t, "personal API key", err, connect.CodePermissionDenied)
	_, err = w.env.User("outsider").Work.GetRequest(ctx, connect.NewRequest(&sandrunv1.GetRequestRequest{OrganizationId: w.org.ID, RequestId: high.GetDevSessionId()}))
	wantCode(t, "outsider", err, connect.CodeNotFound)
}

func TestDeleteRunnerUsesLockedState(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := w.env.User(w.org.Admin)
	member := w.env.User(w.org.Member)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	session := w.createSession(t, member, profileID)
	session = w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	runnerID := session.GetRunnerId()
	deleteRequest := &sandrunv1.DeleteRunnerRequest{OrganizationId: w.org.ID, RunnerId: runnerID}
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
	if _, err := w.env.DB.Pool.Exec(ctx, updateState, runnerID, int16(sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)); err != nil {
		t.Fatal(err)
	}
	holder := holdTx(t, w.env, updateState, runnerID, int16(sandrunv1.RunnerState_RUNNER_STATE_BUSY))
	type deleteResult struct {
		response *connect.Response[sandrunv1.DeleteRunnerResponse]
		err      error
	}
	result := make(chan deleteResult, 1)
	go func() {
		response, err := admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
		result <- deleteResult{response, err}
	}()
	waitForLockWaiter(t, w.env, lockWait{blockerPID: holder.pid,
		query: "FROM runners WHERE organization_id = $1 AND runner_id = $2 FOR UPDATE"})
	holder.commit()
	first := <-result
	if first.err != nil || first.response.Msg.GetRunner().GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETING ||
		first.response.Msg.GetRunner().GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
		t.Fatalf("delete after concurrent transition: %v %v", first.response, first.err)
	}
	if s := w.session(t, session.GetDevSessionId()); s.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("session after runner deletion: %v", s)
	}
	action := sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_DELETED
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after deletion: %d, want 1", got)
	}
	retry, err := admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
	if err != nil || retry.Msg.GetRunner().GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETING {
		t.Fatalf("delete a DELETING runner: %v %v", retry, err)
	}
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after DELETING retry: %d, want 1", got)
	}

	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	eventually(t, "runner deleted", func() bool {
		return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
	})
	retry, err = admin.Work.DeleteRunner(ctx, connect.NewRequest(deleteRequest))
	if err != nil || retry.Msg.GetRunner().GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED ||
		retry.Msg.GetRunner().GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
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
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	admin := w.env.User(w.org.Admin)
	aID := w.createProfile(t, devProfileProto("a", 0, 0))
	bID := w.createProfile(t, devProfileProto("b", 1, 0))
	running := w.createSession(t, w.env.User(w.org.Member), aID)
	running = w.waitState(t, running.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	eventually(t, "warm runner of b", func() bool { return w.fake.Active(w.osOrg) == 2 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("full"))
	})
	queuedA := w.createSession(t, w.env.User(w.org.Member), aID)

	// Profile b: its warm runner goes.
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: bID})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "warm runner deleted", func() bool { return w.fake.Active(w.osOrg) == 1 })
	// Profile a: its queued session ends; the running one stays.
	if _, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{OrganizationId: w.org.ID, RunnerProfileId: aID})); err != nil {
		t.Fatal(err)
	}
	if s := w.session(t, queuedA.GetDevSessionId()); s.GetState() != sandrunv1.RequestState_REQUEST_STATE_CANCELED || s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_PROFILE_DELETED {
		t.Fatalf("queued session of a deleted profile %v", s)
	}
	if s := w.session(t, running.GetDevSessionId()); s.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
		t.Fatalf("busy session of a deleted profile %v", s)
	}
	// RemoveOwnSandLink deletes the running session's sandbox.
	if _, err := admin.OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&sandrunv1.RemoveOwnSandLinkRequest{OrganizationId: w.org.ID})); err != nil {
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
	w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	if w.fake.Active(w.osOrg) != 0 {
		t.Fatal("a sandbox survived the organization deletion")
	}
	var rows int
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM requests WHERE organization_id = $1) + (SELECT count(*) FROM runners WHERE organization_id = $1)", w.org.ID).Scan(&rows); err != nil {
		t.Fatalf("count work rows after organization deletion: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d work rows left", rows)
	}
}

func isEnded(s sandrunv1.RequestState) bool {
	return s == sandrunv1.RequestState_REQUEST_STATE_ENDED || s == sandrunv1.RequestState_REQUEST_STATE_FAILED || s == sandrunv1.RequestState_REQUEST_STATE_CANCELED
}

// TestOrganizationDeletionFailureKeepsDispatch: when OwnSand cannot delete a
// sandbox, DeleteOrganization fails, the organization stays and its
// dispatcher still starts new work.
func TestOrganizationDeletionFailureKeepsDispatch(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
	})
	if _, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: w.org.ID})); err == nil {
		t.Fatal("the organization deletion succeeded without the sandbox deletion")
	}
	var removing bool
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT removing FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&removing); err != nil || removing {
		t.Fatalf("link still marked as being removed: %v %v", removing, err)
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })
	next := w.createSession(t, w.env.User(w.org.Member), profileID)
	w.waitState(t, next.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
}

// TestConcurrentCancelRequest: parallel cancellations of one queued request
// cancel it once; the others get failed_precondition.
func TestConcurrentCancelRequest(t *testing.T) {
	t.Parallel()
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
			_, err := owner.Work.CancelRequest(ctx, connect.NewRequest(&sandrunv1.CancelRequestRequest{OrganizationId: w.org.ID, RequestId: s.GetDevSessionId()}))
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
	if countAudit(t, w.env, w.org.ID, sandrunv1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED) != 1 {
		t.Fatal("audit events")
	}
}

// TestCreateRetryAfterProfileChange: when the CreateSandbox response is
// lost and an admin then changes the profile, the retry sends the original
// parameters with the key and recovers the created sandbox.
func TestCreateRetryAfterProfileChange(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	if _, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(ctx, connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{OrganizationId: w.org.ID,
		RunnerProfileId: profileID, Profile: &sandrunv1.RunnerProfile{Resources: &sandrunv1.Resources{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"resources"}}})); err != nil {
		t.Fatal(err)
	}
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
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
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeUnavailable, errors.New("503 Service Unavailable"))
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	stays(t, "one sandbox", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes) != 1 || w.runner(t, s.GetRunnerId()).GetOwnsandSandboxId() != sandboxes[0].GetSandboxId() {
		t.Fatalf("sandboxes %v", sandboxes)
	}
}

// TestDeleteAfterLostCreateResponse: a runner whose CreateSandbox response
// was lost stays DELETING until SandRun found its sandbox with the stored key,
// and the sandbox is deleted.
func TestDeleteAfterLostCreateResponse(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	ctx := context.Background()
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateResponseError = connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	})
	s := w.createSession(t, w.env.User(w.org.Member), profileID)
	eventually(t, "a sandbox with a lost response", func() bool { return len(w.fake.Sandboxes(w.osOrg)) == 1 })
	list, err := w.env.User(w.org.Owner).Work.ListRunners(ctx, connect.NewRequest(&sandrunv1.ListRunnersRequest{OrganizationId: w.org.ID}))
	if err != nil || len(list.Msg.GetRunners()) != 1 || list.Msg.GetRunners()[0].GetOwnsandSandboxId() != "" {
		t.Fatalf("runners %v %v", list, err)
	}
	runnerID := list.Msg.GetRunners()[0].GetRunnerId()
	if _, err := w.env.User(w.org.Member).DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{OrganizationId: w.org.ID, DevSessionId: s.GetDevSessionId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "runner deleting", func() bool { return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETING })
	stays(t, "runner deleting while the creation is uncertain", func() bool {
		return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETING
	})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateResponseError = nil })
	eventually(t, "runner deleted", func() bool { return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED })
	sandboxes := w.fake.Sandboxes(w.osOrg)
	if len(sandboxes) != 1 || sandboxes[0].GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETED || w.runner(t, runnerID).GetOwnsandSandboxId() != sandboxes[0].GetSandboxId() {
		t.Fatalf("sandboxes %v", sandboxes)
	}
}

func TestDeleteSandboxNow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		revokeKey   bool
		deleteError error
		activeAfter int
	}{
		{name: "revoked key", revokeKey: true, activeAfter: 1},
		{name: "not found", deleteError: connect.NewWireError(connect.CodeNotFound, errors.New("sandbox not found")), activeAfter: 1},
		{name: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			ctx := t.Context()
			member := w.env.User(w.org.Member)
			profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
			session := w.createSession(t, member, profileID)
			session = w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			if runner := w.runner(t, session.GetRunnerId()); runner.GetOwnsandSandboxId() == "" {
				t.Fatal("running session has no stored sandbox ID")
			}

			// Give the lease to another instance so only the RPC can delete the sandbox.
			w.env.App.Lease.Release()
			other := lease.Lease{DB: w.env.DB, InstanceID: "other-instance"}
			if held, err := other.TryAcquire(ctx); err != nil || !held {
				t.Fatalf("could not hand off the lease: held=%v, err=%v", held, err)
			}
			if tc.revokeKey {
				w.fake.RevokeKey(w.osOrg)
			}
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = tc.deleteError })

			_, err := member.DevSession.DeleteDevSession(ctx, connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{
				OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(),
			}))
			if err != nil {
				t.Fatal(err)
			}
			runner := w.runner(t, session.GetRunnerId())
			if runner.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETED ||
				runner.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
				t.Fatalf("runner after synchronous deletion: %v", runner)
			}
			if calls, active := w.fake.Calls("DeleteSandbox"), w.fake.Active(w.osOrg); calls != 1 || active != tc.activeAfter {
				t.Fatalf("DeleteSandbox calls=%d, active sandboxes=%d, want 1 and %d", calls, active, tc.activeAfter)
			}
		})
	}
}

func TestDeleteSandboxAttemptTimeout(t *testing.T) {
	t.Parallel()
	timing := testenv.FastWorkTiming()
	timing.DeleteTimeout = 150 * time.Millisecond
	w := newWorkWorldWithTiming(t, devsession.Timing{}, &timing)
	admin := w.env.User(w.org.Admin)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	session := w.createSession(t, w.env.User(w.org.Member), profileID)
	session = w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	runnerID := session.GetRunnerId()
	sandboxID := w.runner(t, runnerID).GetOwnsandSandboxId()
	if sandboxID == "" {
		t.Fatal("running session has no stored sandbox ID")
	}

	// Park the lease to isolate the inline attempt from reconciliation.
	w.env.App.Lease.Release()
	other := lease.Lease{DB: w.env.DB, InstanceID: "other-instance"}
	if held, err := other.TryAcquire(t.Context()); err != nil || !held {
		t.Fatalf("could not park the lease: held=%v, err=%v", held, err)
	}

	attempts := make(chan error, 1)
	w.fake.SetDeleteHook(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			select {
			case attempts <- ctx.Err():
			default:
			}
			return ctx.Err()
		case <-t.Context().Done():
			return t.Context().Err()
		}
	})
	assertAttemptTimedOut := func() {
		t.Helper()
		select {
		case err := <-attempts:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("delete attempt ended with %v, want deadline exceeded", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("OwnSand delete attempt did not time out")
		}
	}
	assertRunner := func(r *sandrunv1.Runner, state sandrunv1.RunnerState) {
		t.Helper()
		if r.GetState() != state || r.GetOwnsandSandboxId() != sandboxID ||
			r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
			t.Fatalf("runner = %v, want %s with the original sandbox ID and deletion reason", r, state)
		}
	}

	deleteRunner := func() *sandrunv1.Runner {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		resp, err := admin.Work.DeleteRunner(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerRequest{
			OrganizationId: w.org.ID, RunnerId: runnerID,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetRunner()
	}
	assertRunner(deleteRunner(), sandrunv1.RunnerState_RUNNER_STATE_DELETING)
	assertAttemptTimedOut()
	if s := w.session(t, session.GetDevSessionId()); s.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
		s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED {
		t.Fatalf("session after runner deletion: %v", s)
	}

	assertRunner(deleteRunner(), sandrunv1.RunnerState_RUNNER_STATE_DELETING)
	action := sandrunv1.AuditAction_AUDIT_ACTION_RUNNER_DELETED
	if got := countAudit(t, w.env, w.org.ID, action); got != 1 {
		t.Fatalf("audit events after repeated DeleteRunner: %d, want 1", got)
	}

	other.Release()
	w.env.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: w.fake, WorkTiming: &timing}, &layers.Layers{}))
	assertAttemptTimedOut()
	assertRunner(w.runner(t, runnerID), sandrunv1.RunnerState_RUNNER_STATE_DELETING)

	w.fake.SetDeleteHook(nil)
	eventually(t, "the reconciler deletes the original sandbox", func() bool {
		return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED &&
			w.fake.Active(w.osOrg) == 0
	})
	assertRunner(w.runner(t, runnerID), sandrunv1.RunnerState_RUNNER_STATE_DELETED)
}

func TestTeardownSandboxDeleteAttemptTimeout(t *testing.T) {
	t.Parallel()
	for _, rpc := range []string{"DeleteRunnerProfile", "RemoveOwnSandLink", "DeleteOrganization"} {
		for _, scenario := range []struct {
			name         string
			cancelCaller bool
			retryCode    connect.Code
		}{
			{name: "success"},
			{name: "not_found", retryCode: connect.CodeNotFound},
			{name: "unauthenticated", retryCode: connect.CodeUnauthenticated},
			{name: "caller_cancellation", cancelCaller: true},
		} {
			t.Run(rpc+"/"+scenario.name, func(t *testing.T) {
				const watchdog = 5 * time.Second
				timing := testenv.FastWorkTiming()
				timing.DeleteTimeout = 150 * time.Millisecond
				wantCause, wantRPCCode := context.DeadlineExceeded, connect.CodeDeadlineExceeded
				if scenario.cancelCaller {
					timing.DeleteTimeout = 2 * watchdog
					wantCause, wantRPCCode = context.Canceled, connect.CodeCanceled
				}
				w := newWorkWorldWithTiming(t, devsession.Timing{}, &timing)
				admin := w.env.User(w.org.Admin)
				profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
				var runnerID, sandboxID string
				eventually(t, "a warm runner with a sandbox ID", func() bool {
					resp, err := admin.Work.ListRunners(t.Context(), connect.NewRequest(&sandrunv1.ListRunnersRequest{
						OrganizationId: w.org.ID,
						States:         []sandrunv1.RunnerState{sandrunv1.RunnerState_RUNNER_STATE_WARM},
					}))
					if err != nil || len(resp.Msg.GetRunners()) != 1 {
						return false
					}
					r := resp.Msg.GetRunners()[0]
					runnerID, sandboxID = r.GetRunnerId(), r.GetOwnsandSandboxId()
					return sandboxID != ""
				})

				// Park the lease to keep reconciliation from racing the teardown retry.
				w.env.App.Lease.Release()
				other := lease.Lease{DB: w.env.DB, InstanceID: "other-instance"}
				if held, err := other.TryAcquire(t.Context()); err != nil || !held {
					t.Fatalf("could not park the lease: held=%v, err=%v", held, err)
				}

				pool := w.env.DB.Pool
				var key []byte
				if err := pool.QueryRow(t.Context(), "SELECT api_key_ciphertext FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&key); err != nil {
					t.Fatal(err)
				}
				teardown := func(ctx context.Context) error {
					switch rpc {
					case "DeleteRunnerProfile":
						_, err := admin.Profile.DeleteRunnerProfile(ctx, connect.NewRequest(&sandrunv1.DeleteRunnerProfileRequest{
							OrganizationId: w.org.ID, RunnerProfileId: profileID,
						}))
						return err
					case "RemoveOwnSandLink":
						return unlinkOwnSand(ctx, w.env, w.org)
					default:
						return deleteSandRunOrg(ctx, w.env, w.org)
					}
				}
				arrived := make(chan context.Context, 1)
				w.fake.SetDeleteHook(func(ctx context.Context) error {
					arrived <- ctx
					<-ctx.Done()
					return ctx.Err()
				})
				parent, cancel := context.WithTimeout(t.Context(), watchdog)
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- teardown(parent) }()

				var attempt context.Context
				select {
				case attempt = <-arrived:
				case err := <-result:
					t.Fatalf("teardown returned before attempting sandbox deletion: %v", err)
				case <-parent.Done():
					t.Fatalf("waiting for the OwnSand call: %v", parent.Err())
				}
				deadline, ok := attempt.Deadline()
				remaining := time.Until(deadline)
				if maxRemaining := min(timing.DeleteTimeout, watchdog+time.Millisecond); !ok || remaining > maxRemaining {
					t.Fatalf("OwnSand attempt deadline in %v, want at most %v (configured %v)", remaining, maxRemaining, timing.DeleteTimeout)
				}
				if scenario.cancelCaller {
					cancel()
				}
				select {
				case <-attempt.Done():
				case <-time.After(watchdog):
					t.Fatal("OwnSand attempt stayed live after teardown cancellation")
				}
				if err := attempt.Err(); !errors.Is(err, wantCause) {
					t.Fatalf("OwnSand attempt ended with %v, want %v", err, wantCause)
				}
				if err := <-result; connect.CodeOf(err) != wantRPCCode {
					t.Fatalf("teardown returned %v, want %v", err, wantRPCCode)
				}
				if !scenario.cancelCaller && parent.Err() != nil {
					t.Fatalf("caller context ended with the attempt: %v", parent.Err())
				}

				eventually(t, "teardown to release its claim and retain its target and credential", func() bool {
					var keptKey []byte
					var removing bool
					err := pool.QueryRow(t.Context(), `SELECT l.api_key_ciphertext,
						l.removing OR l.removal_token IS NOT NULL OR o.deletion_attempt_id IS NOT NULL
						FROM organizations o JOIN ownsand_links l USING (organization_id)
						JOIN runner_profiles p USING (organization_id)
						WHERE o.organization_id = $1 AND p.runner_profile_id = $2`, w.org.ID, profileID).Scan(&keptKey, &removing)
					return err == nil && !removing && bytes.Equal(key, keptKey)
				})
				assertRunner := func(state sandrunv1.RunnerState) {
					t.Helper()
					r := w.runner(t, runnerID)
					if r.GetState() != state || r.GetOwnsandSandboxId() != sandboxID ||
						r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
						t.Fatalf("runner = %v, want %s with the original sandbox ID and CLEAN_UP", r, state)
					}
				}
				assertRunner(sandrunv1.RunnerState_RUNNER_STATE_DELETING)
				t.Logf("OwnSand had %v until its deadline and observed %v. The runner, teardown target and credential remained available.", remaining, wantCause)

				w.fake.SetDeleteHook(nil)
				wantActive := 0
				if scenario.retryCode != 0 {
					w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
						o.DeleteError = connect.NewWireError(scenario.retryCode, errors.New("sandbox deletion is already terminal"))
					})
					if scenario.retryCode == connect.CodeNotFound {
						w.fake.EndSandbox(w.osOrg, sandboxID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
					} else {
						wantActive = 1 // A sandbox with a revoked key ends at its idle TTL.
					}
				}
				retry, cancelRetry := context.WithTimeout(t.Context(), watchdog)
				defer cancelRetry()
				if err := teardown(retry); err != nil {
					t.Fatalf("retry teardown: %v", err)
				}
				if calls, active := w.fake.Calls("DeleteSandbox"), w.fake.Active(w.osOrg); calls != 1 || active != wantActive {
					t.Fatalf("retry made %d delete calls and left %d active sandboxes, want 1 and %d", calls, active, wantActive)
				}
				var orgExists, linkExists, profileExists bool
				if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM organizations WHERE organization_id = $1),
					EXISTS (SELECT FROM ownsand_links WHERE organization_id = $1),
					EXISTS (SELECT FROM runner_profiles WHERE runner_profile_id = $2)`, w.org.ID, profileID).Scan(&orgExists, &linkExists, &profileExists); err != nil {
					t.Fatal(err)
				}
				if orgExists != (rpc != "DeleteOrganization") || linkExists != (rpc == "DeleteRunnerProfile") || profileExists != (rpc == "RemoveOwnSandLink") {
					t.Fatalf("rows after retry: organization=%v link=%v profile=%v", orgExists, linkExists, profileExists)
				}
				if orgExists {
					assertRunner(sandrunv1.RunnerState_RUNNER_STATE_DELETED)
				}
				t.Log("The retry finalized teardown.")
			})
		}
	}
}

func TestDeleteSandboxRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		revokeKey   bool
		activeAfter int
	}{
		{name: "key revoked before retry", revokeKey: true, activeAfter: 1},
		{name: "OwnSand recovers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			member := w.env.User(w.org.Member)
			profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
			session := w.createSession(t, member, profileID)
			session = w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.DeleteError = connect.NewWireError(connect.CodeUnavailable, errors.New("device busy"))
			})
			_, err := member.DevSession.DeleteDevSession(t.Context(), connect.NewRequest(&sandrunv1.DeleteDevSessionRequest{
				OrganizationId: w.org.ID, DevSessionId: session.GetDevSessionId(),
			}))
			if err != nil {
				t.Fatal(err)
			}

			eventually(t, "a deletion retry after the RPC", func() bool { return w.fake.Calls("DeleteSandbox") >= 2 })
			before := w.runner(t, session.GetRunnerId())
			if before.GetState() != sandrunv1.RunnerState_RUNNER_STATE_DELETING || before.GetOwnsandSandboxId() == "" ||
				before.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER {
				t.Fatalf("runner after unavailable error: %v", before)
			}
			if tc.revokeKey {
				w.fake.RevokeKey(w.osOrg)
			}
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = nil })

			eventually(t, "runner deleted by the reconciler", func() bool {
				return w.runner(t, session.GetRunnerId()).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
			})
			after := w.runner(t, session.GetRunnerId())
			if after.GetEndReason() != before.GetEndReason() || w.fake.Active(w.osOrg) != tc.activeAfter {
				t.Fatalf("runner after retry: %v, active sandboxes=%d, want %d", after, w.fake.Active(w.osOrg), tc.activeAfter)
			}
			calls := w.fake.Calls("DeleteSandbox")
			stays(t, "no more DeleteSandbox calls", func() bool { return w.fake.Calls("DeleteSandbox") == calls })
		})
	}
}

func TestStartFailureAfterSandboxStatusDeadline(t *testing.T) {
	t.Parallel()
	timing := testenv.FastWorkTiming()
	w := newWorkWorldWithTiming(t, devsession.Timing{}, &timing)
	unavailable := connect.NewWireError(connect.CodeUnavailable, errors.New("OwnSand temporarily unavailable"))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.StartRunning = false
		o.GetError = unavailable
	})
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	session := w.createSession(t, w.env.User(w.org.Member), profileID)
	requestID := session.GetDevSessionId()
	getRequest := func() *work.Request {
		t.Helper()
		req, err := w.l.Engine.GetRequest(t.Context(), w.env.DB.Pool, w.org.ID, requestID)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	queueTime := getRequest().QueueTime

	for attempt := int32(1); attempt <= work.MaxStartAttempts; attempt++ {
		starting := w.waitState(t, requestID, sandrunv1.RequestState_REQUEST_STATE_STARTING)
		runnerID := starting.GetRunnerId()
		runner := w.runner(t, runnerID)
		if runner.GetOwnsandSandboxId() == "" {
			t.Fatal("starting runner has no sandbox ID")
		}
		// Prevent replacement sandboxes while checking the result of this attempt.
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = unavailable })

		w.env.Clock.Advance(timing.StartTimeout)
		calls := w.fake.Calls("GetSandbox")
		eventually(t, "repeated status errors at the startup limit", func() bool {
			return w.fake.Calls("GetSandbox") >= calls+3
		})
		if state := w.runner(t, runnerID).GetState(); state != sandrunv1.RunnerState_RUNNER_STATE_CREATING {
			t.Fatalf("runner state at startup limit = %s", state)
		}
		if req := getRequest(); req.State != sandrunv1.RequestState_REQUEST_STATE_STARTING || req.StartAttempts != attempt {
			t.Fatalf("request at startup limit has state %s and %d attempts, want STARTING and %d attempts", req.State, req.StartAttempts, attempt)
		}
		w.env.Clock.Advance(time.Millisecond)

		eventually(t, "runner start failure after the status deadline", func() bool {
			return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_FAILED
		})
		if reason := w.runner(t, runnerID).GetEndReason(); reason != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
			t.Fatalf("runner end reason = %s", reason)
		}
		eventually(t, "failed sandbox deleted and cleanup mark cleared", func() bool {
			var pending bool
			if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT sandbox_cleanup FROM runners WHERE runner_id = $1", runnerID).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			return !pending && w.fake.Active(w.osOrg) == 0
		})

		req := getRequest()
		if req.StartAttempts != attempt || !req.QueueTime.Equal(queueTime) {
			t.Fatalf("request has %d attempts and queue time %v, want %d and %v", req.StartAttempts, req.QueueTime, attempt, queueTime)
		}
		if attempt == work.MaxStartAttempts {
			if req.State != sandrunv1.RequestState_REQUEST_STATE_FAILED || req.EndReason != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED {
				t.Fatalf("exhausted request state = %s, end reason = %s", req.State, req.EndReason)
			}
			break
		}
		if req.State != sandrunv1.RequestState_REQUEST_STATE_QUEUED || req.RunnerID != "" {
			t.Fatalf("retry request state = %s, runner = %q", req.State, req.RunnerID)
		}
		w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
	}
}

func TestStartFailureSandboxStatusBeforeDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		recovered bool
	}{
		{name: "recovers", recovered: true},
		{name: "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			unavailable := connect.NewWireError(connect.CodeUnavailable, errors.New("OwnSand temporarily unavailable"))
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.StartRunning = false
				o.GetError = unavailable
			})
			profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
			session := w.createSession(t, w.env.User(w.org.Member), profileID)
			starting := w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_STARTING)
			runnerID := starting.GetRunnerId()
			eventually(t, "sandbox status lookup", func() bool { return w.fake.Calls("GetSandbox") > 0 })

			if tc.recovered {
				w.fake.SetRunning(w.osOrg, w.runner(t, runnerID).GetOwnsandSandboxId())
				w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.GetError = nil })
				running := w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
				if running.GetRunnerId() != runnerID {
					t.Fatalf("recovered session runner = %q, want %q", running.GetRunnerId(), runnerID)
				}
				return
			}
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = unavailable
				o.GetError = connect.NewWireError(connect.CodeNotFound, errors.New("sandbox not found"))
			})
			eventually(t, "runner start failure without advancing the clock", func() bool {
				return w.runner(t, runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_FAILED
			})
			if reason := w.runner(t, runnerID).GetEndReason(); reason != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
				t.Fatalf("runner end reason = %s", reason)
			}
			w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_QUEUED)
		})
	}
}

// TestStartFailureCleanupRetry: when the sandbox deletion after a start
// failure fails, SandRun retries it until OwnSand deletes the sandbox.
func TestStartFailureCleanupRetry(t *testing.T) {
	t.Parallel()
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
	s = w.waitState(t, s.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_FAILED)
	if s.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_RUNNER_START_FAILED || w.fake.Active(w.osOrg) != 3 {
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

func TestStartFailureCleanupRetryAfterCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	timing := testenv.FastWorkTiming()
	timing.TerminalCleanupTimeout = 2 * time.Second
	w := newWorkWorldWithTiming(t, devsession.Timing{}, &timing)
	w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		code := int64(1)
		return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
	}, nil)

	attempts := make(chan context.Context, 1)
	allowDelete := make(chan struct{})
	w.fake.SetDeleteHook(func(ctx context.Context) error {
		select {
		case attempts <- ctx:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.Context().Done():
			return t.Context().Err()
		case <-allowDelete:
			return nil
		}
	})
	nextAttempt := func() context.Context {
		t.Helper()
		select {
		case ctx := <-attempts:
			return ctx
		case <-time.After(5 * time.Second):
			t.Fatal("OwnSand received no cleanup attempt")
			return nil
		}
	}
	assertAttemptEnded := func(ctx context.Context, want error) {
		t.Helper()
		select {
		case <-ctx.Done():
			if err := ctx.Err(); !errors.Is(err, want) {
				t.Fatalf("cleanup attempt ended with %v, want %v", err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("OwnSand cleanup attempt did not end with %v", want)
		}
	}

	// A warm runner fails once and stays in backoff on the fixture clock.
	w.createProfile(t, devProfileProto("warm", 1, 0))
	inline := nextAttempt()
	leaseReleased := make(chan struct{})
	go func() {
		defer close(leaseReleased)
		w.env.App.Lease.Release()
	}()
	assertAttemptEnded(inline, context.Canceled)
	select {
	case <-leaseReleased:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup blocked lease release")
	}

	pool := w.env.DB.Pool
	var runnerID string
	if err := pool.QueryRow(t.Context(), "SELECT runner_id::text FROM runners WHERE organization_id = $1", w.org.ID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	checkPending := func() bool {
		t.Helper()
		var pending bool
		if err := pool.QueryRow(t.Context(), "SELECT sandbox_cleanup FROM runners WHERE runner_id = $1", runnerID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		return pending
	}
	if !checkPending() || w.fake.Active(w.osOrg) != 1 {
		t.Fatal("lease cancellation lost the cleanup mark or deleted the sandbox")
	}
	t.Log("OwnSand observed cancellation during lease release. The cleanup mark and sandbox remained.")

	replica := w.env.NewReplica(t, testenv.WithLayerOptions(layers.Options{OwnSand: w.fake, WorkTiming: &timing}, &layers.Layers{}))
	retry := nextAttempt()
	assertAttemptEnded(retry, context.DeadlineExceeded)
	if !replica.App.Lease.Held() || !checkPending() || w.fake.Active(w.osOrg) != 1 {
		t.Fatal("the deadline lost the lease, lost the cleanup mark or deleted the sandbox")
	}
	t.Log("OwnSand observed the cleanup deadline while SandRun held the lease. The cleanup mark and sandbox remained.")

	close(allowDelete)
	eventually(t, "cleanup completed", func() bool { return !checkPending() && w.fake.Active(w.osOrg) == 0 })
	if r := w.runner(t, runnerID); r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_FAILED ||
		r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
		t.Fatalf("runner after cleanup = %v, want FAILED with START_FAILED", r)
	}
	t.Log("A later OwnSand call deleted the sandbox and cleared the mark. GetRunner returned FAILED with START_FAILED.")
}

func TestTerminalSandboxCleanupBeforeTeardown(t *testing.T) {
	t.Parallel()
	for _, teardown := range []struct {
		name   string
		delete bool
	}{
		{name: "unlink"},
		{name: "delete organization", delete: true},
	} {
		for _, scenario := range []string{"retry", "already removed", "key rejected", "failed mark update"} {
			t.Run(teardown.name+"/"+scenario, func(t *testing.T) {
				w := newWorkWorld(t, devsession.Timing{})
				ctx := context.Background()
				remove := func() error {
					if teardown.delete {
						_, err := w.env.User(w.org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&sandrunv1.DeleteOrganizationRequest{OrganizationId: w.org.ID}))
						return err
					}
					_, err := w.env.User(w.org.Admin).OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&sandrunv1.RemoveOwnSandLinkRequest{OrganizationId: w.org.ID}))
					return err
				}
				w.fake.SetExec(func(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
					code := int64(1)
					return &ownsand.ExecResult{ExecID: p.ExecID, Kind: ownsand.ExecResultExited, ExitCode: &code}, nil
				}, nil)
				setDeleteError := func(err error) { w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.DeleteError = err }) }
				setDeleteError(connect.NewWireError(connect.CodeUnavailable, errors.New("device busy")))
				// The fixture clock keeps warm fill in its backoff after one failed start.
				w.createProfile(t, devProfileProto("warm", 1, 0))
				pool := w.env.DB.Pool
				var runnerID, sandboxID string
				eventually(t, "a failed runner with pending sandbox cleanup", func() bool {
					err := pool.QueryRow(ctx, "SELECT runner_id::text, ownsand_sandbox_id::text FROM runners WHERE organization_id = $1 AND state = $2 AND end_reason = $3 AND sandbox_cleanup",
						w.org.ID, int16(sandrunv1.RunnerState_RUNNER_STATE_FAILED), int16(sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED)).Scan(&runnerID, &sandboxID)
					return err == nil && w.fake.Active(w.osOrg) == 1 && w.fake.Calls("DeleteSandbox") > 0
				})

				var key []byte
				if err := pool.QueryRow(ctx, "SELECT api_key_ciphertext FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&key); err != nil {
					t.Fatal(err)
				}
				assertRetained := func() {
					t.Helper()
					var pending, removing bool
					var keptKey []byte
					err := pool.QueryRow(ctx, `SELECT r.sandbox_cleanup, l.removing, l.api_key_ciphertext
						FROM organizations o JOIN runners r USING (organization_id) JOIN ownsand_links l USING (organization_id)
						WHERE o.organization_id = $1 AND r.runner_id = $2`,
						w.org.ID, runnerID).Scan(&pending, &removing, &keptKey)
					if err != nil || !pending || removing || !bytes.Equal(key, keptKey) {
						t.Fatalf("teardown did not retain the organization, pending runner and key: pending=%v removing=%v same key=%v err=%v",
							pending, removing, bytes.Equal(key, keptKey), err)
					}
				}
				wantCode(t, "teardown while deletion is unavailable", remove(), connect.CodeUnavailable)
				assertRetained()
				if got := w.fake.Active(w.osOrg); got != 1 {
					t.Fatalf("active sandboxes after failed teardown = %d, want 1", got)
				}

				wantActive := 0
				switch scenario {
				case "already removed":
					w.fake.EndSandbox(w.osOrg, sandboxID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
					setDeleteError(connect.NewWireError(connect.CodeNotFound, errors.New("sandbox already removed")))
				case "key rejected":
					setDeleteError(connect.NewWireError(connect.CodeUnauthenticated, errors.New("OwnSand key revoked")))
					wantActive = 1 // The sandbox ends when its idle TTL expires.
				case "failed mark update":
					if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
						BEGIN RAISE EXCEPTION 'cannot clear cleanup mark'; END; $$;
						CREATE TRIGGER refuse_cleanup BEFORE UPDATE OF sandbox_cleanup ON runners
						FOR EACH ROW WHEN (OLD.sandbox_cleanup AND NOT NEW.sandbox_cleanup) EXECUTE FUNCTION refuse_cleanup();`); err != nil {
						t.Fatal(err)
					}
					setDeleteError(nil)
					wantCode(t, "teardown while the mark cannot be cleared", remove(), connect.CodeInternal)
					assertRetained()
					if got := w.fake.Active(w.osOrg); got != 0 {
						t.Fatalf("active sandboxes after failed mark update = %d, want 0", got)
					}
					if _, err := pool.Exec(ctx, "DROP TRIGGER refuse_cleanup ON runners"); err != nil {
						t.Fatal(err)
					}
				default:
					setDeleteError(nil)
				}
				if err := remove(); err != nil {
					t.Fatal(err)
				}
				if got := w.fake.Active(w.osOrg); got != wantActive {
					t.Fatalf("active sandboxes after teardown = %d, want %d", got, wantActive)
				}
				var orgExists, linkExists, runnersExist bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM organizations WHERE organization_id = $1),
					EXISTS (SELECT FROM ownsand_links WHERE organization_id = $1),
					EXISTS (SELECT FROM runners WHERE organization_id = $1)`, w.org.ID).Scan(&orgExists, &linkExists, &runnersExist); err != nil {
					t.Fatal(err)
				}
				if linkExists || orgExists == teardown.delete || runnersExist == teardown.delete {
					t.Fatalf("rows after teardown: organization=%v link=%v runners=%v", orgExists, linkExists, runnersExist)
				}
				if !teardown.delete {
					r := w.runner(t, runnerID)
					var pending bool
					err := pool.QueryRow(ctx, "SELECT sandbox_cleanup FROM runners WHERE runner_id = $1", runnerID).Scan(&pending)
					if err != nil || pending || r.GetState() != sandrunv1.RunnerState_RUNNER_STATE_FAILED ||
						r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_START_FAILED {
						t.Fatalf("runner after unlink: state=%s reason=%s pending=%v err=%v", r.GetState(), r.GetEndReason(), pending, err)
					}
				}
			})
		}
	}
}

// TestStalledCreateSandbox verifies that another session can queue while
// OwnSand stalls and that the first runner retries with its stored key.
func TestStalledCreateSandbox(t *testing.T) {
	t.Parallel()
	timing := testenv.FastWorkTiming()
	timing.CreateTimeout = 5 * time.Second
	w := newWorkWorldWithTiming(t, devsession.Timing{}, &timing)
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))

	testCtx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type createAttempt struct {
		deadline    time.Time
		hasDeadline bool
		runnerID    string
		key         string
	}
	entered := make(chan createAttempt, 1)
	finished := make(chan error, 1)
	retried := make(chan string, 1)
	var first sync.Once
	var firstRunnerID string
	w.fake.SetCreateRequestHook(func(ctx context.Context, req *ownsandv1.CreateSandboxRequest) error {
		runnerID := req.GetLabels()[ownsandclient.LabelRunnerID]
		key := req.GetIdempotencyKey()
		initial := false
		first.Do(func() {
			initial = true
			firstRunnerID = runnerID
			deadline, hasDeadline := ctx.Deadline()
			entered <- createAttempt{deadline: deadline, hasDeadline: hasDeadline, runnerID: runnerID, key: key}
		})
		if !initial {
			if runnerID == firstRunnerID {
				select {
				case retried <- key:
				default:
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			err := ctx.Err()
			finished <- err
			return connect.NewError(connect.CodeDeadlineExceeded, err)
		case <-testCtx.Done():
			return testCtx.Err()
		}
	})

	createSession := func(user string) (*sandrunv1.DevSession, time.Time) {
		t.Helper()
		resp, err := w.env.User(user).DevSession.CreateDevSession(testCtx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
			OrganizationId:  w.org.ID,
			RunnerProfileId: profileID,
			Label:           "build",
		}))
		receivedAt := time.Now()
		if err != nil {
			t.Fatalf("queue dev session: %v", err)
		}
		return resp.Msg.GetDevSession(), receivedAt
	}

	a, _ := createSession(w.org.Member)
	var stalled createAttempt
	select {
	case stalled = <-entered:
	case <-testCtx.Done():
		t.Fatal("first sandbox create did not enter the hook")
	}
	if !stalled.hasDeadline {
		t.Fatal("first sandbox create has no deadline")
	}
	if stalled.runnerID == "" || stalled.key == "" {
		t.Fatal("first sandbox create has no runner ID or idempotency key")
	}
	b, queuedAt := createSession(w.org.Admin)
	if b.GetState() != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("second session create returned %s, want QUEUED", b.GetState())
	}
	if !queuedAt.Before(stalled.deadline) {
		t.Fatalf("second session queue response arrived %v after the first sandbox create deadline", queuedAt.Sub(stalled.deadline))
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first sandbox create ended with %v, want context.DeadlineExceeded", err)
		}
	case <-testCtx.Done():
		t.Fatal("first sandbox create did not reach its deadline")
	}
	select {
	case key := <-retried:
		if key != stalled.key {
			t.Fatalf("retry for runner %s used idempotency key %q, want %q", stalled.runnerID, key, stalled.key)
		}
	case <-testCtx.Done():
		t.Fatalf("sandbox create for runner %s was not retried", stalled.runnerID)
	}

	for _, session := range []*sandrunv1.DevSession{a, b} {
		id := session.GetDevSessionId()
		eventually(t, "dev session "+id+" to reach RUNNING", func() bool {
			resp, err := w.env.User(w.org.Owner).DevSession.GetDevSession(testCtx, connect.NewRequest(&sandrunv1.GetDevSessionRequest{
				OrganizationId: w.org.ID,
				DevSessionId:   id,
			}))
			if err != nil {
				t.Fatalf("get dev session %s: %v", id, err)
			}
			return resp.Msg.GetDevSession().GetState() == sandrunv1.RequestState_REQUEST_STATE_RUNNING
		})
	}
	if n := len(w.fake.Sandboxes(w.osOrg)); n != 2 {
		t.Fatalf("got %d sandboxes, want 2", n)
	}
}

func TestWatchQueueSnapshotReplay(t *testing.T) {
	t.Parallel()
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
			seedWatchRequests(t, q, other.ID, "other", sandrunv1.RequestState_REQUEST_STATE_RUNNING, 1)

			if test.openFirst {
				seedWatchRequests(t, q, w.org.ID, "open", sandrunv1.RequestState_REQUEST_STATE_RUNNING, 1)
			}
			seedWatchRequests(t, q, w.org.ID, "historical", sandrunv1.RequestState_REQUEST_STATE_ENDED, test.endedRows)
			if _, err := q.Exec(t.Context(), `INSERT INTO runners
				(runner_id, organization_id, runner_profile_id, kind, guest_os, state, idempotency_key,
				 create_request, create_time, update_time, end_time)
				SELECT gen_random_uuid(), $1, gen_random_uuid(), $2, $3, $4, gen_random_uuid(), '',
				 now(), now(), now() FROM generate_series(1, 3)`,
				w.org.ID, int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION),
				int16(sandrunv1.GuestOs_GUEST_OS_LINUX), int16(sandrunv1.RunnerState_RUNNER_STATE_DELETED)); err != nil {
				t.Fatal(err)
			}
			if test.openLast {
				seedWatchRequests(t, q, w.org.ID, "open", sandrunv1.RequestState_REQUEST_STATE_RUNNING, 1)
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
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	q := w.env.DB.Pool
	seedWatchRequests(t, q, w.org.ID, "open", sandrunv1.RequestState_REQUEST_STATE_RUNNING, 2)
	watch, cancel := openQueueWatch(t, w, "open-1", "open-2")

	endWatchRequest(t, q, w.org.ID, "open-1")
	expectWatchEnd(t, watch, "open-1")
	seedWatchRequests(t, q, w.org.ID, "new-terminal", sandrunv1.RequestState_REQUEST_STATE_ENDED, 1)
	expectWatchEnd(t, watch, "new-terminal-1")

	// Advance the change sequence by more than the previous replay window.
	if _, err := q.Exec(t.Context(), "SELECT nextval('work_change_seq') FROM generate_series(1, 1100)"); err != nil {
		t.Fatal(err)
	}
	seedWatchRequests(t, q, w.org.ID, "advance", sandrunv1.RequestState_REQUEST_STATE_ENDED, 1)
	expectWatchEnd(t, watch, "advance-1")
	endWatchRequest(t, q, w.org.ID, "open-2")
	expectWatchEnd(t, watch, "open-2")
	assertQueueWatchIdle(t, watch, cancel)
}

func TestWatchQueueLateCommit(t *testing.T) {
	t.Parallel()
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
			seedWatchRequests(t, q, w.org.ID, "open", sandrunv1.RequestState_REQUEST_STATE_RUNNING, 1)
			ctx := t.Context()
			tx, err := q.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if test.beforeSnapshot {
				endWatchRequest(t, tx, w.org.ID, "open-1")
				seedWatchRequests(t, q, w.org.ID, "historical", sandrunv1.RequestState_REQUEST_STATE_ENDED, 1)
			}
			watch, cancel := openQueueWatch(t, w, "open-1")
			if !test.beforeSnapshot {
				endWatchRequest(t, tx, w.org.ID, "open-1")
				seedWatchRequests(t, q, w.org.ID, "advance", sandrunv1.RequestState_REQUEST_STATE_ENDED, 1)
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
func seedWatchRequests(t *testing.T, q db.Querier, orgID, prefix string, state sandrunv1.RequestState, count int) {
	t.Helper()
	if _, err := q.Exec(t.Context(), `INSERT INTO requests
		(request_id, organization_id, runner_profile_id, kind, source, source_key, state, priority,
		 queue_time, queue_deadline, create_time, update_time, end_time)
		SELECT gen_random_uuid(), $1, gen_random_uuid(), $2, 99, $3 || '-' || n, $4, $5,
		 now(), now() + interval '1 hour', now(), now(), CASE WHEN $4::smallint = $6 THEN now() END
		FROM generate_series(1, $7) AS n`, orgID, int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION),
		prefix, int16(state), int16(sandrunv1.Priority_PRIORITY_NORMAL), int16(sandrunv1.RequestState_REQUEST_STATE_ENDED), count); err != nil {
		t.Fatal(err)
	}
}

func endWatchRequest(t *testing.T, q db.Querier, orgID, key string) {
	t.Helper()
	if tag, err := q.Exec(t.Context(), `UPDATE requests SET state = $3, end_time = now()
		WHERE organization_id = $1 AND source_key = $2`, orgID, key, int16(sandrunv1.RequestState_REQUEST_STATE_ENDED)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("end request %s: %v, %v", key, tag, err)
	}
}

func openQueueWatch(t *testing.T, w *workWorld, requestKeys ...string) (*connect.ServerStreamForClient[sandrunv1.WatchQueueResponse], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(ctx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: w.org.ID}))
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
		if req == nil || req.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
			t.Fatalf("unexpected snapshot event: %v", msg)
		}
		keys = append(keys, req.GetSourceKey())
	}
	t.Fatalf("snapshot did not complete: %v", watch.Err())
	return nil, nil
}

func expectWatchEnd(t *testing.T, watch *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse], key string) {
	t.Helper()
	if !watch.Receive() {
		t.Fatalf("no terminal event for %s: %v", key, watch.Err())
	}
	got := watch.Msg().GetRequest()
	if got.GetSourceKey() != key || got.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED {
		t.Fatalf("want terminal event for %s, got %v", key, watch.Msg())
	}
}

func assertQueueWatchIdle(t *testing.T, watch *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse], cancel context.CancelFunc) {
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
	t.Parallel()
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
	watch, err := w.env.User(w.org.Member).Work.WatchQueue(ctx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: w.org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	for watch.Receive() && watch.Msg().GetSnapshotComplete() == nil {
	}
	w.createSession(t, w.env.User(w.org.Member), profileID)
	runnerID := ""
	for runnerID == "" && watch.Receive() {
		if r := watch.Msg().GetRunner(); r != nil && r.GetState() == sandrunv1.RunnerState_RUNNER_STATE_CREATING {
			runnerID = r.GetRunnerId()
		}
	}
	if runnerID == "" {
		t.Fatalf("no CREATING runner: %v", watch.Err())
	}
	close(release)
	for watch.Receive() {
		if r := watch.Msg().GetRunner(); r != nil && r.GetRunnerId() == runnerID && r.GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED {
			if r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
				t.Fatalf("removed runner end reason = %v, want CLEAN_UP", r.GetEndReason())
			}
			return
		}
	}
	t.Fatalf("the removed runner was not sent as DELETED: %v", watch.Err())
}

const (
	watchRequestOpen = sandrunv1.RequestState_REQUEST_STATE_RUNNING
	watchRequestEnd  = sandrunv1.RequestState_REQUEST_STATE_CANCELED
	watchRunnerOpen  = sandrunv1.RunnerState_RUNNER_STATE_BUSY
	watchRunnerEnd   = sandrunv1.RunnerState_RUNNER_STATE_DELETED
)

func TestWatchQueueCommitOrder(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// Keep the work components from waiting on the same runners lock as the watch.
	seed := testenv.New(t)
	eventually(t, "core instance holds lease", seed.App.Lease.Held)
	env := seed.NewReplica(t, testenv.WithLayers(ownsandfake.New(), &layers.Layers{}))
	org := env.NewOrg("alice")
	requestID := seedQueueWatchRow(t, env, org.ID, false)
	runnerID := seedQueueWatchRow(t, env, org.ID, true)

	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSetup()
	config := env.DB.Pool.Config().ConnConfig
	observer, err := pgx.ConnectConfig(setupCtx, config)
	if err != nil {
		t.Fatalf("connect queue snapshot lock observer: %v", err)
	}
	defer observer.Close(t.Context())
	writer, err := env.DB.Pool.Begin(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(t.Context())
	if _, err := writer.Exec(setupCtx, "LOCK TABLE runners IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	writerPID := writer.Conn().PgConn().PID()
	cancelSetup()

	watchCtx, cancelWatch := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelWatch()
	type watchResult struct {
		stream *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse]
		err    error
	}
	firstEvent := make(chan watchResult, 1)
	go func() {
		stream, err := env.User(org.Member).Work.WatchQueue(watchCtx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org.ID}))
		// Connect can report a server error on Receive even if WatchQueue returned a stream.
		if err == nil && !stream.Receive() {
			err = stream.Err()
			if err == nil {
				err = io.EOF
			}
		}
		firstEvent <- watchResult{stream, err}
	}()

	// Commit only after the watch read requests and is blocked reading runners.
	observeCtx, cancelObservation := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancelObservation()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var readerPID uint32
	for readerPID == 0 && observeCtx.Err() == nil {
		select {
		case result := <-firstEvent:
			if result.err != nil {
				t.Fatalf("WatchQueue ended before waiting for writer PID %d: %v", writerPID, result.err)
			}
			t.Fatalf("WatchQueue emitted an event before waiting for writer PID %d", writerPID)
		case <-observeCtx.Done():
			continue
		case <-tick.C:
		}
		err := observer.QueryRow(observeCtx, `SELECT COALESCE(min(a.pid), 0)
			FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid AND l.database = a.datid
			WHERE a.datname = current_database()
			  AND l.relation = 'runners'::regclass AND l.locktype = 'relation'
			  AND l.mode = 'AccessShareLock' AND NOT l.granted
			  AND $1 = ANY(pg_blocking_pids(a.pid))`, writerPID).Scan(&readerPID)
		if err != nil && observeCtx.Err() == nil {
			logQueueSnapshotLocks(t, config)
			t.Fatalf("observe queue snapshot reader blocked by writer PID %d: %v", writerPID, err)
		}
	}
	if readerPID == 0 {
		logQueueSnapshotLocks(t, config)
		t.Fatalf("timed out waiting for the queue snapshot reader to be blocked by writer PID %d: %v", writerPID, observeCtx.Err())
	}
	tick.Stop()
	cancelObservation()
	t.Logf("queue snapshot reader PID %d waited for writer PID %d on runners", readerPID, writerPID)

	endQueueWatchRow(t, watchCtx, writer, requestID, false)
	endQueueWatchRow(t, watchCtx, writer, runnerID, true)
	if err := writer.Commit(watchCtx); err != nil {
		t.Fatal(err)
	}
	watch := <-firstEvent
	if watch.err != nil {
		t.Fatalf("receive the first WatchQueue event: %v", watch.err)
	}
	defer watch.stream.Close()
	expectQueueWatchState(t, watch.stream.Msg(), requestID, watchRequestOpen, watchRunnerOpen)
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), runnerID, watchRequestOpen, watchRunnerOpen)
	if msg := nextQueueWatchEvent(t, watch.stream); msg.GetSnapshotComplete() == nil {
		t.Fatalf("expected snapshot_complete, got %v", msg)
	}
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), requestID, watchRequestEnd, watchRunnerEnd)
	expectQueueWatchState(t, nextQueueWatchEvent(t, watch.stream), runnerID, watchRequestEnd, watchRunnerEnd)
}

func logQueueSnapshotLocks(t *testing.T, config *pgx.ConnConfig) {
	t.Helper()
	// Use a fresh connection in case the observation deadline interrupted a query.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Logf("connect to collect queue snapshot lock diagnostics: %v", err)
		return
	}
	defer conn.Close(ctx)

	var locks string
	err = conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(activity) ORDER BY pid), '[]'::jsonb)::text
		FROM (
			SELECT a.pid, a.state, a.wait_event_type, a.wait_event, pg_blocking_pids(a.pid) AS blocked_by,
				l.locktype, l.relation::regclass::text AS relation, l.mode, l.granted
			FROM pg_stat_activity a
			LEFT JOIN pg_locks l ON l.pid = a.pid AND (NOT l.granted OR
				(l.database = a.datid AND l.relation IN ('requests'::regclass, 'runners'::regclass)))
			WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
		) activity`).Scan(&locks)
	if err != nil {
		t.Logf("collect queue snapshot lock diagnostics: %v", err)
		return
	}
	t.Logf("queue snapshot lock diagnostics: %s", locks)
}

func seedQueueWatchRow(t *testing.T, env *testenv.Env, orgID string, runner bool) string {
	t.Helper()
	kind := int16(sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION)
	var row pgx.Row
	if runner {
		row = env.DB.Pool.QueryRow(t.Context(), `INSERT INTO runners (runner_id, organization_id, runner_profile_id,
			kind, guest_os, state, idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, $3, $4,
				gen_random_uuid(), ''::bytea, gen_random_uuid(), now(), now()) RETURNING runner_id::text`,
			orgID, kind, int16(sandrunv1.GuestOs_GUEST_OS_LINUX), int16(watchRunnerOpen))
	} else {
		row = env.DB.Pool.QueryRow(t.Context(), `INSERT INTO requests (request_id, organization_id, runner_profile_id,
			kind, source, source_key, state, priority, queue_time, queue_deadline, create_time, update_time)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, $3, gen_random_uuid()::text,
				$4, $5, now(), now() + interval '1 hour', now(), now()) RETURNING request_id::text`,
			orgID, kind, int16(work.SourceDevSession), int16(watchRequestOpen), int16(sandrunv1.Priority_PRIORITY_NORMAL))
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

func startQueueWatch(t *testing.T, ctx context.Context, env *testenv.Env, org testenv.Org) *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse] {
	t.Helper()
	watch, err := env.User(org.Member).Work.WatchQueue(ctx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

func nextQueueWatchEvent(t *testing.T, watch *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse]) *sandrunv1.WatchQueueResponse {
	t.Helper()
	if !watch.Receive() {
		t.Fatalf("watch ended before the expected event: %v", watch.Err())
	}
	return watch.Msg()
}

func expectQueueWatchState(t *testing.T, event *sandrunv1.WatchQueueResponse, id string, requestState sandrunv1.RequestState, runnerState sandrunv1.RunnerState) {
	t.Helper()
	if r := event.GetRequest(); r != nil && r.GetRequestId() == id && r.GetState() == requestState {
		return
	}
	if r := event.GetRunner(); r != nil && r.GetRunnerId() == id && r.GetState() == runnerState {
		return
	}
	t.Fatalf("expected state %v or %v for %s, got %v", requestState, runnerState, id, event)
}
