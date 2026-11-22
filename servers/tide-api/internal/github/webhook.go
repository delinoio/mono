// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
	"github.com/delinoio/mono/servers/tide-api/internal/webhook"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// maxDeliveryIDLength bounds X-GitHub-Delivery (a GUID).
const maxDeliveryIDLength = 100

// surplusTimeout bounds the surplus removal after a completed event.
const surplusTimeout = 2 * time.Minute

// RunnerNamePrefix is the prefix of the GitHub runner name tide-<runner_id>.
const RunnerNamePrefix = "tide-"

// selfHostedLabel is the label that GitHub gives every self-hosted runner.
const selfHostedLabel = "self-hosted"

// WebhookHandler serves POST /webhooks/github.
type WebhookHandler struct{ l *Layer }

// workflowJobEvent is the part of a workflow_job payload that tide reads.
type workflowJobEvent struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	WorkflowJob struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		Name       string   `json:"name"`
		Labels     []string `json:"labels"`
		RunnerName *string  `json:"runner_name"`
		Conclusion *string  `json:"conclusion"`
	} `json:"workflow_job"`
}

// installationEvent is the part of an installation payload that tide reads.
type installationEvent struct {
	Action       string                 `json:"action"`
	Installation githubapi.Installation `json:"installation"`
}

// outcome is what one delivery did, for the answer, the metric and the log.
type outcome struct {
	result         webhook.Result
	event, action  string
	githubInstID   int64
	githubJobID    int64
	organizationID string
	requestID      string
	runnerID       string
	detail         string
	// after runs after the commit.
	after []func(context.Context)
}

func (o *outcome) ignore(detail string) error {
	o.result, o.detail = webhook.ResultIgnored, detail
	return nil
}

// errBadPayload rejects a signed body that does not parse.
var errBadPayload = errors.New("invalid payload")

func (h WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l := h.l
	ctx := r.Context()
	o := &outcome{event: r.Header.Get("X-GitHub-Event")}
	status := http.StatusNoContent
	defer func() {
		l.m.WebhookDelivery(webhook.SenderGitHub.Label(), string(o.result))
		level := slog.LevelInfo
		if status >= 400 {
			level = slog.LevelWarn
		}
		l.svc.Logger.LogAttrs(ctx, level, "github_webhook_delivery", slog.String("delivery_id", safeHeader(r.Header.Get("X-GitHub-Delivery"))),
			slog.String("event", safeHeader(o.event)), slog.String("action", safeHeader(o.action)), slog.String("result", string(o.result)),
			slog.Int("status", status), slog.Int64("github_installation_id", o.githubInstID), slog.Int64("github_job_id", o.githubJobID),
			slog.String("organization_id", o.organizationID), slog.String("request_id", o.requestID), slog.String("runner_id", o.runnerID),
			slog.String("detail", o.detail))
	}()
	answer := func(code int, result webhook.Result) {
		status, o.result = code, result
		w.WriteHeader(code)
	}
	if !l.Configured() {
		answer(http.StatusServiceUnavailable, webhook.ResultNotConfigured)
		return
	}
	body, err := webhook.ReadBody(w, r)
	if errors.Is(err, webhook.ErrTooLarge) {
		answer(http.StatusRequestEntityTooLarge, webhook.ResultTooLarge)
		return
	}
	if err != nil {
		answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
		return
	}
	// The signature is checked before anything of the body is read.
	if !githubapi.VerifySignature(l.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		answer(http.StatusUnauthorized, webhook.ResultInvalidSignature)
		return
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" || len(deliveryID) > maxDeliveryIDLength || safeHeader(deliveryID) != deliveryID {
		answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
		return
	}
	err = l.svc.DB.InTx(ctx, func(tx pgx.Tx) error {
		o.after = nil
		isNew, err := webhook.Record(ctx, tx, webhook.SenderGitHub, deliveryID, l.svc.NowUTC())
		if err != nil {
			return err
		}
		if !isNew {
			o.result = webhook.ResultDuplicate
			return nil
		}
		o.result = webhook.ResultProcessed
		switch o.event {
		case "workflow_job":
			return l.workflowJob(ctx, tx, body, o)
		case "installation":
			return l.installationEvent(ctx, tx, body, o)
		}
		return o.ignore("event not handled")
	})
	if errors.Is(err, errBadPayload) {
		answer(http.StatusBadRequest, webhook.ResultInvalidPayload)
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			l.svc.Logger.ErrorContext(ctx, "github_webhook_failed", slog.String("delivery_id", deliveryID), slog.Any("error", err))
		}
		// Not recorded: the redelivery job, or GitHub, delivers it again.
		answer(http.StatusInternalServerError, webhook.ResultFailed)
		return
	}
	answer(http.StatusNoContent, o.result)
	if len(o.after) == 0 {
		return
	}
	// The follow-up work (kicks, sandbox deletions, surplus removal) runs
	// after the answer, because GitHub counts a delivery that takes more
	// than 10 seconds as failed.
	after, bgCtx := o.after, context.WithoutCancel(ctx)
	l.bg.Add(1)
	go func() {
		defer l.bg.Done()
		for _, fn := range after {
			fn(bgCtx)
		}
	}()
}

