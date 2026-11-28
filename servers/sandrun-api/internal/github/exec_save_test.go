// SPDX-License-Identifier: FSL-1.1-ALv2

package github_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestRunnerExecIDSaveRetry(t *testing.T) {
	t.Parallel()
	const (
		preparing = sandrunv1.RunnerState_RUNNER_STATE_PREPARING
		waiting   = sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
		busy      = sandrunv1.RunnerState_RUNNER_STATE_BUSY
	)
	for _, tc := range []struct {
		name         string
		stateAtSave  sandrunv1.RunnerState
		progressWhen sandrunv1.RunnerState
	}{
		{name: "preparing", stateAtSave: waiting, progressWhen: waiting},
		{name: "busy", stateAtSave: busy, progressWhen: busy},
		{name: "webhook during retry", stateAtSave: busy, progressWhen: preparing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newExecSaveScenario(t)
			if tc.progressWhen == busy {
				s.progress(t)
			}
			s.failSave(t, tc.progressWhen == preparing, false)
			if tc.progressWhen == preparing {
				s.progress(t)
			}
			eventually(t, "the original exec ID saved", func() bool {
				if s.launches.Load() > 1 {
					t.Fatal("the driver launched a second runner program after the failed save")
				}
				return s.runner(t, s.runnerID).GetState() == tc.stateAtSave && s.storedID(t) == s.execID
			})
			s.assertOneLaunch(t)
			if tc.progressWhen == waiting {
				s.progress(t)
			}
			s.assertAttached(t, s.execID)
			if tc.progressWhen == busy {
				// The next lease holder can attach using the saved ID.
				s.env.Stop()
				l := &layers.Layers{}
				opts := layers.Options{OwnSand: s.os, GitHub: s.githubOptions(s.env.Clock.Now)}
				s.env = s.env.NewReplica(t, testenv.WithLayerOptions(opts, l))
				s.l = l
				s.gh.Target, s.gh.TargetClient = s.env.Server.URL+"/webhooks/github", s.env.Server.Client()
				s.assertAttached(t, s.execID)
			}
			s.progs.exit(s.sandboxID)
			eventually(t, "deleted after the original exec exits", func() bool {
				return s.runner(t, s.runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
			})
			if r := s.runner(t, s.runnerID); r.GetEndReason() != sandrunv1.RunnerEndReason_RUNNER_END_REASON_WORK_DONE ||
				s.os.Active(s.osOrg) != 0 || s.requestOfJob(t, 1).GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
				t.Fatal("the original exec exit did not retire only the runner")
			}
			s.send(t, "workflow_job", job("completed", 501, 1, sandRunLinux, s.runnerName))
			if req := s.requestOfJob(t, 1); req.GetState() != sandrunv1.RequestState_REQUEST_STATE_ENDED ||
				req.GetEndReason() != sandrunv1.RequestEndReason_REQUEST_END_REASON_COMPLETED {
				t.Fatalf("request after completed webhook: %v", req)
			}
			t.Log("PostgreSQL rejected the first save. The same runner saved and attached to the original ID, and GitHub completed the request after sandbox cleanup.")
		})
	}
}

func TestRunnerExecIDSaveGuards(t *testing.T) {
	t.Parallel()
	t.Run("deleted runner", func(t *testing.T) {
		s := newExecSaveScenario(t)
		s.progress(t)
		s.failSave(t, false, true)
		_, err := s.env.User(s.org.Admin).Work.DeleteRunner(t.Context(), connect.NewRequest(&sandrunv1.DeleteRunnerRequest{
			OrganizationId: s.org.ID, RunnerId: s.runnerID,
		}))
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "runner deletion", func() bool {
			return s.runner(t, s.runnerID).GetState() == sandrunv1.RunnerState_RUNNER_STATE_DELETED
		})
		stays(t, "the user deletion and empty exec ID", func() bool {
			return s.runner(t, s.runnerID).GetEndReason() == sandrunv1.RunnerEndReason_RUNNER_END_REASON_DELETED_BY_USER &&
				s.storedID(t) == "" && s.os.Calls("AttachExec") == 0 && s.launches.Load() == 1
		})
		t.Log("User deletion won while the exec ID write was failing.")
	})
	t.Run("already saved ID", func(t *testing.T) {
		s := newExecSaveScenario(t)
		s.progress(t)
		s.failSave(t, false, true)
		saved := api.NewID()
		if _, err := s.env.DB.Pool.Exec(t.Context(), "UPDATE runners SET program_exec_id = $2 WHERE runner_id = $1", s.runnerID, saved); err != nil {
			t.Fatal(err)
		}
		s.assertAttached(t, saved)
		if got := s.storedID(t); got != saved {
			t.Fatalf("stored exec ID = %s, want %s", got, saved)
		}
		s.assertOneLaunch(t)
		t.Log("The driver attached with the ID already stored in PostgreSQL.")
	})
	t.Run("driver canceled", func(t *testing.T) {
		s := newExecSaveScenario(t)
		s.progress(t)
		s.failSave(t, false, true)
		s.env.Stop()
		attempts := func() int64 {
			var count int64
			if err := s.env.DB.Pool.QueryRow(t.Context(), "SELECT last_value FROM exec_save_attempts").Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count
		}
		stoppedAt := attempts()
		stays(t, "no more writes after driver cancellation", func() bool {
			return attempts() == stoppedAt && s.launches.Load() == 1 && s.storedID(t) == ""
		})
		t.Log("Stopping the SandRun instance ended the retries without another launch.")
	})
}

