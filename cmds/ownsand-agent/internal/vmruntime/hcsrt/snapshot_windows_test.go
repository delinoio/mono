// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// snapshotFixture registers a VM whose HCS pause and resume calls are
// replaced, so Snapshot runs without a Hyper-V VM. The closed exited
// channel makes the guest flush that every pause starts fail at once; a
// failed flush only logs.
type snapshotFixture struct {
	r       *Runtime
	v       *vm
	pauses  int
	resumes int
}

func newSnapshotFixture(t *testing.T, resumeErr error, paused bool) *snapshotFixture {
	t.Helper()
	r := &Runtime{logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), vms: map[string]*vm{}}
	f := &snapshotFixture{r: r}
	v := &vm{
		id:      "snapshot-resume",
		dir:     t.TempDir(),
		guestOS: ownsandv1.GuestOs_GUEST_OS_LINUX,
		exited:  make(chan struct{}),
		paused:  paused,
		sys: &system{
			pauseFn:  func() error { f.pauses++; return nil },
			resumeFn: func() error { f.resumes++; return resumeErr },
		},
	}
	close(v.exited)
	f.v = v
	r.vms[v.id] = v
	return f
}

func writeDisk(t *testing.T, v *vm) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(v.dir, diskFile), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotOfRunningVMResumesIt(t *testing.T) {
	f := newSnapshotFixture(t, nil, false)
	writeDisk(t, f.v)
	info, err := f.r.Snapshot(context.Background(), f.v, filepath.Join(t.TempDir(), "snap"))
	if err != nil {
		t.Fatal(err)
	}
	if info.SizeBytes != 4 {
		t.Fatalf("size %d, want 4", info.SizeBytes)
	}
	if f.pauses != 1 || f.resumes != 1 || f.v.paused {
		t.Fatalf("pauses %d resumes %d paused %v, want 1 1 false", f.pauses, f.resumes, f.v.paused)
	}
}

func TestSnapshotOfPausedVMKeepsItPaused(t *testing.T) {
	f := newSnapshotFixture(t, errors.New("must not be called"), true)
	writeDisk(t, f.v)
	if _, err := f.r.Snapshot(context.Background(), f.v, filepath.Join(t.TempDir(), "snap")); err != nil {
		t.Fatal(err)
	}
	if f.pauses != 0 || f.resumes != 0 || !f.v.paused {
		t.Fatalf("pauses %d resumes %d paused %v, want 0 0 true", f.pauses, f.resumes, f.v.paused)
	}
}

// A resume that fails after a completed capture fails the snapshot, so the
// manager records no READY snapshot for a VM that stays paused.
func TestSnapshotReportsResumeFailure(t *testing.T) {
	resumeErr := errors.New("hcs resume rejected")
	f := newSnapshotFixture(t, resumeErr, false)
	writeDisk(t, f.v)
	info, err := f.r.Snapshot(context.Background(), f.v, filepath.Join(t.TempDir(), "snap"))
	if !errors.Is(err, resumeErr) || !strings.Contains(err.Error(), "resume after snapshot") {
		t.Fatalf("snapshot error %v, want the resume error", err)
	}
	if info.SizeBytes != 0 || info.HasMemoryState {
		t.Fatalf("snapshot info %+v, want zero", info)
	}
	if !f.v.paused {
		t.Fatal("the VM must stay recorded as paused after a failed resume")
	}
}

// A capture error stays the result when the resume fails too, and the
// resume error is added to its message.
func TestSnapshotCaptureErrorWinsOverResumeFailure(t *testing.T) {
	f := newSnapshotFixture(t, errors.New("hcs resume rejected"), false)
	// The source disk is missing, so the copy fails.
	_, err := f.r.Snapshot(context.Background(), f.v, filepath.Join(t.TempDir(), "snap"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot error %v, want the disk error", err)
	}
	if !strings.Contains(err.Error(), "hcs resume rejected") {
		t.Fatalf("snapshot error %q lacks the resume error", err)
	}
	if f.resumes != 1 {
		t.Fatalf("resumes %d, want 1", f.resumes)
	}
}