// safeHeader keeps printable ASCII of a header for the log, at most 100
// bytes.
func safeHeader(v string) string {
	if len(v) > maxDeliveryIDLength {
		v = v[:maxDeliveryIDLength]
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if c := v[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// MatchProfileName returns the runner profile name of a job's labels: the
// labels must be exactly one label tide-<name>, optionally with
// self-hosted (GitHub compares labels without case). Every other job is
// ignored, so a tide runner never waits for a job whose other labels it
// does not have.
func MatchProfileName(labels []string) (string, bool) {
	name := ""
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == selfHostedLabel {
			continue
		}
		n, ok := strings.CutPrefix(label, RunnerNamePrefix)
		if !ok || n == "" || (name != "" && n != name) {
			return "", false
		}
		name = n
	}
	return name, name != ""
}

// matchProfile returns the GITHUB_JOB profile of an organization that the
// job's labels name, or nil.
func (l *Layer) matchProfile(ctx context.Context, tx pgx.Tx, orgID string, labels []string) (*profile.Profile, error) {
	name, ok := MatchProfileName(labels)
	if !ok {
		return nil, nil
	}
	profiles, err := l.profiles.ListAll(ctx, tx, orgID)
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.Kind == tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB && p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

func (l *Layer) requestOfJob(ctx context.Context, tx pgx.Tx, orgID string, jobID int64) (*work.Request, error) {
	r, err := work.ScanRequest(tx.QueryRow(ctx, "SELECT "+work.RequestColumns+" FROM requests WHERE organization_id = $1 AND source = $2 AND source_key = $3 FOR UPDATE",
		orgID, int16(work.SourceGitHub), strconv.FormatInt(jobID, 10)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// createRequestTx creates the request of a matching job (or returns the
// existing one).
func (l *Layer) createRequestTx(ctx context.Context, tx pgx.Tx, inst *Installation, ev *workflowJobEvent, o *outcome) (*work.Request, error) {
	p, err := l.matchProfile(ctx, tx, inst.OrganizationID, ev.WorkflowJob.Labels)
	if err != nil || p == nil {
		return nil, err
	}
	if err := api.LockOrganizationShare(ctx, tx, inst.OrganizationID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			// The organization is being deleted.
			return nil, nil
		}
		return nil, err
	}
	req, created, err := l.engine.CreateRequest(ctx, tx, work.NewRequest{Profile: p, Source: work.SourceGitHub,
		SourceKey: strconv.FormatInt(ev.WorkflowJob.ID, 10), PoolKey: inst.PoolKey()})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO github_jobs (request_id, organization_id, github_installation_id, github_job_id, github_workflow_run_id,
		repository_full_name, job_name) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		req.ID, inst.OrganizationID, inst.GitHubID, ev.WorkflowJob.ID, ev.WorkflowJob.RunID, ev.Repository.FullName, ev.WorkflowJob.Name); err != nil {
		return nil, err
	}
	if created {
		org := inst.OrganizationID
		o.after = append(o.after, func(context.Context) { l.svc.WorkChanged(org) })
	}
	return req, nil
}

// tideRunner returns the open GITHUB_JOB runner of a pool that a GitHub
// runner name tide-<runner_id> names, or nil.
func (l *Layer) tideRunner(ctx context.Context, tx pgx.Tx, inst *Installation, runnerName *string) (*work.Runner, error) {
	if runnerName == nil {
		return nil, nil
	}
	id, ok := strings.CutPrefix(*runnerName, RunnerNamePrefix)
	if !ok || !uuidv7.Valid(id) {
		return nil, nil
	}
	r, err := l.engine.GetRunner(ctx, tx, inst.OrganizationID, strings.ToLower(id))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Kind != tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB || r.PoolKey != inst.PoolKey() {
		return nil, nil
	}
	return r, nil
}

func (l *Layer) workflowJob(ctx context.Context, tx pgx.Tx, body []byte, o *outcome) error {
	var ev workflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return errBadPayload
	}
	o.action, o.githubJobID = ev.Action, ev.WorkflowJob.ID
	if ev.Installation == nil || ev.Installation.ID <= 0 || ev.WorkflowJob.ID <= 0 {
		return o.ignore("no installation or job")
	}
	o.githubInstID = ev.Installation.ID
	inst, err := l.byGitHubID(ctx, tx, ev.Installation.ID, lockNone)
	if err != nil {
		return err
	}
	if inst == nil || !inst.Active() {
		return o.ignore("installation not linked or not active")
	}
	o.organizationID = inst.OrganizationID
	// The share lock of the link makes the job wait for a concurrent
	// deactivation (installation event or unlink), which ends the pool's
	// requests, and then read the new state, so no request is stored after
	// it. The organization is locked first, in the order of the unlink.
	if err := api.LockOrganizationShare(ctx, tx, inst.OrganizationID); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return o.ignore("organization is being deleted")
		}
		return err
	}
	org := inst.OrganizationID
	if inst, err = l.byGitHubID(ctx, tx, ev.Installation.ID, lockShare); err != nil {
		return err
	}
	if inst == nil || !inst.Active() || inst.OrganizationID != org {
		return o.ignore("installation not linked or not active")
	}
	switch ev.Action {
	case "queued":
		req, err := l.createRequestTx(ctx, tx, inst, &ev, o)
		if err != nil {
			return err
		}
		if req == nil {
			return o.ignore("labels match no runner profile")
		}
		o.requestID = req.ID
		return nil
	case "in_progress":
		return l.jobInProgress(ctx, tx, inst, &ev, o)
	case "completed":
		return l.jobCompleted(ctx, tx, inst, &ev, o)
	}
	// waiting (for approvals) and future actions.
	return o.ignore("action not handled")
}

// jobInProgress binds the job's request to the runner that GitHub names
// and marks both RUNNING and BUSY. A request whose queued delivery was lost
// is created first.
func (l *Layer) jobInProgress(ctx context.Context, tx pgx.Tx, inst *Installation, ev *workflowJobEvent, o *outcome) error {
	req, err := l.requestOfJob(ctx, tx, inst.OrganizationID, ev.WorkflowJob.ID)
	if err != nil {
		return err
	}
	if req == nil {
		if req, err = l.createRequestTx(ctx, tx, inst, ev, o); err != nil {
			return err
		}
		if req == nil {
			return o.ignore("labels match no runner profile")
		}
	}
	o.requestID = req.ID
	if work.RequestTerminal(req.State) {
		return o.ignore("request already ended")
	}
	r, err := l.tideRunner(ctx, tx, inst, ev.WorkflowJob.RunnerName)
	if err != nil {
		return err
	}
	if r == nil {
		// Another runner with the tide label (not one of tide) took the job.
		// The request stays until the completed event.
		return o.ignore("runner is not a tide runner of the pool")
	}
	o.runnerID = r.ID
	if r.ProfileID != req.ProfileID {
		return o.ignore("runner belongs to another profile")
	}
	if err := l.engine.MarkBusyTx(ctx, tx, inst.OrganizationID, r.ID, req.ID); err != nil {
		if connect.CodeOf(err) == connect.CodeFailedPrecondition {
			// The runner no longer waits (for example, its program exited
			// first). MarkBusyTx changed nothing; the completed event ends
			// the request.
			return o.ignore("runner does not wait for work")
		}
		return err
	}
	runnerID := r.ID
	o.after = append(o.after, func(context.Context) { l.engine.RunnerChanged(runnerID) })
	return nil
}

// jobCompleted ends the job's request: ENDED and COMPLETED when the job ran
// on a tide runner, CANCELED and CANCELED_BY_SOURCE when it never started.
// A job without a request gets an ended request. After the commit, the
// surplus runners of the pool are removed.
func (l *Layer) jobCompleted(ctx context.Context, tx pgx.Tx, inst *Installation, ev *workflowJobEvent, o *outcome) error {
	req, err := l.requestOfJob(ctx, tx, inst.OrganizationID, ev.WorkflowJob.ID)
	if err != nil {
		return err
	}
	if req == nil {
		// The queued delivery failed or was lost, and its redelivery may
		// still come. The request is created and ended at once, so a late
		// queued or in_progress delivery finds an ended request and does not
		// start runners for a job that ended.
		if req, err = l.createRequestTx(ctx, tx, inst, ev, o); err != nil {
			return err
		}
		if req == nil {
			return o.ignore("labels match no runner profile")
		}
	}
	o.requestID = req.ID
	if !work.RequestTerminal(req.State) {
		// GitHub documents no field that tells that a job never started, so
		// tide decides: the job started when tide bound it to a runner, or
		// when the completed event names a tide runner of the pool (the
		// in_progress delivery was lost or came late).
		started := req.State == tidev1.RequestState_REQUEST_STATE_RUNNING
		if !started {
			r, err := l.tideRunner(ctx, tx, inst, ev.WorkflowJob.RunnerName)
			if err != nil {
				return err
			}
			started = r != nil
		}
		end := work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED}
		if !started {
			end = work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_CANCELED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_SOURCE}
		}
		if _, err := l.engine.EndRequestTx(ctx, tx, req.ID, end); err != nil {
			return err
		}
	}
	org, profileID, pool := inst.OrganizationID, req.ProfileID, inst.PoolKey()
	o.after = append(o.after, func(ctx context.Context) {
		l.svc.WorkChanged(org)
		ctx, cancel := context.WithTimeout(ctx, surplusTimeout)
		defer cancel()
		l.removeSurplus(ctx, org, profileID, pool)
	})
	return nil
}

