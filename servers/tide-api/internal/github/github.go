// SPDX-License-Identifier: FSL-1.1-ALv2

// Package github is the GitHub App layer of tide-api: GitHubService (the
// installation links), POST /webhooks/github (workflow_job and installation
// events), the GITHUB_JOB workload adapter (the GitHub Actions runner in the
// guest with a JIT configuration) and the lease component that requests
// redeliveries of failed webhook deliveries. The owning contract is the
// GitHub App integration, Webhooks, Runner lifecycle and Runner
// installation sections of docs/servers-tide-api-foundation.md.
//
// A runner pool is one pair of a GitHub installation and a GITHUB_JOB
// runner profile. The pool key of requests and runners
// (work.Request.PoolKey, work.Runner.PoolKey) is the GitHub numeric
// installation ID in decimal.
//
// Without a GitHub App configuration (config.GitHubApp), the layer still
// registers its service, endpoint and adapter: the RPCs that need GitHub
// fail with failed_precondition, the webhook answers 503, and no runner
// starts, so development setups without a GitHub App work.
package github

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/jobs"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// Timing holds the intervals of the layer.
type Timing struct {
	// RedeliveryInterval is the interval of the redelivery job (5 minutes).
	RedeliveryInterval time.Duration
	// RedeliveryWindow is how far back the job looks (24 hours).
	RedeliveryWindow time.Duration
	// PrepareTimeout bounds the runner installation exec.
	PrepareTimeout time.Duration
	// RegistrationGiveUp is how long a DELETING runner waits for its GitHub
	// registration removal before its sandbox is deleted anyway.
	RegistrationGiveUp time.Duration
}

// DefaultTiming returns the production intervals.
func DefaultTiming() Timing {
	return Timing{RedeliveryInterval: 5 * time.Minute, RedeliveryWindow: 24 * time.Hour, PrepareTimeout: 15 * time.Minute, RegistrationGiveUp: 10 * time.Minute}
}

// Options configures the layer.
type Options struct {
	// Client calls GitHub as the app; nil turns the layer off.
	Client *githubapi.Client
	// WebhookSecret is the app webhook secret.
	WebhookSecret string
	Timing        Timing
}

// installStateLifetime is the validity of a CreateGitHubInstallUrl state.
const installStateLifetime = 10 * time.Minute

// Layer is the wired GitHub layer.
type Layer struct {
	svc      *api.Service
	engine   *work.Engine
	profiles *profile.Profiles
	m        *metrics.Metrics
	gh       *githubapi.Client
	secret   []byte
	timing   Timing

	// bg tracks the surplus removals that run after a webhook answer.
	bg sync.WaitGroup
}

// Register wires the layer: GitHubService, POST /webhooks/github, the
// GITHUB_JOB adapter, the redelivery lease component and the daily
// deletion of expired installation states.
func Register(r *app.Registry, engine *work.Engine, m *metrics.Metrics, opts Options) *Layer {
	if opts.Timing.RedeliveryInterval == 0 {
		opts.Timing = DefaultTiming()
	}
	l := &Layer{svc: r.Service, engine: engine, profiles: engine.Profiles(), m: m, gh: opts.Client, secret: []byte(opts.WebhookSecret), timing: opts.Timing}
	if l.gh == nil {
		l.secret = nil
	}
	r.Handle(tidev1connect.NewGitHubServiceHandler(&Handler{l: l}, r.HandlerOptions()...))
	r.Handle("POST /webhooks/github", WebhookHandler{l: l})
	engine.RegisterAdapter(&Adapter{l: l})
	r.AddDailyJob(jobs.SQLJob(r.Service.DB, "github_install_states", "DELETE FROM github_install_states WHERE expire_time < $1::timestamptz"))
	if l.gh != nil {
		r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "github_redelivery", RunFunc: l.runRedelivery})
	}
	return l
}

// Configured reports whether the GitHub App is configured.
func (l *Layer) Configured() bool { return l.gh != nil }

// Wait waits for the surplus removals that webhooks started (tests).
func (l *Layer) Wait() { l.bg.Wait() }

var errNotConfigured = api.ErrPrecondition("the GitHub App is not configured on this server")

