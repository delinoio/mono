// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// dismountRecorder is a mountedISO.run that records its calls and returns the
// scripted results in order, then nil.
type dismountRecorder struct {
	results []error
	ctxErrs []error
	env     [][]string
}

func (d *dismountRecorder) run(ctx context.Context, script string, env ...string) ([]byte, error) {
	i := len(d.ctxErrs)
	d.ctxErrs = append(d.ctxErrs, ctx.Err())
	d.env = append(d.env, env)
	if !strings.Contains(script, "Dismount-DiskImage") {
		return nil, errors.New("unexpected script")
	}
	if i < len(d.results) {
		return nil, d.results[i]
	}
	return nil, nil
}

// The build dismounts once after the image is applied with the build context
// and defers a second dismount with a background context.
func TestMountedISOFailedExplicitDismountIsRetriedByDeferred(t *testing.T) {
	rec := &dismountRecorder{results: []error{errors.New("detach failed")}}
	m := &mountedISO{path: `C:\iso\win.iso`, ours: true, run: rec.run}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	m.release(canceled, logger) // the explicit dismount, which fails
	if !m.ours {
		t.Fatal("ours was cleared by a failed dismount")
	}
	if !strings.Contains(buf.String(), "dismount the ISO failed") {
		t.Errorf("the failure was not logged: %q", buf.String())
	}
	m.release(context.Background(), logger) // the deferred dismount
	if len(rec.ctxErrs) != 2 {
		t.Fatalf("the runner ran %d times, want 2", len(rec.ctxErrs))
	}
	if rec.ctxErrs[1] != nil {
		t.Errorf("the deferred dismount ran with a canceled context: %v", rec.ctxErrs[1])
	}
	if m.ours {
		t.Error("ours stayed set after the dismount succeeded")
	}
	if got := rec.env[1]; len(got) != 1 || got[0] != `OWNSAND_ISO=C:\iso\win.iso` {
		t.Errorf("env = %v", got)
	}
	m.release(context.Background(), logger)
	if len(rec.ctxErrs) != 2 {
		t.Errorf("a dismount after success ran the runner again (%d calls)", len(rec.ctxErrs))
	}
}

func TestMountedISOSuccessfulExplicitDismountLeavesDeferredIdle(t *testing.T) {
	rec := &dismountRecorder{}
	m := &mountedISO{path: "x.iso", ours: true, run: rec.run}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m.release(context.Background(), logger)
	m.release(context.Background(), logger) // deferred
	if len(rec.ctxErrs) != 1 {
		t.Errorf("the runner ran %d times, want 1", len(rec.ctxErrs))
	}
	if m.ours || buf.Len() != 0 {
		t.Errorf("ours = %v, log = %q", m.ours, buf.String())
	}
}

func TestMountedISOCanceledDismountKeepsOurs(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	m := &mountedISO{path: "x.iso", ours: true}
	var calls int
	m.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := m.dismount(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("dismount = %v, want context.Canceled", err)
	}
	if !m.ours {
		t.Fatal("a canceled dismount cleared ours")
	}
	if err := m.dismount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || m.ours {
		t.Errorf("calls = %d, ours = %v", calls, m.ours)
	}
}

func TestMountedISONotOursMakesNoCall(t *testing.T) {
	m := &mountedISO{path: "x.iso", run: func(context.Context, string, ...string) ([]byte, error) {
		t.Error("dismount of an ISO the build did not attach ran a command")
		return nil, nil
	}}
	if err := m.dismount(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.release(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
}
