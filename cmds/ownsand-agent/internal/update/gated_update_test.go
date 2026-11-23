// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// startUpdateWorker runs one update command on its own goroutine and returns
// the channel that receives its result and the function that cancels its
// context. The test cleanup cancels the context and then joins the goroutine,
// so a failed assertion cannot leave a worker running after the test. Call it
// after newGateTransport: cleanups run last-in first-out, so the worker is
// released before the gate is opened.
func startUpdateWorker(t *testing.T, u *Updater, cmd *ownsandv1.UpdateAgentCommand, order uint64) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- u.HandleOrdered(ctx, cmd, order)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(gateTimeout):
			t.Errorf("the update worker did not exit within %v of its cancellation", gateTimeout)
		}
	})
	return done, cancel
}

// statusTransport answers one request path with a fixed HTTP status and
// passes every other request to base.
type statusTransport struct {
	base   http.RoundTripper
	path   string
	status int
	hits   atomic.Int32
}

func (s *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == s.path {
		s.hits.Add(1)
		return &http.Response{
			StatusCode: s.status,
			Status:     http.StatusText(s.status),
			Header:     http.Header{},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	return s.base.RoundTrip(req)
}

var (
	errUpdateEndedEarly = errors.New("the update finished before the download started")
	errDownloadTimeout  = errors.New("the download did not start in time")
)

// waitStarted waits until the gated download starts. It returns an error that
// wraps the update's own result when the update ends first, and
// errDownloadTimeout when neither happens within timeout.
func waitStarted(started <-chan struct{}, done <-chan error, timeout time.Duration) error {
	select {
	case <-started:
		return nil
	case err := <-done:
		return fmt.Errorf("%w: %w", errUpdateEndedEarly, err)
	case <-time.After(timeout):
		return fmt.Errorf("%w: %v", errDownloadTimeout, timeout)
	}
}

// awaitStartedOrEnd fails the test when the gated download does not begin in
// time. Unlike awaitStarted it also watches done, so an update that ends
// before the artifact request fails at once with its own result instead of
// after the timeout.
func (g *gateTransport) awaitStartedOrEnd(t *testing.T, done <-chan error) {
	t.Helper()
	if err := waitStarted(g.started, done, gateTimeout); err != nil {
		t.Fatal(err)
	}
}

// TestGateFailsFastWhenTheManifestFails checks that an update error before the
// artifact request is reported at once with the original error, and not after
// the gate timeout.
func TestGateFailsFastWhenTheManifestFails(t *testing.T) {
	r := newRelease(t, nil)
	u, _, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	manifest := &statusTransport{base: r.srv.Client().Transport, path: "/manifest.json", status: http.StatusInternalServerError}
	gate := newGateTransport(t, manifest)
	u.opts.HTTP = &http.Client{Transport: gate}

	done, _ := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
	err := waitStarted(gate.started, done, gateTimeout)
	if !errors.Is(err, errUpdateEndedEarly) {
		t.Fatalf("waiting for the download: %v, want the update's own error", err)
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("the original manifest error is missing: %v", err)
	}
	if manifest.hits.Load() == 0 {
		t.Fatal("the manifest was never requested")
	}
}

// TestCancelAtTheGateReleasesTheWorker checks that cancelling the context
// while the artifact request is held at the gate ends the update within the
// deadline, without ever opening the gate, and that the cleanup can join it.
func TestCancelAtTheGateReleasesTheWorker(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, _ := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	gate := newGateTransport(t, r.srv.Client().Transport)
	u.opts.HTTP = &http.Client{Transport: gate}

	done, cancel := startUpdateWorker(t, u, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}, u.NextOrder())
	gate.awaitStartedOrEnd(t, done)
	cancel()
	if err := awaitDone(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled download finished with %v, want context.Canceled", err)
	}
	if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 {
		t.Fatalf("installed after a cancelled download: %v %v", ok, err)
	}
}
