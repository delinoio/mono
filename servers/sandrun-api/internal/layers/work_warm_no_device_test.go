// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/devsession"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

func TestWarmNoDeviceCooldownAndRecovery(t *testing.T) {
	t.Parallel()
	w := newWorkWorld(t, devsession.Timing{})
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
	})
	var calls atomic.Int32
	firstKeys := make(chan string, 2)
	w.fake.SetCreateRequestHook(func(_ context.Context, req *ownsandv1.CreateSandboxRequest) error {
		switch calls.Add(1) {
		case 1:
			firstKeys <- req.GetIdempotencyKey()
			return connect.NewError(connect.CodeUnavailable, errors.New("lost response"))
		case 2:
			firstKeys <- req.GetIdempotencyKey()
		}
		return nil
	})
	profileID := w.createProfile(t, devProfileProto("warm", 1, 0))
	failed := func() int {
		return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_FAILED)
	}
	cooldown := testenv.FastWorkTiming().PassInterval
	for want := 1; want <= 3; want++ {
		eventually(t, "one failed runner for the next warm reservation", func() bool { return failed() == want })
		if want == 1 {
			var storedKey string
			if err := w.env.DB.Pool.QueryRow(t.Context(),
				"SELECT idempotency_key::text FROM runners WHERE runner_profile_id = $1", profileID).Scan(&storedKey); err != nil {
				t.Fatal(err)
			}
			if len(firstKeys) != 2 {
				t.Fatalf("captured %d create keys, want the first call and its replay", len(firstKeys))
			}
			if first, replay := <-firstKeys, <-firstKeys; first != storedKey || replay != storedKey {
				t.Fatalf("initial and replay keys %q and %q, want stored key %q", first, replay, storedKey)
			}
		}
		retryAt := w.env.Clock.Now().Add(cooldown)
		assertWarmNoDeviceRetry(t, w, profileID, want, &retryAt)
		w.env.Clock.Advance(cooldown - time.Millisecond)
		w.l.Engine.Kick(w.org.ID)
		stays(t, "no create during the cooldown", func() bool {
			return calls.Load() == int32(want+1) && failed() == want
		})
		if want == 3 {
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
		}
		w.env.Clock.Advance(time.Millisecond)
		w.l.Engine.Kick(w.org.ID)
		cooldown *= 2
	}
	eventually(t, "the profile warms when a device becomes eligible", func() bool {
		return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
	})
	assertWarmNoDeviceRetry(t, w, profileID, 0, nil)
	if calls.Load() != 5 || failed() != 3 || strings.Contains(w.env.Logs.String(), "warm_runner_fill_stopped") {
		t.Fatalf("after recovery: %d create calls, %d failed runners, logs %s", calls.Load(), failed(), w.env.Logs.String())
	}
}

func TestWarmNoDeviceRejectionFromOldRevision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *workWorld, string)
	}{
		{"profile", renameWarmProfile},
		{"link", func(t *testing.T, w *workWorld, _ string) {
			if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			profileID := w.createProfile(t, devProfileProto("warm", 0, 0))
			started, release := make(chan struct{}), make(chan struct{})
			var first atomic.Bool
			w.fake.SetCreateHook(func(ctx context.Context) error {
				if !first.CompareAndSwap(false, true) {
					return nil
				}
				close(started)
				select {
				case <-release:
					return connect.NewWireError(connect.CodeFailedPrecondition, errors.New("old revision has no eligible device"))
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			enableWarmProfiles(t, w, profileID)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("warm create did not start")
			}
			tc.change(t, w, profileID)
			close(release)
			eventually(t, "the new revision warms without advancing the clock", func() bool {
				return profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_WARM) == 1
			})
			if got := profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_FAILED); got != 1 {
				t.Fatalf("old revision failed runners = %d, want 1", got)
			}
			assertWarmNoDeviceRetry(t, w, profileID, 0, nil)
		})
	}
}

func TestNoDeviceRejectionForQueuedDemand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   sandrunv1.WorkloadKind
		source work.Source
		pool   string
	}{
		{"binding", sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, work.SourceDevSession, ""},
		{"GitHub sponsor", sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, work.SourceGitHub, "123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkWorld(t, devsession.Timing{})
			spec := devProfileProto("demand", 0, 0)
			spec.WorkloadKind = tc.kind
			if tc.pool != "" {
				spec.WorkspacePath = ""
			}
			profileID := w.createProfile(t, spec)
			w.env.Stop()
			w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
				o.CreateError = connect.NewWireError(connect.CodeFailedPrecondition, errors.New("no eligible device"))
			})
			ctx := t.Context()
			database := w.env.DB
			p, err := w.l.Profiles.Get(ctx, database.Pool, w.org.ID, profileID)
			if err != nil {
				t.Fatal(err)
			}
			id := api.NewID()
			if err := database.InTx(ctx, func(tx pgx.Tx) error {
				_, _, err := w.l.Engine.CreateRequest(ctx, tx, work.NewRequest{
					Profile: p, Source: tc.source, PoolKey: tc.pool, RequestID: id, OwnerUserID: w.org.Owner,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			w.l.Engine.Pass(ctx, w.org.ID)
			req, err := w.l.Engine.GetRequest(ctx, database.Pool, w.org.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			if req.State != sandrunv1.RequestState_REQUEST_STATE_FAILED ||
				req.EndReason != sandrunv1.RequestEndReason_REQUEST_END_REASON_NO_ELIGIBLE_DEVICE || req.StartAttempts != 0 {
				t.Fatalf("request after rejection: state %s, reason %s, start attempts %d", req.State, req.EndReason, req.StartAttempts)
			}
			if count := profileRunnerCount(t, w, profileID, sandrunv1.RunnerState_RUNNER_STATE_FAILED); count != 1 || w.fake.Calls("CreateSandbox") != 1 {
				t.Fatalf("queued demand: %d failed runners and %d create calls", count, w.fake.Calls("CreateSandbox"))
			}
			assertWarmNoDeviceRetry(t, w, profileID, 0, nil)
		})
	}
}

func assertWarmNoDeviceRetry(t *testing.T, w *workWorld, profileID string, wantFailures int, wantRetryAt *time.Time) {
	t.Helper()
	var failures, rejections int
	var retryAt *time.Time
	if err := w.env.DB.Pool.QueryRow(t.Context(), `SELECT warm_failures, warm_create_rejections, warm_retry_after
		FROM runner_profiles WHERE runner_profile_id = $1`, profileID).Scan(&failures, &rejections, &retryAt); err != nil {
		t.Fatal(err)
	}
	if failures != wantFailures || rejections != 0 || (wantRetryAt == nil && retryAt != nil) ||
		(wantRetryAt != nil && (retryAt == nil || !retryAt.Equal(*wantRetryAt))) {
		t.Fatalf("warm retry state: %d failures, %d permanent rejections, retry at %v; want %d failures, 0 permanent rejections, retry at %v",
			failures, rejections, retryAt, wantFailures, wantRetryAt)
	}
}
