// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/profile"
)

// Adapter is the per-workload part of the queue and the runner lifecycle.
// The engine owns the queue order, the dispatch, the sandbox lifecycle, the
// retries and the state changes; an adapter decides what the contract leaves
// to the workload kind and vendor. Register one adapter per workload kind
// with Engine.RegisterAdapter (a later layer for GITHUB_JOB, one for
// AGENT_SESSION that dispatches by vendor, and the dev session layer for
// DEV_SESSION). A request of a kind without an adapter stays QUEUED until
// its maximum queue wait.
//
// A layer that receives demand creates requests with Engine.CreateRequest
// (unique per organization, source and source key), and reports the
// progress of its runner with Engine.MarkBusy and Engine.EndRequest (for
// example when a GitHub workflow_job event arrives).
type Adapter interface {
	// Kind is the workload kind of the adapter.
	Kind() tidev1.WorkloadKind

	// BindsAtDispatch reports whether the dispatcher binds one runner to one
	// request when it dispatches it (AGENT_SESSION, DEV_SESSION). Such a
	// request is STARTING from the assignment until its runner is BUSY.
	// When false (GITHUB_JOB), the dispatcher starts runners for a runner
	// pool (Request.PoolKey and Runner.PoolKey, for example the GitHub
	// installation), the source binds work to a runner later, and the
	// oldest QUEUED requests of a pool count as covered while the pool has
	// as many runners in CREATING, PREPARING or WAITING_FOR_WORK.
	BindsAtDispatch() bool

	// Prepare runs in PREPARING (lifecycle step 2) when the sandbox is
	// RUNNING: check the runner program and install it when it is missing,
	// or (DEV_SESSION) create the workspace. An error is a start failure.
	Prepare(ctx context.Context, g *Guest) error

	// Start runs in lifecycle step 3 when the runner has its request (or,
	// without BindsAtDispatch, right after Prepare): write the runner
	// credential to a guest file that only the runner process reads and
	// start the runner program with an OwnSand exec whose stream carries
	// only the exit result. An error is a start failure.
	Start(ctx context.Context, g *Guest) (StartResult, error)

	// ProgramExited reports how the request of a runner ends when its
	// program exits; the engine then deletes the sandbox (WORK_DONE).
	// keep=true leaves the request unchanged (for example, GitHub sends the
	// completed event).
	ProgramExited(req *Request, res *ownsand.ExecResult) (end RequestEnd, keep bool)

	// SandboxEnded maps an end of the sandbox that tide did not ask for
	// (TTL, device removal, LOST_ON_DEVICE) to the runner and request
	// outcome (lifecycle step 6). req is nil for a runner without a request.
	SandboxEnded(req *Request, reason ownsandv1.SandboxEndReason) SandboxOutcome

	// RunnerDeleted reports how the request of a runner ends when a user, a
	// membership end or a clean-up deletes the runner.
	RunnerDeleted(req *Request, cause DeleteCause) (end RequestEnd, keep bool)

	// ReleaseClaim runs after tide ended a request before a runner took its
	// session (cancellation, maximum queue wait, failures, clean-up), for
	// example to release a Cursor or Devin claim. Errors are logged.
	ReleaseClaim(ctx context.Context, req *Request) error
}

// StartResult is the outcome of Adapter.Start.
type StartResult struct {
	// Busy: the runner serves its request at once (DEV_SESSION). The runner
	// becomes BUSY and the request RUNNING.
	Busy bool
	// ProgramExecID is the exec of the runner program, which the engine
	// follows with AttachExec. The runner becomes WAITING_FOR_WORK.
	ProgramExecID string
}

// RequestEnd is a terminal request state and its end reason.
type RequestEnd struct {
	State  tidev1.RequestState
	Reason tidev1.RequestEndReason
}

// SandboxOutcome is the outcome of an unexpected sandbox end.
type SandboxOutcome struct {
	// RunnerState is DELETED or FAILED, with RunnerReason.
	RunnerState  tidev1.RunnerState
	RunnerReason tidev1.RunnerEndReason
	// Request is the end of the runner's request; KeepRequest leaves it.
	Request     RequestEnd
	KeepRequest bool
}

// DeleteCause is why tide deletes a runner that has a request.
type DeleteCause int

const (
	DeleteByUser DeleteCause = iota + 1
	DeleteMemberRemoved
	DeleteCleanUp
)

// Guest is the sandbox of a runner during Prepare and Start.
type Guest struct {
	Client  ownsandclient.Client
	Runner  *Runner
	Request *Request // nil for a runner without a request
	// Profile is the runner's profile, or nil when it was deleted.
	Profile *profile.Profile
}

// Exec runs one command in the guest and waits for its result.
func (g *Guest) Exec(ctx context.Context, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
	return g.Client.Exec(ctx, g.Runner.SandboxID, p)
}

// BaseAdapter has the default behavior of the lifecycle: a program exit
// ends the request with ENDED COMPLETED, an unexpected sandbox end fails the
// runner and the request (RUNNER_LOST), a deletion fails the request and no
// claim exists. Adapters embed it and replace what they need.
type BaseAdapter struct{}

func (BaseAdapter) ProgramExited(*Request, *ownsand.ExecResult) (RequestEnd, bool) {
	return RequestEnd{State: reqEnded, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_COMPLETED}, false
}

func (BaseAdapter) SandboxEnded(_ *Request, _ ownsandv1.SandboxEndReason) SandboxOutcome {
	return SandboxOutcome{
		RunnerState: runFailed, RunnerReason: tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED,
		Request: RequestEnd{State: reqFailed, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST},
	}
}

func (BaseAdapter) RunnerDeleted(_ *Request, cause DeleteCause) (RequestEnd, bool) {
	if cause == DeleteMemberRemoved {
		return RequestEnd{State: reqEnded, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED}, false
	}
	return RequestEnd{State: reqFailed, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_RUNNER_LOST}, false
}

func (BaseAdapter) ReleaseClaim(context.Context, *Request) error { return nil }
