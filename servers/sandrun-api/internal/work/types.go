// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"strconv"
	"time"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
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
	Kind           sandrunv1.WorkloadKind
	Source         Source
	SourceKey      string
	State          sandrunv1.RequestState
	EndReason      sandrunv1.RequestEndReason
	Priority       sandrunv1.Priority
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
	Kind             sandrunv1.WorkloadKind
	GuestOS          sandrunv1.GuestOs
	State            sandrunv1.RunnerState
	EndReason        sandrunv1.RunnerEndReason
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
	// A pool runner can have a queued sponsor before GitHub assigns work.
	sponsorRequestID string
	// The revisions are set only for runners created by warm fill.
	warmProfileRevision string
	warmLinkRevision    string
}

// Request states.
const (
	reqQueued   = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	reqStarting = sandrunv1.RequestState_REQUEST_STATE_STARTING
	reqRunning  = sandrunv1.RequestState_REQUEST_STATE_RUNNING
	reqEnded    = sandrunv1.RequestState_REQUEST_STATE_ENDED
	reqCanceled = sandrunv1.RequestState_REQUEST_STATE_CANCELED
	reqExpired  = sandrunv1.RequestState_REQUEST_STATE_EXPIRED
	reqFailed   = sandrunv1.RequestState_REQUEST_STATE_FAILED
)

// Runner states.
const (
	runCreating = sandrunv1.RunnerState_RUNNER_STATE_CREATING
	runPrep     = sandrunv1.RunnerState_RUNNER_STATE_PREPARING
	runWarm     = sandrunv1.RunnerState_RUNNER_STATE_WARM
	runWaiting  = sandrunv1.RunnerState_RUNNER_STATE_WAITING_FOR_WORK
	runBusy     = sandrunv1.RunnerState_RUNNER_STATE_BUSY
	runDeleting = sandrunv1.RunnerState_RUNNER_STATE_DELETING
	runDeleted  = sandrunv1.RunnerState_RUNNER_STATE_DELETED
	runFailed   = sandrunv1.RunnerState_RUNNER_STATE_FAILED
)

// RequestTerminal reports whether a request state is terminal.
func RequestTerminal(s sandrunv1.RequestState) bool {
	return s == reqEnded || s == reqCanceled || s == reqExpired || s == reqFailed
}

// RunnerTerminal reports whether a runner state is terminal.
func RunnerTerminal(s sandrunv1.RunnerState) bool { return s == runDeleted || s == runFailed }

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
	r.Kind, r.Source, r.State = sandrunv1.WorkloadKind(kind), Source(source), sandrunv1.RequestState(state)
	r.EndReason, r.Priority = sandrunv1.RequestEndReason(reason), sandrunv1.Priority(priority)
	return &r, nil
}

const runnerColumns = `runner_id::text, organization_id::text, runner_profile_id::text, kind, guest_os, state, end_reason, COALESCE(request_id::text, ''),
	COALESCE(pool_key, ''), idempotency_key::text, COALESCE(ownsand_sandbox_id::text, ''), ownsand_device_id, ownsand_end_reason, workspace_path,
	COALESCE(program_exec_id::text, ''), create_time, ready_time, end_time, update_time, change_seq,
	COALESCE(warm_profile_revision::text, ''), COALESCE(warm_link_revision::text, ''), COALESCE(sponsor_request_id::text, '')`

func scanRunner(row scanner) (*Runner, error) {
	var r Runner
	var kind, guest, state, reason int16
	if err := row.Scan(&r.ID, &r.OrganizationID, &r.ProfileID, &kind, &guest, &state, &reason, &r.RequestID, &r.PoolKey, &r.IdempotencyKey,
		&r.SandboxID, &r.DeviceID, &r.OwnSandEndReason, &r.WorkspacePath, &r.ProgramExecID, &r.CreateTime, &r.ReadyTime, &r.EndTime,
		&r.UpdateTime, &r.ChangeSeq, &r.warmProfileRevision, &r.warmLinkRevision, &r.sponsorRequestID); err != nil {
		return nil, err
	}
	r.Kind, r.GuestOS, r.State, r.EndReason = sandrunv1.WorkloadKind(kind), sandrunv1.GuestOs(guest), sandrunv1.RunnerState(state), sandrunv1.RunnerEndReason(reason)
	return &r, nil
}

// Proto returns the API form of a request.
func (r *Request) Proto() *sandrunv1.Request {
	out := &sandrunv1.Request{
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
func (r *Runner) Proto() *sandrunv1.Runner {
	out := &sandrunv1.Runner{
		RunnerId: r.ID, OrganizationId: r.OrganizationID, RunnerProfileId: r.ProfileID, Kind: r.Kind, State: r.State, EndReason: r.EndReason,
		OwnsandSandboxId: r.SandboxID, OwnsandDeviceId: r.DeviceID, RequestId: r.RequestID, CreatedAt: api.Timestamp(r.CreateTime),
		EndedAt: api.TimestampPtr(r.EndTime), OwnsandEndReason: r.OwnSandEndReason, GuestOs: r.GuestOS,
	}
	if r.Kind == sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB {
		out.GithubInstallationId, _ = strconv.ParseInt(r.PoolKey, 10, 64)
	}
	return out
}