// Installation is a stored installation link.
type Installation struct {
	ID              string
	OrganizationID  string
	GitHubID        int64
	Login           string
	AccountID       int64
	State           tidev1.GitHubInstallationState
	RunnerGroupID   int64
	RunnerGroupName string
	CreateTime      time.Time
}

// PoolKey is the runner pool key of the installation.
func (i *Installation) PoolKey() string { return strconv.FormatInt(i.GitHubID, 10) }

// Active reports an ACTIVE installation.
func (i *Installation) Active() bool {
	return i.State == tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE
}

func (i *Installation) proto() *tidev1.GitHubInstallation {
	return &tidev1.GitHubInstallation{
		InstallationId: i.ID, OrganizationId: i.OrganizationID, GithubInstallationId: i.GitHubID, GithubAccountLogin: i.Login,
		GithubAccountId: i.AccountID, State: i.State, GithubRunnerGroupId: i.RunnerGroupID, RunnerGroupName: i.RunnerGroupName,
		CreateTime: api.Timestamp(i.CreateTime),
	}
}

const installationColumns = `installation_id::text, organization_id::text, github_installation_id, github_account_login, github_account_id, state,
	github_runner_group_id, runner_group_name, create_time`

func scanInstallation(row pgx.Row) (*Installation, error) {
	var i Installation
	var state int16
	if err := row.Scan(&i.ID, &i.OrganizationID, &i.GitHubID, &i.Login, &i.AccountID, &state, &i.RunnerGroupID, &i.RunnerGroupName, &i.CreateTime); err != nil {
		return nil, err
	}
	i.State = tidev1.GitHubInstallationState(state)
	return &i, nil
}

