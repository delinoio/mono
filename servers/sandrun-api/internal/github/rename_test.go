// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

const renamedLabel = "sandrun-renamed"

func TestRenamedProfileBeforeLabelPublication(t *testing.T) {
	gate := &registrationLookupGate{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(gate.release) })
	w := newGHWorld(t, worldOpts{}, func(o *app.Options) {
		cfg := o.DB.Pool.Config()
		cfg.ConnConfig.Tracer = gate
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		o.DB = &db.DB{Pool: pool}
	})
	defer release()
	w.link(t, 501, "acme")
	// Arm the lookup gate from Prepare, after dispatch has selected its
	// installation and before the driver takes the profile snapshot for Start.
	w.os.SetExec(func(ctx context.Context, sandboxID string, params ownsand.ExecParams) (*ownsand.ExecResult, error) {
		if !strings.Contains(strings.Join(params.Command, " "), "--jitconfig") {
			gate.armed.Store(true)
		}
		return w.progs.exec(ctx, sandboxID, params)
	}, w.progs.attach)

	// Use a warm runner so the new-label job is the only queued request and
	// relies on this start for coverage.
	profileID := w.createProfile(t, ghProfile("linux", 1))
	awaitUnlinkStep(t, "runner installation lookup before label publication", gate.entered)
	preparing := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
	if len(preparing) != 1 {
		t.Fatalf("preparing runners = %d, want 1", len(preparing))
	}
	id := preparing[0].GetRunnerId()
	var published bool
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM github_runners WHERE runner_id = $1)", id).Scan(&published); err != nil || published {
		t.Fatalf("registration label published before rename = %v, error = %v", published, err)
	}
	renameGitHubProfile(t, w, profileID)
	w.send(t, "workflow_job", job("queued", 501, 1, []string{renamedLabel}, ""))
	w.l.GitHub.Wait()
	release()

	eventually(t, "the original runner waiting for work", func() bool {
		return w.runner(t, id).GetState() == sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	})
	assertRegistrationLabel(t, w, id, renamedLabel, "acme")
	t.Log("The original runner published the renamed profile label and used it in the GitHub JIT request.")
}

// registrationLookupGate holds the first unlocked installation lookup
// after guest preparation. This is the adapter's first query, after the
// driver's unlocked profile read and before the adapter publishes a label.
type registrationLookupGate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	hold    sync.Once
}

