// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// jitFileMode lets only the owner (the user of the guest exec that starts
// the runner) read the JIT configuration file. Windows guests ignore modes;
// the installation restricts the runner directory there.
const jitFileMode = 0o600

// Adapter is the GITHUB_JOB adapter of the work engine. The dispatcher
// starts runners per runner pool; GitHub assigns jobs to them, and the
// workflow_job events bind a job's request to the runner that took it.
type Adapter struct {
	work.BaseAdapter
	l *Layer
}

var (
	_ work.Adapter             = (*Adapter)(nil)
	_ work.RegistrationRemover = (*Adapter)(nil)
	_ work.WarmPooler          = (*Adapter)(nil)
	_ work.WarmReclaimer       = (*Adapter)(nil)
	_ work.RequestDecorator    = (*Adapter)(nil)
)

func (*Adapter) Kind() tidev1.WorkloadKind { return tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB }

func (*Adapter) BindsAtDispatch() bool { return false }

// Prepare checks for the GitHub Actions runner in the guest and installs
// the pinned version from the official release archive, with checksum
// verification, when it is missing (InstallCommand).
func (a *Adapter) Prepare(ctx context.Context, g *work.Guest) error {
	if !a.l.Configured() {
		return errors.New("the GitHub App is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, a.l.timing.PrepareTimeout)
	defer cancel()
	res, err := g.Exec(ctx, ownsand.ExecParams{ExecID: api.NewID(), Command: InstallCommand(g.Runner.GuestOS), Timeout: a.l.timing.PrepareTimeout})
	if err != nil {
		return err
	}
	if res.Kind != ownsand.ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 {
		code := int64(-1)
		if res.ExitCode != nil {
			code = *res.ExitCode
		}
		return fmt.Errorf("the GitHub Actions runner installation failed (%s, exit code %d)", res.Kind, code)
	}
	return nil
}

// errStarted ends the start exec stream after the started event; the
// runner program keeps running and the engine follows it with AttachExec.
var errStarted = errors.New("runner program started")

// Start creates the JIT configuration right before the start (it is never
// stored), writes it to a guest file that only the runner process reads and
// starts the runner with StartCommand.
func (a *Adapter) Start(ctx context.Context, g *work.Guest) (work.StartResult, error) {
	l := a.l
	r := g.Runner
	if !l.Configured() {
		return work.StartResult{}, errors.New("the GitHub App is not configured")
	}
	if g.Profile == nil {
		return work.StartResult{}, errors.New("the runner profile was deleted")
	}
	jit, err := l.startRegistration(ctx, r, g.Profile.GitHubLabel())
	if err != nil {
		return work.StartResult{}, err
	}
	cfg := []byte(jit.EncodedJITConfig)
	jit.EncodedJITConfig = ""
	fw, err := g.Client.OpenFileWrite(r.SandboxID, ownsand.FileWriteParams{WriteID: api.NewID(), Path: JITFilePath(r.GuestOS)})
	if err != nil {
		return work.StartResult{}, err
	}
	_, err = fw.Finish(ctx, 0, cfg, int64(len(cfg)), jitFileMode)
	clear(cfg)
	if err != nil {
		return work.StartResult{}, fmt.Errorf("write the JIT configuration file: %w", err)
	}
	execID := api.NewID()
	res, err := g.Exec(ctx, ownsand.ExecParams{ExecID: execID, Command: StartCommand(r.GuestOS), OnEvent: func(ev ownsand.ExecEvent) error {
		if ev.Kind == ownsand.ExecEventStarted {
			return errStarted
		}
		return nil
	}})
	switch {
	case errors.Is(err, errStarted):
	case err != nil:
		return work.StartResult{}, err
	case res.Kind == ownsand.ExecResultFailed:
		return work.StartResult{}, fmt.Errorf("the runner program did not start (%s)", res.FailureReason)
	}
	// An exec that already exited is followed too: AttachExec returns its
	// result, and the engine deletes the runner.
	return work.StartResult{ProgramExecID: execID}, nil
}

// errDeletedWhileStarting fails a start whose runner was deleted (for
// example as a surplus runner) before its registration was recorded.
var errDeletedWhileStarting = errors.New("the runner was deleted while it started")

// startRegistration holds a share lock on the installation until it publishes
// the runner ID. Unlink holds the exclusive lock before reading runner IDs.
func (l *Layer) startRegistration(ctx context.Context, r *work.Runner, label string) (*githubapi.JITConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	inst, err := l.poolInstallation(ctx, l.svc.DB.Pool, r.OrganizationID, r.PoolKey)
	if err != nil {
		return nil, err
	}
	unavailable := errors.New("the GitHub installation of the runner pool is not linked or not active")
	if inst == nil || !inst.Active() {
		return nil, unavailable
	}
	// Keep the placeholder's foreign-key locks out of the JIT transaction.
	// A retry must keep the previous ID visible until its replacement commits.
	if _, err := l.svc.DB.Pool.Exec(ctx, `INSERT INTO github_runners (runner_id, organization_id, github_installation_id, github_label) VALUES ($1, $2, $3, $4)
		ON CONFLICT (runner_id) DO NOTHING`, r.ID, r.OrganizationID, inst.GitHubID, label); err != nil {
		return nil, err
	}

	var jit *githubapi.JITConfig
	var starting bool
	err = l.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		cur, err := l.byGitHubID(ctx, tx, inst.GitHubID, lockShare)
		if err != nil {
			return err
		}
		if cur == nil || cur.ID != inst.ID || !cur.Active() {
			return unavailable
		}
		inst = cur

		// A previous start may have registered this runner before a lease handover.
		prev, err := l.registration(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if prev != nil && !prev.removed && prev.githubRunnerID != 0 {
			if err := l.gh.DeleteRunner(ctx, inst.GitHubID, inst.Login, prev.githubRunnerID); err != nil {
				return fmt.Errorf("remove the previous registration: %w", err)
			}
		}
		jit, err = l.generateJITConfig(ctx, inst, r.ID, label)
		if err != nil {
			return err
		}

		// A savepoint lets us undo a failed write and remove the GitHub
		// registration before releasing the installation lock.
		err = pgx.BeginFunc(ctx, tx, func(tx pgx.Tx) error {
			var writeErr error
			starting, writeErr = recordRegistration(ctx, tx, r.ID, label, jit.Runner.ID)
			return writeErr
		})
		if err != nil {
			l.rollbackRegistration(ctx, inst, r.ID, jit.Runner.ID)
			jit = nil
			return err
		}
		if !starting {
			// A surplus removal can delete the runner before its ID is stored.
			// Preserve the ID so a later cleanup can retry a failed GitHub call.
			if _, err := l.removeRegistration(ctx, tx, inst, r.ID); err != nil {
				l.svc.Logger.WarnContext(ctx, "github_runner_removal_failed", slog.String("organization_id", inst.OrganizationID), slog.String("runner_id", r.ID), slog.Any("error", err))
			}
		}
		return nil
	})
	if err != nil {
		if jit != nil && registrationCommitRolledBack(err) {
			l.rollbackRegistration(ctx, inst, r.ID, jit.Runner.ID)
		}
		return nil, err
	}
	if !starting {
		return nil, errDeletedWhileStarting
	}
	l.svc.Logger.InfoContext(ctx, "github_runner_registered", slog.String("organization_id", r.OrganizationID), slog.String("runner_id", r.ID),
		slog.Int64("github_installation_id", inst.GitHubID), slog.Int64("github_runner_id", jit.Runner.ID), slog.Int64("github_runner_group_id", inst.RunnerGroupID))
	return jit, nil
}

func (l *Layer) generateJITConfig(ctx context.Context, inst *Installation, runnerID, label string) (*githubapi.JITConfig, error) {
	name := RunnerNamePrefix + runnerID
	labels := []string{label}
	jit, err := l.gh.GenerateJITConfig(ctx, inst.GitHubID, inst.Login, name, inst.RunnerGroupID, labels)
	if !errors.Is(err, githubapi.ErrRunnerNameTaken) {
		return jit, err
	}
	// GitHub does not document why it returns 409. A previous registration
	// with the same name can cause it. Remove that registration and retry once.
	found, err := l.gh.FindRunner(ctx, inst.GitHubID, inst.Login, name)
	if err != nil {
		return nil, err
	}
	if found != nil {
		if err := l.gh.DeleteRunner(ctx, inst.GitHubID, inst.Login, found.ID); err != nil {
			return nil, err
		}
	}
	return l.gh.GenerateJITConfig(ctx, inst.GitHubID, inst.Login, name, inst.RunnerGroupID, labels)
}

// recordRegistration locks the runner row in the same order as surplus
// removal. If deletion has already started, the caller removes the late
// registration and skips starting the runner program.
func recordRegistration(ctx context.Context, tx pgx.Tx, runnerID, label string, githubRunnerID int64) (bool, error) {
	var state int16
	if err := tx.QueryRow(ctx, "SELECT state FROM runners WHERE runner_id = $1 FOR UPDATE", runnerID).Scan(&state); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE github_runners SET github_runner_id = $2, github_label = $3, registration_removed = false WHERE runner_id = $1",
		runnerID, githubRunnerID, label); err != nil {
		return false, err
	}
	s := tidev1.RunnerState(state)
	return s == tidev1.RunnerState_RUNNER_STATE_PREPARING || s == tidev1.RunnerState_RUNNER_STATE_WARM, nil
}

