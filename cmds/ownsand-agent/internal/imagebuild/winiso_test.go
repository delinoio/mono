// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
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

// fakeISOHost is an isoRunner that models the attach state of one ISO and
// records the steps. Each step can fail through its field; failMountAfter
// makes the mount step attach the image and still fail.
type fakeISOHost struct {
	attached bool
	steps    []string
	ctxErrs  map[string][]error

	queryErr, mountErr, volumeErr, dismountErr error
	mountAttachesOnError                       bool
	queryErrAfterMount                         error
	volumeOut                                  string
	onVolume                                   func() // runs inside the volume step
}

func (f *fakeISOHost) run(ctx context.Context, script string, _ ...string) ([]byte, error) {
	var step string
	switch script {
	case isoQueryScript:
		step = "query"
	case isoMountScript:
		step = "mount"
	case isoVolumeScript:
		step = "volume"
	case isoDismountScript:
		step = "dismount"
	default:
		return nil, errors.New("unexpected script")
	}
	if f.ctxErrs == nil {
		f.ctxErrs = map[string][]error{}
	}
	f.ctxErrs[step] = append(f.ctxErrs[step], ctx.Err())
	f.steps = append(f.steps, step)
	switch step {
	case "query":
		mounted := false
		for _, s := range f.steps[:len(f.steps)-1] {
			mounted = mounted || s == "mount"
		}
		if mounted && f.queryErrAfterMount != nil {
			return nil, f.queryErrAfterMount
		}
		if f.queryErr != nil {
			return nil, f.queryErr
		}
		if f.attached {
			return []byte(`{"attached":true}`), nil
		}
		return []byte(`{"attached":false}`), nil
	case "mount":
		if f.mountErr != nil {
			f.attached = f.mountAttachesOnError
			return nil, f.mountErr
		}
		f.attached = true
		return nil, nil
	case "volume":
		if f.onVolume != nil {
			f.onVolume()
		}
		if f.volumeErr != nil {
			return nil, f.volumeErr
		}
		out := f.volumeOut
		if out == "" {
			out = `{"letter":"E"}`
		}
		return []byte(out), nil
	default:
		if f.dismountErr != nil {
			return nil, f.dismountErr
		}
		f.attached = false
		return nil, nil
	}
}

func (f *fakeISOHost) count(step string) int {
	n := 0
	for _, s := range f.steps {
		if s == step {
			n++
		}
	}
	return n
}

func TestMountISOAttachesAndDismountsOnce(t *testing.T) {
	f := &fakeISOHost{}
	m, err := mountISOWith(context.Background(), `C:\iso\a.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if m.Root != `E:\` || !m.ours {
		t.Fatalf("Root = %q, ours = %v", m.Root, m.ours)
	}
	if got := strings.Join(f.steps, ","); got != "query,mount,volume" {
		t.Fatalf("steps = %s", got)
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	m.release(context.Background(), logger)
	m.release(context.Background(), logger)
	if f.count("dismount") != 1 || f.attached {
		t.Errorf("dismounts = %d, attached = %v", f.count("dismount"), f.attached)
	}
}

func TestMountISOKeepsAnISOAttachedBeforeTheBuild(t *testing.T) {
	f := &fakeISOHost{attached: true}
	m, err := mountISOWith(context.Background(), `C:\iso\b.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if m.ours || f.count("mount") != 0 {
		t.Fatalf("ours = %v, mounts = %d", m.ours, f.count("mount"))
	}
	m.release(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if f.count("dismount") != 0 || !f.attached {
		t.Errorf("a pre-attached ISO was dismounted (dismounts = %d)", f.count("dismount"))
	}
}

func TestMountISOPreAttachedStaysAttachedAfterFailedDiscovery(t *testing.T) {
	f := &fakeISOHost{attached: true, volumeErr: errors.New("Get-Volume failed")}
	_, err := mountISOWith(context.Background(), `C:\iso\c.iso`, f.run)
	if err == nil || !strings.Contains(err.Error(), "Get-Volume failed") {
		t.Fatalf("err = %v", err)
	}
	if f.count("dismount") != 0 || !f.attached {
		t.Errorf("a pre-attached ISO was dismounted after a failure")
	}
}

func TestMountISODiscoveryFailuresDismount(t *testing.T) {
	volumeErr := errors.New("Get-Volume failed")
	for _, tc := range []struct {
		name string
		host *fakeISOHost
		want string
	}{
		{"volume error", &fakeISOHost{volumeErr: volumeErr}, "Get-Volume failed"},
		{"malformed JSON", &fakeISOHost{volumeOut: "not json"}, "unexpected output"},
		{"no drive letter", &fakeISOHost{volumeOut: `{"letter":""}`}, "no drive letter"},
		{"bad drive letter", &fakeISOHost{volumeOut: `{"letter":"1"}`}, "no drive letter"},
		{"long drive letter", &fakeISOHost{volumeOut: `{"letter":"EF"}`}, "no drive letter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := mountISOWith(context.Background(), `C:\iso\d.iso`, tc.host.run)
			if m != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("m = %v, err = %v", m, err)
			}
			if tc.host.count("dismount") != 1 || tc.host.attached {
				t.Errorf("dismounts = %d, attached = %v", tc.host.count("dismount"), tc.host.attached)
			}
		})
	}
}

