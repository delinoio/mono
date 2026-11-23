// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"strconv"
	"time"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
)

// Source is who sent the demand of a request. Together with the source key
// it is unique per organization.
type Source int16

const (
	SourceUnspecified Source = iota
	SourceDevSession
	SourceGitHub
	SourceClaudeCode
	SourceCursor
	SourceOpenAICodex
	SourceDevin
)

// Request is a stored request.
type Request struct {
	ID             string
	OrganizationID string
	ProfileID      string
	Kind           tidev1.WorkloadKind
	Source         Source
	SourceKey      string
	State          tidev1.RequestState
	EndReason      tidev1.RequestEndReason
	Priority       tidev1.Priority
	QueueTime      time.Time
	QueueDeadline  time.Time
	StartTime      *time.Time
	EndTime        *time.Time
	RunnerID       string
	StartAttempts  int32
	OwnerUserID    string
	Label          string
	PoolKey        string
	CreateTime     time.Time
	UpdateTime     time.Time
	ChangeSeq      int64
}

// Runner is a stored runner.
type Runner struct {
	ID               string
	OrganizationID   string
	ProfileID        string
	Kind             tidev1.WorkloadKind
	GuestOS          tidev1.GuestOs
	State            tidev1.RunnerState
	EndReason        tidev1.RunnerEndReason
	RequestID        string
	PoolKey          string
	IdempotencyKey   string
	SandboxID        string
	DeviceID         string
	OwnSandEndReason string
	WorkspacePath    string
	ProgramExecID    string
	CreateTime       time.Time
	ReadyTime        *time.Time
	EndTime          *time.Time
	UpdateTime       time.Time
	ChangeSeq        int64
}

// Request states.
const (
	reqQueued   = tidev1.RequestState_REQUEST_STATE_QUEUED
	reqStarting = tidev1.RequestState_REQUEST_STATE_STARTING
	reqRunning  = tidev1.RequestState_REQUEST_STATE_RUNNING
	reqEnded    = tidev1.RequestState_REQUEST_STATE_ENDED
	reqCanceled = tidev1.RequestState_REQUEST_STATE_CANCELED
	reqExpired  = tidev1.RequestState_REQUEST_STATE_EXPIRED
	reqFailed   = tidev1.RequestState_REQUEST_STATE_FAILED
)

// Runner states.
const (
	runCreating = tidev1.RunnerState_RUNNER_STATE_CREATING
	runPrep     = tidev1.RunnerState_RUNNER_STATE_PREPARING
	runWarm     = tidev1.RunnerState_RUNNER_STATE_WARM
	runWaiting  = tidev1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	runBusy     = tidev1.RunnerState_RUNNER_STATE_BUSY
	runDeleting = tidev1.RunnerState_RUNNER_STATE_DELETING
	runDeleted  = tidev1.RunnerState_RUNNER_STATE_DELETED
	runFailed   = tidev1.RunnerState_RUNNER_STATE_FAILED
)

// RequestTerminal reports whether a request state is terminal.
func RequestTerminal(s tidev1.RequestState) bool {
	return s == reqEnded || s == reqCanceled || s == reqExpired || s == reqFailed
}

// RunnerTerminal reports whether a runner state is terminal.
func RunnerTerminal(s tidev1.RunnerState) bool { return s == runDeleted || s == runFailed }

// openRequestStates are the non-terminal request states.
var openRequestStates = []int16{int16(reqQueued), int16(reqStarting), int16(reqRunning)}

// openRunnerStates are the non-terminal runner states.
var openRunnerStates = []int16{int16(runCreating), int16(runPrep), int16(runWarm), int16(runWaiting), int16(runBusy), int16(runDeleting)}

const requestColumns = `request_id::text, organization_id::text, runner_profile_id::text, kind, source, source_key, state, end_reason, priority,
	queue_time, queue_deadline, start_time, end_time, COALESCE(runner_id::text, ''), start_attempts, owner_user_id, label, COALESCE(pool_key, ''),
	create_time, update_time, change_seq`

type scanner interface{ Scan(dest ...any) error }

// RequestColumns is the column list that ScanRequest reads.
const RequestColumns = requestColumns

// RunnerColumns is the column list that ScanRunner reads.
const RunnerColumns = runnerColumns