type execSaveScenario struct {
	*ghWorld
	runnerID, runnerName, sandboxID, execID string
	registration                            int64
	launches                                atomic.Int32
	release                                 func()
	attached                                chan string
}

func newExecSaveScenario(t *testing.T) *execSaveScenario {
	t.Helper()
	w := newGHWorld(t, worldOpts{})
	w.link(t, 501, "acme")
	w.createProfile(t, ghProfile("linux", 0))
	gate, started := make(chan struct{}), make(chan string, 1)
	s := &execSaveScenario{ghWorld: w, release: sync.OnceFunc(func() { close(gate) }), attached: make(chan string, 2)}
	t.Cleanup(s.release)
	w.progs.hold(gate, nil)
	w.os.SetExec(func(ctx context.Context, sandboxID string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
		if strings.Contains(strings.Join(p.Command, " "), "--jitconfig") && s.launches.Add(1) == 1 {
			started <- p.ExecID
		}
		return w.progs.exec(ctx, sandboxID, p)
	}, func(ctx context.Context, sandboxID, execID string, p ownsand.AttachParams) (*ownsand.ExecResult, error) {
		select {
		case s.attached <- execID:
		default:
		}
		return w.progs.attach(ctx, sandboxID, execID, p)
	})
	w.send(t, "workflow_job", job("queued", 501, 1, sandRunLinux, ""))
	select {
	case s.execID = <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the original runner program")
	}
	r := w.runnersIn(t, sandrunv1.RunnerState_RUNNER_STATE_PREPARING)
	if len(r) != 1 {
		t.Fatalf("got %d preparing runners, want 1", len(r))
	}
	s.runnerID, s.sandboxID = r[0].GetRunnerId(), r[0].GetOwnsandSandboxId()
	s.runnerName = "sandrun-" + s.runnerID
	reg, ok := w.gh.RunnerByName(s.runnerName)
	if !ok {
		t.Fatal("the original GitHub registration is missing")
	}
	s.registration = reg.ID
	return s
}

func (s *execSaveScenario) failSave(t *testing.T, whilePreparing, always bool) {
	t.Helper()
	const failure = "injected exec ID save commit failure"
	// nextval is visible across rollbacks. Reject the first COMMIT and
	// optionally hold later saves until the state changes or the driver stops.
	if _, err := s.env.DB.Pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE SEQUENCE exec_save_attempts;
		CREATE FUNCTION fail_exec_save() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('exec_save_attempts') = 1 OR (%t AND OLD.state = %d) OR %t THEN
				RAISE EXCEPTION '%s' USING ERRCODE = '40001';
			END IF;
			RETURN NULL;
		END;
		$$;
		CREATE CONSTRAINT TRIGGER fail_exec_save AFTER UPDATE ON runners DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW WHEN (NEW.runner_id = '%s' AND NEW.program_exec_id = '%s' AND OLD.program_exec_id IS DISTINCT FROM NEW.program_exec_id)
		EXECUTE FUNCTION fail_exec_save();`, whilePreparing, sandrunv1.RunnerState_RUNNER_STATE_PREPARING, always, failure, s.runnerID, s.execID)); err != nil {
		t.Fatal(err)
	}
	if id := s.storedID(t); id != "" {
		t.Fatalf("exec ID already stored before Start returns: %s", id)
	}
	s.release()
	eventually(t, "PostgreSQL rejecting the first exec ID save", func() bool {
		return strings.Contains(s.env.Logs.String(), failure)
	})
}

func (s *execSaveScenario) progress(t *testing.T) {
	t.Helper()
	s.send(t, "workflow_job", job("in_progress", 501, 1, sandRunLinux, s.runnerName))
	if req := s.requestOfJob(t, 1); req.GetRunnerId() != s.runnerID ||
		req.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING ||
		s.runner(t, s.runnerID).GetState() != sandrunv1.RunnerState_RUNNER_STATE_BUSY {
		t.Fatalf("in_progress did not bind the original runner: %v", req)
	}
}

func (s *execSaveScenario) storedID(t *testing.T) string {
	t.Helper()
	var id string
	if err := s.env.DB.Pool.QueryRow(t.Context(), "SELECT COALESCE(program_exec_id::text, '') FROM runners WHERE runner_id = $1", s.runnerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *execSaveScenario) assertOneLaunch(t *testing.T) {
	t.Helper()
	reg, ok := s.gh.RunnerByName(s.runnerName)
	if !ok || reg.ID != s.registration || s.gh.Calls("generate_jitconfig") != 1 || s.launches.Load() != 1 {
		t.Fatalf("runner program launches = %d, registrations = %d, original registration present = %t",
			s.launches.Load(), s.gh.Calls("generate_jitconfig"), ok && reg.ID == s.registration)
	}
}

func (s *execSaveScenario) assertAttached(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-s.attached:
		if got != want {
			t.Fatalf("AttachExec ID = %s, want %s", got, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for AttachExec")
	}
}