// removeSurplus compares the pool's runners in CREATING, PREPARING or
// WAITING_FOR_WORK with all of its remaining QUEUED requests and removes
// the newest surplus runners: first the GitHub registration, then the
// sandbox. A runner whose removal GitHub refuses took a job and stays. The
// profile's warm count is kept as a reserve (recorded decision), so the
// rule does not delete the warm runners that the dispatcher keeps.
func (l *Layer) removeSurplus(ctx context.Context, orgID, profileID, pool string) {
	reserve := 0
	if p, err := l.profiles.Get(ctx, l.svc.DB.Pool, orgID, profileID); err == nil {
		reserve = int(p.WarmCount)
	}
	surplus, err := l.engine.PoolSurplus(ctx, orgID, profileID, pool, reserve)
	if err != nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		return
	}
	if len(surplus) == 0 {
		return
	}
	inst, err := l.poolInstallation(ctx, l.svc.DB.Pool, orgID, pool)
	if err != nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		return
	}
	for _, r := range surplus {
		if r.State == tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK && inst != nil && inst.Active() {
			refused, err := l.removeRegistration(ctx, inst, r.ID)
			if err != nil {
				// Without a removed registration the runner may still take a
				// job, so it stays; the next completed event tries again.
				l.svc.Logger.WarnContext(ctx, "github_surplus_registration_failed", slog.String("organization_id", orgID), slog.String("runner_id", r.ID), slog.Any("error", err))
				continue
			}
			if refused {
				continue
			}
		}
		d, err := l.engine.DeleteIdleRunner(ctx, orgID, r.ID, tidev1.RunnerEndReason_RUNNER_END_REASON_SURPLUS)
		if err != nil {
			l.svc.Logger.WarnContext(ctx, "github_surplus_delete_failed", slog.String("organization_id", orgID), slog.String("runner_id", r.ID), slog.Any("error", err))
			continue
		}
		if d != nil {
			l.engine.DeleteSandboxOf(ctx, orgID, d.ID)
			l.svc.Logger.InfoContext(ctx, "github_surplus_runner_removed", slog.String("organization_id", orgID), slog.String("runner_id", r.ID))
		}
	}
}

