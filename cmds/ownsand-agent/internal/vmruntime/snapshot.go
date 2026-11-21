// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import "fmt"

// FinishSnapshot combines the result of a snapshot capture with the result
// of the resume that follows it, for a runtime that paused a running VM to
// capture it.
//
// A VM that stays paused while the manager reports it running must not yield
// a READY snapshot, so a resume error fails an otherwise successful capture
// and clears the returned info. A capture error takes priority: it stays the
// returned error (and keeps its error chain), and a failed resume is
// appended to its message as additional diagnostic context. A nil resumeErr
// returns the capture result unchanged.
func FinishSnapshot(info SnapshotInfo, captureErr, resumeErr error) (SnapshotInfo, error) {
	switch {
	case resumeErr == nil:
		return info, captureErr
	case captureErr != nil:
		return info, fmt.Errorf("%w (the resume after the failed snapshot failed too: %s)", captureErr, resumeErr)
	default:
		return SnapshotInfo{}, fmt.Errorf("resume after snapshot: %w", resumeErr)
	}
}
