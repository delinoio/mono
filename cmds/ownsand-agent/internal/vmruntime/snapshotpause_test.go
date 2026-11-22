// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// snapshotCalls records the order of the pause, capture and resume calls.
type snapshotCalls struct {
	calls                           []string
	pauseErr, captureErr, resumeErr error
}

func (c *snapshotCalls) run(wasPaused bool) (SnapshotInfo, error) {
	return SnapshotWithPause(discardLogger(), "sbx", wasPaused,
		func() error { c.calls = append(c.calls, "pause"); return c.pauseErr },
		func() error { c.calls = append(c.calls, "resume"); return c.resumeErr },
		func() (SnapshotInfo, error) {
			c.calls = append(c.calls, "capture")
			if c.captureErr != nil {
				return SnapshotInfo{SizeBytes: 1}, c.captureErr
			}
			return SnapshotInfo{SizeBytes: 7, HasMemoryState: true}, nil
		})
}

func (c *snapshotCalls) order() string { return strings.Join(c.calls, ",") }

func TestSnapshotWithPauseSuccess(t *testing.T) {
	c := &snapshotCalls{}
	info, err := c.run(false)
	if err != nil || info.SizeBytes != 7 || !info.HasMemoryState || info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
		t.Fatalf("info %+v err %v, want the captured info", info, err)
	}
	if c.order() != "pause,capture,resume" {
		t.Fatalf("calls %s", c.order())
	}
}

func TestSnapshotWithPauseResumeFailureFailsSnapshot(t *testing.T) {
	resumeErr := errors.New("hcs: resume rejected")
	c := &snapshotCalls{resumeErr: resumeErr}
	info, err := c.run(false)
	if !errors.Is(err, resumeErr) || !strings.Contains(err.Error(), "resume after snapshot") {
		t.Fatalf("error %v, want the resume error", err)
	}
	if want := (SnapshotInfo{SourceState: ownsandv1.SandboxState_SANDBOX_STATE_PAUSED}); !reflect.DeepEqual(info, want) {
		t.Fatalf("info %+v, want %+v: the source stays paused and no capture is usable", info, want)
	}
}

func TestSnapshotWithPauseCaptureErrorWinsAndKeepsResumeDiagnostic(t *testing.T) {
	captureErr := errors.New("disk copy failed")
	resumeErr := errors.New("hcs: resume rejected")
	c := &snapshotCalls{captureErr: captureErr, resumeErr: resumeErr}
	info, err := c.run(false)
	if !errors.Is(err, captureErr) {
		t.Fatalf("error %v does not wrap the capture error", err)
	}
	if !errors.Is(err, resumeErr) || !strings.Contains(err.Error(), resumeErr.Error()) {
		t.Fatalf("error %v does not carry the resume diagnostic", err)
	}
	if !strings.HasPrefix(err.Error(), captureErr.Error()) {
		t.Fatalf("error %q must lead with the capture error", err)
	}
	if want := (SnapshotInfo{SourceState: ownsandv1.SandboxState_SANDBOX_STATE_PAUSED}); !reflect.DeepEqual(info, want) {
		t.Fatalf("info %+v, want %+v", info, want)
	}
	if c.order() != "pause,capture,resume" {
		t.Fatalf("calls %s, want the resume attempted after the failed capture", c.order())
	}
}

func TestSnapshotWithPauseCaptureErrorOnly(t *testing.T) {
	captureErr := errors.New("disk copy failed")
	c := &snapshotCalls{captureErr: captureErr}
	info, err := c.run(false)
	if err != captureErr {
		t.Fatalf("error %v, want the capture error unchanged", err)
	}
	if info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
		t.Fatalf("source state %v, want UNSPECIFIED after a successful resume", info.SourceState)
	}
}

func TestSnapshotWithPauseAlreadyPaused(t *testing.T) {
	// The sandbox stays paused: no pause, no resume, even when a resume
	// would fail.
	c := &snapshotCalls{resumeErr: errors.New("must not run")}
	info, err := c.run(true)
	if err != nil || info.SizeBytes != 7 || info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
		t.Fatalf("info %+v err %v", info, err)
	}
	if c.order() != "capture" {
		t.Fatalf("calls %s, want capture only", c.order())
	}
	c = &snapshotCalls{captureErr: errors.New("boom")}
	if info, err := c.run(true); err == nil || c.order() != "capture" || info.SourceState != ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
		t.Fatalf("info %+v err %v calls %s", info, err, c.order())
	}
}

func TestSnapshotWithPausePauseFailure(t *testing.T) {
	pauseErr := errors.New("pause rejected")
	c := &snapshotCalls{pauseErr: pauseErr}
	info, err := c.run(false)
	if err != pauseErr || c.order() != "pause" || !reflect.DeepEqual(info, SnapshotInfo{}) {
		t.Fatalf("info %+v err %v calls %s, want the pause error, no source state change and no capture", info, err, c.order())
	}
}