// installationEvent applies deleted, suspend and unsuspend. deleted and
// suspend mark the link DELETED or SUSPENDED, end its QUEUED requests with
// CANCELED, end its started requests, and delete its runners that wait for
// a job. unsuspend makes a SUSPENDED link ACTIVE again (recorded decision:
// GitHub delivers events of the installation again). Other actions only
// refresh the account login.
func (l *Layer) installationEvent(ctx context.Context, tx pgx.Tx, body []byte, o *outcome) error {
	var ev installationEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return errBadPayload
	}
	o.action, o.githubInstID = ev.Action, ev.Installation.ID
	if ev.Installation.ID <= 0 {
		return o.ignore("no installation")
	}
	inst, err := l.byGitHubID(ctx, tx, ev.Installation.ID, lockUpdate)
	if err != nil {
		return err
	}
	if inst == nil {
		return o.ignore("installation not linked")
	}
	o.organizationID = inst.OrganizationID
	now := l.svc.NowUTC()
	if login := ev.Installation.Account.Login; login != "" && login != inst.Login {
		if _, err := tx.Exec(ctx, "UPDATE github_installations SET github_account_login = $2, update_time = $3 WHERE installation_id = $1", inst.ID, login, now); err != nil {
			return err
		}
		inst.Login = login
	}
	var state tidev1.GitHubInstallationState
	switch ev.Action {
	case "deleted":
		state = tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED
	case "suspend":
		state = tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED
	case "unsuspend":
		if inst.State != tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_SUSPENDED {
			return o.ignore("installation not suspended")
		}
		if _, err := tx.Exec(ctx, "UPDATE github_installations SET state = $2, update_time = $3 WHERE installation_id = $1",
			inst.ID, int16(tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_ACTIVE), now); err != nil {
			return err
		}
		return nil
	default:
		return o.ignore("action not handled")
	}
	if inst.State == tidev1.GitHubInstallationState_GIT_HUB_INSTALLATION_STATE_DELETED {
		return o.ignore("installation already deleted")
	}
	if _, err := tx.Exec(ctx, "UPDATE github_installations SET state = $2, update_time = $3 WHERE installation_id = $1", inst.ID, int16(state), now); err != nil {
		return err
	}
	// GitHub no longer gives the app tokens for the installation, so the
	// registrations of the deleted runners cannot be removed; GitHub removes
	// JIT runners that never connect by itself.
	deleted, err := l.deactivateTx(ctx, tx, inst, nil)
	if err != nil {
		return err
	}
	org := inst.OrganizationID
	o.after = append(o.after, func(ctx context.Context) {
		for _, id := range deleted {
			l.engine.DeleteSandboxOf(ctx, org, id)
		}
		l.svc.WorkChanged(org)
	})
	return nil
}