func (l *Layer) rollbackRegistration(ctx context.Context, inst *Installation, runnerID string, githubRunnerID int64) {
	// Normal sandbox cleanup cannot look up a runner ID that did not commit.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := l.gh.DeleteRunner(cleanupCtx, inst.GitHubID, inst.Login, githubRunnerID); err != nil {
		l.svc.Logger.WarnContext(cleanupCtx, "github_runner_removal_failed",
			slog.String("organization_id", inst.OrganizationID), slog.String("runner_id", runnerID),
			slog.Int64("github_installation_id", inst.GitHubID), slog.Int64("github_runner_id", githubRunnerID), slog.Any("error", err))
	}
}

func registrationCommitRolledBack(err error) bool {
	if errors.Is(err, pgx.ErrTxCommitRollback) || pgconn.SafeToRetry(err) {
		return true
	}
	// A PostgreSQL ERROR rejects COMMIT. Connection errors and an explicit
	// statement_completion_unknown can leave the outcome uncertain.
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.SeverityUnlocalized == "ERROR" &&
		!strings.HasPrefix(pgErr.Code, "08") && pgErr.Code != "40003"
}

// ProgramExited keeps the request: GitHub ends it with the completed event.
func (*Adapter) ProgramExited(*work.Request, *ownsand.ExecResult) (work.RequestEnd, bool) {
	return work.RequestEnd{}, true
}

