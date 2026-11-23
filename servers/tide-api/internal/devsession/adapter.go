// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/work"
)

// prepareTimeout bounds the exec that creates the workspace.
const prepareTimeout = 2 * time.Minute

// Adapter is the DEV_SESSION adapter of the work engine: the dispatcher
// binds a runner to the session, PREPARING creates the workspace path, and
// the runner is BUSY at once without a runner program.
type Adapter struct{ work.BaseAdapter }

var _ work.Adapter = Adapter{}

func (Adapter) Kind() tidev1.WorkloadKind { return tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION }

func (Adapter) BindsAtDispatch() bool { return true }

// MkdirCommand returns the command that creates a workspace path and its
// parents when they are missing.
func MkdirCommand(guest tidev1.GuestOs, path string) []string {
	if guest == tidev1.GuestOs_GUEST_OS_WINDOWS {
		// PowerShell is part of every supported Windows guest; a single
		// quoted string takes the path literally once its quotes double.
		quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"New-Item -ItemType Directory -Force -Path " + quoted + " | Out-Null"}
	}
	return []string{"mkdir", "-p", "--", path}
}

// Prepare creates the workspace path when it is missing. tide installs no
// runner program for a dev session.
func (Adapter) Prepare(ctx context.Context, g *work.Guest) error {
	r := g.Runner
	if r.WorkspacePath == "" {
		return fmt.Errorf("the runner has no workspace path")
	}
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	res, err := g.Exec(ctx, ownsand.ExecParams{ExecID: newExecID(), Command: MkdirCommand(r.GuestOS, r.WorkspacePath), Timeout: prepareTimeout})
	if err != nil {
		return err
	}
	if res.Kind != ownsand.ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 {
		return fmt.Errorf("creating the workspace path failed (%s)", res.Kind)
	}
	return nil
}

// Start makes the runner BUSY at once: a dev session has no runner program.
func (Adapter) Start(context.Context, *work.Guest) (work.StartResult, error) {
	return work.StartResult{Busy: true}, nil
}

// SandboxEnded: an OwnSand TTL ends the session with SESSION_TTL_EXPIRED
// and the runner with DELETED; any other OwnSand end fails the runner and
// the session with RUNNER_LOST.
func (Adapter) SandboxEnded(_ *work.Request, reason ownsandv1.SandboxEndReason) work.SandboxOutcome {
	switch reason {
	case ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED:
		return work.SandboxOutcome{
			RunnerState: tidev1.RunnerState_RUNNER_STATE_DELETED, RunnerReason: tidev1.RunnerEndReason_RUNNER_END_REASON_SANDBOX_ENDED,
			Request: work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_TTL_EXPIRED},
		}
	}
	return work.BaseAdapter{}.SandboxEnded(nil, reason)
}

// RunnerDeleted: a deletion by a user ends the session with
// SESSION_DELETED, the end of the owner's membership with MEMBER_REMOVED.
func (Adapter) RunnerDeleted(_ *work.Request, cause work.DeleteCause) (work.RequestEnd, bool) {
	if cause == work.DeleteMemberRemoved {
		return work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_MEMBER_REMOVED}, false
	}
	return work.RequestEnd{State: tidev1.RequestState_REQUEST_STATE_ENDED, Reason: tidev1.RequestEndReason_REQUEST_END_REASON_SESSION_DELETED}, false
}
