// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/delinoio/mono/go/uuidv7"
)

// gatedBucket serves a bucket whose hook runs the gate and closes done once
// the hook returned for the first request of the gate's snapshot.
func gatedBucket(t *testing.T, gate *getGate) (url string, done <-chan struct{}) {
	t.Helper()
	finished := make(chan struct{})
	var once sync.Once
	b := bucketWithHook(t, func(r *http.Request) int {
		status := gate.hook(r)
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/"+gate.id+"/") {
			once.Do(func() { close(finished) })
		}
		return status
	})
	return b.URL, finished
}

func getWithContext(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// TestGetGateEndsWhenTheRequestIsCanceled checks that a blocked download ends
// when its client goes away, without any release of the gate.
func TestGetGateEndsWhenTheRequestIsCanceled(t *testing.T) {
	id, _ := uuidv7.NewString()
	gate := newGetGate(id)
	url, done := gatedBucket(t, gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- getWithContext(ctx, url+"/"+id+"/1") }()
	waitClosed(t, gate.started, "the gated download")
	select {
	case <-done:
		t.Fatal("the hook ended before the request was canceled or the gate released")
	default:
	}
	cancel()
	waitClosed(t, done, "the hook to end after the cancellation")
	<-errc
	select {
	case <-gate.release:
		t.Fatal("the cancellation released the gate")
	default:
	}
}

// TestGetGateWaitsForReleaseAndPassesOtherRequests checks that the first
// matching download waits for the release, that releasing twice is safe, and
// that other requests and later downloads do not wait.
func TestGetGateWaitsForReleaseAndPassesOtherRequests(t *testing.T) {
	id, _ := uuidv7.NewString()
	other, _ := uuidv7.NewString()
	gate := newGetGate(id)
	url, done := gatedBucket(t, gate)
	t.Cleanup(gate.Release)
	errc := make(chan error, 1)
	go func() { errc <- getWithContext(context.Background(), url+"/"+id+"/1") }()
	waitClosed(t, gate.started, "the gated download")
	// Requests that are not the first matching GET pass while it waits.
	if err := getWithContext(context.Background(), url+"/"+other+"/1"); err != nil {
		t.Fatalf("request for another snapshot: %v", err)
	}
	select {
	case <-done:
		t.Fatal("the first download did not wait for the release")
	default:
	}
	gate.Release()
	gate.Release()
	waitClosed(t, done, "the download after the release")
	if err := <-errc; err != nil {
		t.Fatalf("gated download: %v", err)
	}
	if err := getWithContext(context.Background(), url+"/"+id+"/2"); err != nil {
		t.Fatalf("later download: %v", err)
	}
	if got := gate.count(); got != 2 {
		t.Fatalf("gets = %d, want 2", got)
	}
}

// TestGatedImportFixtureReleasesTheGateOnCleanup checks that a test that
// ends while a download waits at the gate does not strand the teardown: the
// cleanup registered by the fixture releases it before the bucket and the
// agent wait for it.
func TestGatedImportFixtureReleasesTheGateOnCleanup(t *testing.T) {
	id, _ := uuidv7.NewString()
	var gate *getGate
	t.Run("body exits with a blocked download", func(t *testing.T) {
		g, f := newGatedImportFixture(t, id, nil)
		gate = g
		f.h.sendCommand(f.stage(t, id, false))
		waitClosed(t, gate.started, "the first download")
		// The subtest ends here without releasing the gate.
	})
	select {
	case <-gate.release:
	default:
		t.Fatal("the fixture cleanup did not release the gate")
	}
}