// SandboxEnded fails the runner and keeps its request: a GITHUB_JOB request
// ends when GitHub sends the completed event (lifecycle step 6).
func (*Adapter) SandboxEnded(*work.Request, ownsandv1.SandboxEndReason) work.SandboxOutcome {
	return work.SandboxOutcome{RunnerState: tidev1.RunnerState_RUNNER_STATE_FAILED, RunnerReason: tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED, KeepRequest: true}
}

// RunnerDeleted keeps the request of a deleted busy runner: its job fails in
// GitHub, which then sends the completed event.
func (*Adapter) RunnerDeleted(*work.Request, work.DeleteCause) (work.RequestEnd, bool) {
	return work.RequestEnd{}, true
}

// RemoveRegistration removes the GitHub registration of a runner before its
// sandbox is deleted. It gives up (and lets the deletion go on) when the
// installation is gone or inactive (GitHub gives no token then), when
// GitHub refuses because the runner runs a job (the job ends with the
// sandbox), on another 4xx answer, and after RegistrationGiveUp; it
// returns an error only for answers that a retry can fix (5xx, network).
// GitHub removes a JIT runner that never connects by itself.
func (a *Adapter) RemoveRegistration(ctx context.Context, r *work.Runner) error {
	l := a.l
	if !l.Configured() {
		return nil
	}
	inst, err := l.poolInstallation(ctx, l.svc.DB.Pool, r.OrganizationID, r.PoolKey)
	if err != nil {
		return err
	}
	if inst == nil || !inst.Active() {
		return nil
	}
	_, err = l.removeRegistration(ctx, l.svc.DB.Pool, inst, r.ID)
	if err == nil {
		return nil
	}
	if status := githubapi.StatusOf(err); status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return nil
	}
	if r.State == tidev1.RunnerState_RUNNER_STATE_DELETING && l.svc.NowUTC().Sub(r.UpdateTime) > l.timing.RegistrationGiveUp {
		l.svc.Logger.WarnContext(ctx, "github_runner_removal_given_up", slog.String("organization_id", r.OrganizationID), slog.String("runner_id", r.ID))
		return nil
	}
	return err
}

// WarmPools returns the ACTIVE installations of the organization, the
// runner pools of the warm runners of its GITHUB_JOB profiles.
func (a *Adapter) WarmPools(ctx context.Context, orgID string) ([]string, error) {
	if !a.l.Configured() {
		return nil, nil
	}
	rows, err := a.l.svc.DB.Pool.Query(ctx, "SELECT github_installation_id FROM github_installations WHERE organization_id = $1 AND state = $2 ORDER BY github_installation_id",
		orgID, int16(tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out, rows.Err()
}

// ReclaimWarm deletes a warm runner of a pool for the capacity of a request
// of another profile (WARM_RECLAIMED). Like a surplus runner, it first
// loses its GitHub registration, and a runner whose removal GitHub refuses
// took a job and stays.
func (a *Adapter) ReclaimWarm(ctx context.Context, r *work.Runner) (bool, error) {
	l := a.l
	inst, err := l.poolInstallation(ctx, l.svc.DB.Pool, r.OrganizationID, r.PoolKey)
	if err != nil {
		return false, err
	}
	return l.removeIdleRunner(ctx, inst, r.OrganizationID, r.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_WARM_RECLAIMED)
}

// DecorateRequests adds the GitHub job to GITHUB_JOB requests.
func (a *Adapter) DecorateRequests(ctx context.Context, q db.Querier, orgID string, reqs []*tidev1.Request) error {
	ids := make([]string, 0, len(reqs))
	byID := map[string]*tidev1.Request{}
	for _, r := range reqs {
		ids = append(ids, r.GetRequestId())
		byID[r.GetRequestId()] = r
	}
	rows, err := q.Query(ctx, `SELECT request_id::text, github_installation_id, github_job_id, github_workflow_run_id, repository_full_name, job_name
		FROM github_jobs WHERE organization_id = $1 AND request_id = ANY($2::uuid[])`, orgID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		j := &tidev1.GitHubJob{}
		if err := rows.Scan(&id, &j.GithubInstallationId, &j.GithubJobId, &j.GithubWorkflowRunId, &j.RepositoryFullName, &j.JobName); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.GithubJob = j
		}
	}
	return rows.Err()
}