func TestMountISOCanceledDiscoveryDismountsOnAFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeISOHost{volumeErr: context.Canceled, onVolume: cancel}
	_, err := mountISOWith(ctx, `C:\iso\e.iso`, f.run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.count("dismount") != 1 || f.attached {
		t.Fatalf("dismounts = %d, attached = %v", f.count("dismount"), f.attached)
	}
	if got := f.ctxErrs["dismount"][0]; got != nil {
		t.Errorf("the cleanup ran on a canceled context: %v", got)
	}
}

func TestMountISOCleanupFailureIsJoinedWithThePrimaryError(t *testing.T) {
	primary := errors.New("Get-Volume failed")
	cleanup := errors.New("detach failed")
	f := &fakeISOHost{volumeErr: primary, dismountErr: cleanup}
	_, err := mountISOWith(context.Background(), `C:\iso\f.iso`, f.run)
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("err = %v, want both the primary and the cleanup error", err)
	}
}

func TestMountISOMountRunsDetachedFromCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeISOHost{}
	// The query step sees the canceled context but the fake ignores it; the
	// mount step must not.
	m, err := mountISOWith(ctx, `C:\iso\g.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.ctxErrs["mount"][0]; got != nil {
		t.Errorf("the mount ran on a canceled context: %v", got)
	}
	m.release(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
}

func TestMountISOAmbiguousMountFailureIsReconciled(t *testing.T) {
	mountErr := errors.New("timed out")
	t.Run("attached despite the error", func(t *testing.T) {
		f := &fakeISOHost{mountErr: mountErr, mountAttachesOnError: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := mountISOWith(ctx, `C:\iso\h.iso`, f.run)
		if !errors.Is(err, mountErr) {
			t.Fatalf("err = %v", err)
		}
		if f.count("query") != 2 || f.count("dismount") != 1 || f.attached {
			t.Errorf("queries = %d, dismounts = %d, attached = %v", f.count("query"), f.count("dismount"), f.attached)
		}
	})
	t.Run("still detached", func(t *testing.T) {
		f := &fakeISOHost{mountErr: mountErr}
		_, err := mountISOWith(context.Background(), `C:\iso\i.iso`, f.run)
		if !errors.Is(err, mountErr) {
			t.Fatalf("err = %v", err)
		}
		if f.count("dismount") != 0 {
			t.Errorf("a detached ISO was dismounted")
		}
	})
	t.Run("state unknown", func(t *testing.T) {
		f := &fakeISOHost{mountErr: mountErr, mountAttachesOnError: true, queryErrAfterMount: errors.New("query failed")}
		_, err := mountISOWith(context.Background(), `C:\iso\j.iso`, f.run)
		if !errors.Is(err, mountErr) {
			t.Fatalf("err = %v", err)
		}
		if f.count("dismount") != 1 {
			t.Errorf("dismounts = %d, want 1: the attach state is unknown, so the build detaches", f.count("dismount"))
		}
	})
	t.Run("reconcile runs on a fresh context", func(t *testing.T) {
		f := &fakeISOHost{mountErr: mountErr, mountAttachesOnError: true}
		ctx, cancel := context.WithCancel(context.Background())
		f2 := f.run
		wrapped := func(c context.Context, script string, env ...string) ([]byte, error) {
			if script == isoMountScript {
				cancel() // the build is canceled while the mount command runs
			}
			return f2(c, script, env...)
		}
		_, err := mountISOWith(ctx, `C:\iso\k.iso`, wrapped)
		if !errors.Is(err, mountErr) {
			t.Fatalf("err = %v", err)
		}
		if got := f.ctxErrs["query"][1]; got != nil {
			t.Errorf("the re-query ran on a canceled context: %v", got)
		}
		if got := f.ctxErrs["dismount"][0]; got != nil {
			t.Errorf("the cleanup ran on a canceled context: %v", got)
		}
	})
}

func TestMountISOQueryFailureAttachesNothing(t *testing.T) {
	f := &fakeISOHost{queryErr: errors.New("Get-DiskImage failed")}
	_, err := mountISOWith(context.Background(), `C:\iso\l.iso`, f.run)
	if err == nil || f.count("mount") != 0 || f.count("dismount") != 0 {
		t.Fatalf("err = %v, steps = %v", err, f.steps)
	}
	// The path lock was released.
	f2 := &fakeISOHost{}
	m, err := mountISOWith(context.Background(), `C:\iso\l.iso`, f2.run)
	if err != nil {
		t.Fatal(err)
	}
	m.release(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
}

func TestMountISOSerializesBuildsOfOneISOPath(t *testing.T) {
	f := &fakeISOHost{}
	first, err := mountISOWith(context.Background(), `C:\iso\m.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	// A canceled wait returns without touching the attachment.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := mountISOWith(ctx, `C:\ISO\M.iso`, f.run); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overlapping mount = %v, want a deadline error", err)
	}
	if f.count("query") != 1 {
		t.Fatalf("an overlapping build queried the ISO while another build held it")
	}
	// A different path does not wait.
	other := &fakeISOHost{}
	o, err := mountISOWith(context.Background(), `C:\iso\n.iso`, other.run)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	o.release(context.Background(), logger)

	type result struct {
		m   *mountedISO
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := mountISOWith(context.Background(), `C:\iso\m.iso`, f.run)
		done <- result{m, err}
	}()
	select {
	case <-done:
		t.Fatal("the second build mounted before the first released the ISO")
	case <-time.After(50 * time.Millisecond):
	}
	first.release(context.Background(), logger)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		r.m.release(context.Background(), logger)
	case <-time.After(5 * time.Second):
		t.Fatal("the second build did not proceed after the release")
	}
	isoLocks.Lock()
	n := len(isoLocks.m)
	isoLocks.Unlock()
	if n != 0 {
		t.Errorf("%d lock entries leaked", n)
	}
}

func TestMountISOFailedDismountKeepsTheLockUntilItSucceeds(t *testing.T) {
	f := &fakeISOHost{}
	m, err := mountISOWith(context.Background(), `C:\iso\o.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	f.dismountErr = errors.New("detach failed")
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	m.release(context.Background(), logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := mountISOWith(ctx, `C:\iso\o.iso`, f.run); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mount while the ISO stays attached = %v", err)
	}
	f.dismountErr = nil
	m.release(context.Background(), logger)
	m2, err := mountISOWith(context.Background(), `C:\iso\o.iso`, f.run)
	if err != nil {
		t.Fatal(err)
	}
	m2.release(context.Background(), logger)
}