// ScanRequest reads a row of RequestColumns.
func ScanRequest(row interface{ Scan(dest ...any) error }) (*Request, error) { return scanRequest(row) }

// ScanRunner reads a row of RunnerColumns.
func ScanRunner(row interface{ Scan(dest ...any) error }) (*Runner, error) { return scanRunner(row) }

// OpenRequestStates are the non-terminal request states as stored.
func OpenRequestStates() []int16 { return append([]int16(nil), openRequestStates...) }

func scanRequest(row scanner) (*Request, error) {
	var r Request
	var kind, source, state, reason, priority int16
	if err := row.Scan(&r.ID, &r.OrganizationID, &r.ProfileID, &kind, &source, &r.SourceKey, &state, &reason, &priority,
		&r.QueueTime, &r.QueueDeadline, &r.StartTime, &r.EndTime, &r.RunnerID, &r.StartAttempts, &r.OwnerUserID, &r.Label, &r.PoolKey,
		&r.CreateTime, &r.UpdateTime, &r.ChangeSeq); err != nil {
		return nil, err
	}
	r.Kind, r.Source, r.State = tidev1.WorkloadKind(kind), Source(source), tidev1.RequestState(state)
	r.EndReason, r.Priority = tidev1.RequestEndReason(reason), tidev1.Priority(priority)
	return &r, nil
}

const runnerColumns = `runner_id::text, organization_id::text, runner_profile_id::text, kind, guest_os, state, end_reason, COALESCE(request_id::text, ''),
	COALESCE(pool_key, ''), idempotency_key::text, COALESCE(ownsand_sandbox_id::text, ''), ownsand_device_id, ownsand_end_reason, workspace_path,
	COALESCE(program_exec_id::text, ''), create_time, ready_time, end_time, update_time, change_seq`

func scanRunner(row scanner) (*Runner, error) {
	var r Runner
	var kind, guest, state, reason int16
	if err := row.Scan(&r.ID, &r.OrganizationID, &r.ProfileID, &kind, &guest, &state, &reason, &r.RequestID, &r.PoolKey, &r.IdempotencyKey,
		&r.SandboxID, &r.DeviceID, &r.OwnSandEndReason, &r.WorkspacePath, &r.ProgramExecID, &r.CreateTime, &r.ReadyTime, &r.EndTime,
		&r.UpdateTime, &r.ChangeSeq); err != nil {
		return nil, err
	}
	r.Kind, r.GuestOS, r.State, r.EndReason = tidev1.WorkloadKind(kind), tidev1.GuestOs(guest), tidev1.RunnerState(state), tidev1.RunnerEndReason(reason)
	return &r, nil
}

// Proto returns the API form of a request.
func (r *Request) Proto() *tidev1.Request {
	out := &tidev1.Request{
		RequestId: r.ID, OrganizationId: r.OrganizationID, RunnerProfileId: r.ProfileID, Kind: r.Kind, State: r.State, EndReason: r.EndReason,
		Priority: r.Priority, QueuedAt: api.Timestamp(r.QueueTime), StartedAt: api.TimestampPtr(r.StartTime), EndedAt: api.TimestampPtr(r.EndTime),
		RunnerId: r.RunnerID, OwnerUserId: r.OwnerUserID, StartAttempts: r.StartAttempts,
	}
	if r.Source != SourceDevSession {
		out.SourceKey = r.SourceKey
	}
	return out
}

// Proto returns the API form of a runner. The runner pool of a GITHUB_JOB
// runner is the GitHub numeric installation ID in decimal.
func (r *Runner) Proto() *tidev1.Runner {
	out := &tidev1.Runner{
		RunnerId: r.ID, OrganizationId: r.OrganizationID, RunnerProfileId: r.ProfileID, Kind: r.Kind, State: r.State, EndReason: r.EndReason,
		OwnsandSandboxId: r.SandboxID, OwnsandDeviceId: r.DeviceID, RequestId: r.RequestID, CreatedAt: api.Timestamp(r.CreateTime),
		EndedAt: api.TimestampPtr(r.EndTime), OwnsandEndReason: r.OwnSandEndReason, GuestOs: r.GuestOS,
	}
	if r.Kind == tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB {
		out.GithubInstallationId, _ = strconv.ParseInt(r.PoolKey, 10, 64)
	}
	return out
}