func (g *registrationLookupGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.HasSuffix(q.SQL, " FROM github_installations WHERE github_installation_id = $1") && g.armed.CompareAndSwap(true, false) {
		g.hold.Do(func() {
			close(g.entered)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*registrationLookupGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRenamedProfileWhileJITRegistrationIsPending(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	profileID := w.createProfile(t, ghProfile("linux", 0))

	entered, release := make(chan struct{}), make(chan struct{})
	releaseJIT := sync.OnceFunc(func() { close(release) })
	defer releaseJIT()
	w.gh.Update(func() {
		w.gh.JITGate = func() {
			close(entered)
			<-release
		}
	})
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	awaitUnlinkStep(t, "old-label JIT request", entered)
	preparing := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
	if len(preparing) != 1 {
		t.Fatalf("preparing runners = %d, want 1", len(preparing))
	}
	id := preparing[0].GetRunnerId()
	var label string
	var githubID *int64
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_label, github_runner_id FROM github_runners WHERE runner_id = $1", id).
		Scan(&label, &githubID); err != nil || label != "sandrun-linux" || githubID != nil {
		t.Fatalf("pending registration label = %q, ID = %v, error = %v", label, githubID, err)
	}
	// The first call is already gated. Let replacement runners register.
	w.gh.Update(func() { w.gh.JITGate = nil })
	renameGitHubProfile(t, w, profileID)
	w.send(t, "workflow_job", job("queued", 501, 2, []string{renamedLabel}, ""))
	eventually(t, "deletion of the old-label runner while JIT is pending", func() bool {
		return w.runner(t, id).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
	})
	if r := w.runner(t, id); r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP {
		t.Fatalf("old runner end reason = %v", r.GetEndReason())
	}
	assertRenamedCapacity(t, w, id)
	releaseJIT()

	eventually(t, "recording and removal of the late GitHub registration", func() bool {
		var removed bool
		if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_runner_id IS NOT NULL AND registration_removed FROM github_runners WHERE runner_id = $1", id).
			Scan(&removed); err != nil {
			t.Fatal(err)
		}
		return removed
	})
	if _, ok := w.gh.RunnerByName("sandrun-" + id); ok {
		t.Fatal("late GitHub registration was retained")
	}
	w.progs.mu.Lock()
	started := w.progs.running[preparing[0].GetOwnsandSandboxId()] != nil
	w.progs.mu.Unlock()
	if started {
		t.Fatal("the deleted runner started its program")
	}
	t.Log("The queue received new-label capacity while old-label JIT was held. Cleanup removed the late registration before guest startup.")
}

func TestRenamedProfileKeepsBusyAndOtherPool(t *testing.T) {
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.link(t, 502, "beta")
	profileID := w.createProfile(t, ghProfile("linux", 0))
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	w.send(t, "workflow_job", job("queued", 502, 2, sandRunLinux, ""))
	eventually(t, "one runner in each pool", func() bool {
		return len(w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)) == 2
	})
	var busy, other *sandrunv1.Runner
	for _, r := range w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK) {
		if r.GetGithubInstallationId() == 501 {
			busy = r
		} else if r.GetGithubInstallationId() == 502 {
			other = r
		}
	}
	if busy == nil || other == nil {
		t.Fatal("the two installations did not get separate runners")
	}
	w.gh.SetBusy("sandrun-"+busy.GetRunnerId(), true)
	renameGitHubProfile(t, w, profileID)
	w.send(t, "workflow_job", job("queued", 501, 3, []string{renamedLabel}, ""))
	w.l.GitHub.Wait()
	assertRenamedCapacity(t, w, busy.GetRunnerId())
	for _, r := range []*sandrunv1.Runner{busy, other} {
		if got := w.runner(t, r.GetRunnerId()); got.GetState() != sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK {
			t.Fatalf("runner in installation %d changed: %v", r.GetGithubInstallationId(), got.GetState())
		}
	}
	assertRegistrationLabel(t, w, busy.GetRunnerId(), "sandrun-linux", "acme")
	assertRegistrationLabel(t, w, other.GetRunnerId(), "sandrun-linux", "beta")
	if w.os.Calls("DeleteSandbox") != 0 || !strings.Contains(w.env.Logs.String(), "github_runner_removal_refused") {
		t.Fatal("rename cleanup did not preserve both sandboxes after GitHub refused the busy deletion")
	}
	t.Log("GitHub refused to remove the busy registration. Both its sandbox and the other installation's runner remained.")
}

func renameGitHubProfile(t *testing.T, w *ghWorld, id string) {
	t.Helper()
	if _, err := w.env.User(w.org.Admin).Profile.UpdateRunnerProfile(t.Context(), connect.NewRequest(&sandrunv1.UpdateRunnerProfileRequest{
		OrganizationId: w.org.ID, RunnerProfileId: id, Profile: &sandrunv1.RunnerProfile{Name: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	})); err != nil {
		t.Fatal(err)
	}
}

func assertRenamedCapacity(t *testing.T, w *ghWorld, oldID string) {
	t.Helper()
	var id string
	eventually(t, "a replacement waiting under the new label in the first pool", func() bool {
		for _, r := range w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK) {
			if r.GetGithubInstallationId() == 501 && r.GetRunnerId() != oldID {
				id = r.GetRunnerId()
				return true
			}
		}
		return false
	})
	assertRegistrationLabel(t, w, id, renamedLabel, "acme")
}

func assertRegistrationLabel(t *testing.T, w *ghWorld, id, want, org string) {
	t.Helper()
	var stored string
	if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT github_label FROM github_runners WHERE runner_id = $1", id).
		Scan(&stored); err != nil || stored != want {
		t.Fatalf("stored label = %q, want %q, error = %v", stored, want, err)
	}
	if reg, ok := w.gh.RunnerByName("sandrun-" + id); !ok || reg.Org != org || !slices.Equal(reg.Labels, []string{"self-hosted", want}) {
		t.Fatalf("registration present = %v, organization = %q, labels = %v, want %q and [self-hosted %s]", ok, reg.Org, reg.Labels, org, want)
	}
}