// installation loads an installation link of an organization by its tide ID
// (not_found otherwise).
func (l *Layer) installation(ctx context.Context, q db.Querier, orgID, id string) (*Installation, error) {
	i, err := scanInstallation(q.QueryRow(ctx, "SELECT "+installationColumns+" FROM github_installations WHERE organization_id = $1 AND installation_id = $2", orgID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, api.ErrNotFound("GitHub installation")
	}
	return i, err
}

// rowLock is the row lock that byGitHubID takes.
type rowLock int

const (
	// lockNone reads the row without a lock.
	lockNone rowLock = iota
	// lockShare lets workflow_job deliveries and JIT registrations share the
	// link while excluding changes to it.
	lockShare
	// lockUpdate serializes a change of the link (an installation event or
	// an RPC) with every other lock of the row.
	lockUpdate
)

// byGitHubID loads the link of a GitHub installation ID, or nil, with the
// row lock lock, so a webhook and an RPC on the same installation
// serialize.
func (l *Layer) byGitHubID(ctx context.Context, q db.Querier, githubID int64, lock rowLock) (*Installation, error) {
	sql := "SELECT " + installationColumns + " FROM github_installations WHERE github_installation_id = $1"
	switch lock {
	case lockShare:
		sql += " FOR SHARE"
	case lockUpdate:
		sql += " FOR UPDATE"
	}
	i, err := scanInstallation(q.QueryRow(ctx, sql, githubID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return i, err
}

// poolInstallation loads the installation of a runner pool of an
// organization, or nil.
func (l *Layer) poolInstallation(ctx context.Context, q db.Querier, orgID, poolKey string) (*Installation, error) {
	id, err := strconv.ParseInt(poolKey, 10, 64)
	if err != nil {
		return nil, nil
	}
	i, err := l.byGitHubID(ctx, q, id, lockNone)
	if err != nil || i == nil || i.OrganizationID != orgID {
		return nil, err
	}
	return i, nil
}

// deactivateTx ends the work of an installation's runner pools in tx: its
// QUEUED requests end with CANCELED and INTEGRATION_REMOVED, its STARTING
// and RUNNING requests with ENDED and INTEGRATION_REMOVED (tide gets no
// further events for them), and its runners that wait for a job are
// deleted (CLEAN_UP). Busy runners finish their job. skip lists runners to
// keep (GitHub refused their removal because they took a job). It returns
// the deleted runners for RunnerChanged after the commit.
func (l *Layer) deactivateTx(ctx context.Context, tx pgx.Tx, inst *Installation, skip map[string]bool) ([]string, error) {
	pool := inst.PoolKey()
	rows, err := tx.Query(ctx, "SELECT request_id::text, state FROM requests WHERE organization_id = $1 AND kind = $2 AND pool_key = $3 AND state = ANY($4)",
		inst.OrganizationID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB), pool, work.OpenRequestStates())
	if err != nil {
		return nil, err
	}
	type open struct {
		id    string
		state tidev1.RequestState
	}
	var reqs []open
	for rows.Next() {
		var o open
		var state int16
		if err := rows.Scan(&o.id, &state); err != nil {
			rows.Close()
			return nil, err
		}
		o.state = tidev1.RequestState(state)
		reqs = append(reqs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, o := range reqs {
		end := work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_INTEGRATION_REMOVED}
		if o.state == tidev1.RequestState_REQUEST_STATE_QUEUED {
			end.State = tidev1.RequestState_REQUEST_STATE_CANCELED
		}
		if _, err := l.engine.EndRequestTx(ctx, tx, o.id, end); err != nil {
			return nil, err
		}
	}
	idle, err := l.idleRunners(ctx, tx, inst.OrganizationID, pool)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, r := range idle {
		if skip[r.ID] {
			continue
		}
		d, err := l.engine.DeleteIdleRunnerTx(ctx, tx, inst.OrganizationID, r.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_CLEAN_UP)
		if err != nil {
			return nil, err
		}
		if d != nil {
			deleted = append(deleted, d.ID)
		}
	}
	return deleted, nil
}

// idleRunners returns the GITHUB_JOB runners of a pool that wait for a job
// (CREATING, PREPARING, WARM or WAITING_FOR_WORK without a request).
func (l *Layer) idleRunners(ctx context.Context, q db.Querier, orgID, pool string) ([]*work.Runner, error) {
	rows, err := q.Query(ctx, "SELECT "+work.RunnerColumns+` FROM runners WHERE organization_id = $1 AND kind = $2 AND pool_key = $3 AND request_id IS NULL
		AND state = ANY($4) ORDER BY create_time DESC, runner_id DESC`, orgID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB), pool,
		[]int16{int16(tidev1.RunnerState_RUNNER_STATE_CREATING), int16(tidev1.RunnerState_RUNNER_STATE_PREPARING), int16(tidev1.RunnerState_RUNNER_STATE_WARM),
			int16(tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK)})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*work.Runner
	for rows.Next() {
		r, err := work.ScanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// registration is the GitHub runner registration of a runner.
type registration struct {
	githubInstallationID int64
	githubRunnerID       int64 // 0 before the JIT configuration exists
	removed              bool
}

func (l *Layer) registration(ctx context.Context, q db.Querier, runnerID string) (*registration, error) {
	var reg registration
	var id *int64
	err := q.QueryRow(ctx, "SELECT github_installation_id, github_runner_id, registration_removed FROM github_runners WHERE runner_id = $1", runnerID).
		Scan(&reg.githubInstallationID, &id, &reg.removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if id != nil {
		reg.githubRunnerID = *id
	}
	return &reg, nil
}

// removeRegistration removes the GitHub registration of a runner of inst.
// It reports refused=true when GitHub refused because the runner took a
// job.
func (l *Layer) removeRegistration(ctx context.Context, q db.Querier, inst *Installation, runnerID string) (bool, error) {
	reg, err := l.registration(ctx, q, runnerID)
	if err != nil || reg == nil || reg.removed || reg.githubRunnerID == 0 {
		return false, err
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = l.gh.DeleteRunner(deleteCtx, inst.GitHubID, inst.Login, reg.githubRunnerID)
	if errors.Is(err, githubapi.ErrRunnerRefused) {
		l.svc.Logger.InfoContext(ctx, "github_runner_removal_refused", slog.String("organization_id", inst.OrganizationID), slog.String("runner_id", runnerID),
			slog.Int64("github_runner_id", reg.githubRunnerID))
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := q.Exec(ctx, "UPDATE github_runners SET registration_removed = true WHERE runner_id = $1 AND github_runner_id = $2", runnerID, reg.githubRunnerID); err != nil {
		l.svc.Logger.WarnContext(ctx, "github_registration_update_failed", slog.String("runner_id", runnerID), slog.Any("error", err))
		return false, err
	}
	l.svc.Logger.InfoContext(ctx, "github_runner_removed", slog.String("organization_id", inst.OrganizationID), slog.String("runner_id", runnerID),
		slog.Int64("github_runner_id", reg.githubRunnerID))
	return false, nil
}
