// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"errors"
	"fmt"
	"log/slog"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// SnapshotWithPause runs capture while the source VM is paused and resumes
// the VM afterwards. A sandbox that is already paused (wasPaused) is captured
// as it is: no pause and no resume run, and the sandbox stays paused.
//
// The resume of a VM that this call paused decides the result:
//   - A resume failure after a completed capture fails the snapshot with the
//     resume error and a SnapshotInfo that is zero except for SourceState
//     PAUSED. The source VM is then still paused while the manager reports it
//     running, so the snapshot must not become READY, and the manager
//     records the sandbox as paused.
//   - A capture error stays the result when the resume fails too. The resume
//     error is added as diagnostic context, errors.Is and errors.As find
//     both errors, and SourceState is PAUSED as well.
//   - A failed pause returns the pause error and runs neither capture nor
//     resume.
//
// Every resume failure is logged with the sandbox ID. The runtime adapters of
// macOS and Hyper-V share this function so that it can be tested without a
// hypervisor.
func SnapshotWithPause(logger *slog.Logger, sandboxID string, wasPaused bool, pause, resume func() error, capture func() (SnapshotInfo, error)) (SnapshotInfo, error) {
	if wasPaused {
		return capture()
	}
	if err := pause(); err != nil {
		return SnapshotInfo{}, err
	}
	info, err := capture()
	rerr := resume()
	if rerr == nil {
		return info, err
	}
	logger.Error("resume after snapshot failed", "sandbox_id", sandboxID, "error", rerr)
	rerr = fmt.Errorf("resume after snapshot: %w", rerr)
	// The VM that this call paused is still paused: report that outcome with
	// the error, so the manager does not keep a RUNNING record for it.
	failed := SnapshotInfo{SourceState: ownsandv1.SandboxState_SANDBOX_STATE_PAUSED}
	if err != nil {
		return failed, errors.Join(err, rerr)
	}
	return failed, rerr
}
