// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Serve the signed release in memory so synctest can advance the retry timer.
type artifactRetryTransport struct {
	release       *release
	artifact      func(attempt int) (status int, body io.Reader, err error)
	manifestGets  int
	signatureGets int
	artifactGets  int
}

func (tr *artifactRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, io.Reader(http.NoBody)
	switch req.URL.Path {
	case "/manifest.json":
		tr.manifestGets++
		body = bytes.NewReader(tr.release.manifest)
	case "/manifest.json.sig":
		tr.signatureGets++
		body = bytes.NewReader([]byte(base64.StdEncoding.EncodeToString(tr.release.sig)))
	case "/agent":
		tr.artifactGets++
		var err error
		status, body, err = tr.artifact(tr.artifactGets)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(body)}, nil
}

func TestArtifactDownloadAttempts(t *testing.T) {
	cases := []struct {
		name         string
		firstStatus  int
		secondStatus int
		transportErr error
		bodyErr      error
		badHash      bool
		wantErr      error
		wantGets     int
	}{
		{name: "success", wantGets: 1},
		{name: "408", firstStatus: http.StatusRequestTimeout, wantGets: 2},
		{name: "429", firstStatus: http.StatusTooManyRequests, wantGets: 2},
		{name: "500", firstStatus: http.StatusInternalServerError, wantGets: 2},
		{name: "502", firstStatus: http.StatusBadGateway, wantGets: 2},
		{name: "503", firstStatus: http.StatusServiceUnavailable, wantGets: 2},
		{name: "504", firstStatus: http.StatusGatewayTimeout, wantGets: 2},
		{name: "timeout", transportErr: os.ErrDeadlineExceeded, wantGets: 2},
		{name: "short body", bodyErr: io.ErrUnexpectedEOF, wantGets: 2},
		{name: "404", firstStatus: http.StatusNotFound, wantErr: downloadStatusError(http.StatusNotFound), wantGets: 1},
		{name: "bad hash", badHash: true, wantErr: ErrHashMismatch, wantGets: 1},
		{name: "last failure", firstStatus: http.StatusServiceUnavailable, secondStatus: http.StatusBadGateway, wantErr: downloadStatusError(http.StatusBadGateway), wantGets: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			synctest.Test(t, func(t *testing.T) {
				busy := &atomic.Bool{}
				busy.Store(true)
				u, installer, _ := newUpdater(t, r, "1.0.0", busy)
				tr := &artifactRetryTransport{release: r, artifact: func(attempt int) (int, io.Reader, error) {
					if attempt == 1 {
						switch {
						case tc.firstStatus != 0:
							return tc.firstStatus, http.NoBody, nil
						case tc.transportErr != nil:
							return 0, nil, tc.transportErr
						case tc.bodyErr != nil:
							return http.StatusOK, iotest.ErrReader(tc.bodyErr), nil
						case tc.badHash:
							return http.StatusOK, bytes.NewBufferString("tampered"), nil
						}
					} else if tc.secondStatus != 0 {
						return tc.secondStatus, http.NoBody, nil
					}
					return http.StatusOK, bytes.NewReader(r.artifact), nil
				}}
				u.opts.HTTP = &http.Client{Transport: tr}
				start := time.Now()
				if err := update(u, "1.1.0", false); !errors.Is(err, tc.wantErr) {
					t.Fatalf("update error = %v, want %v", err, tc.wantErr)
				}
				if tr.artifactGets != tc.wantGets || tr.manifestGets != 1 || tr.signatureGets != 1 {
					t.Fatalf("GETs = manifest %d, signature %d, artifact %d", tr.manifestGets, tr.signatureGets, tr.artifactGets)
				}
				if elapsed := time.Since(start); elapsed != time.Duration(tc.wantGets-1)*time.Second {
					t.Fatalf("update took %v with %d artifact requests", elapsed, tc.wantGets)
				}
				if tc.wantErr != nil {
					if state, _ := u.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED || u.artifact != "" {
						t.Fatalf("failed update left state %v, artifact %q", state, u.artifact)
					}
					return
				}
				if state, target := u.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE || target != "1.1.0" || u.artifact == "" {
					t.Fatalf("queued update = (%v, %q), artifact %q", state, target, u.artifact)
				}
				if ok, err := u.TryInstall(); ok || err != nil {
					t.Fatalf("install while busy = (%t, %v)", ok, err)
				}
				busy.Store(false)
				if ok, err := u.TryInstall(); !ok || err != nil || installer.installed.Load() != 1 {
					t.Fatalf("install while idle = (%t, %v), calls %d", ok, err, installer.installed.Load())
				}
			})
		})
	}
}

func TestArtifactRetryWait(t *testing.T) {
	for _, name := range []string{"cancellation", "withdrawal", "supersession", "removal", "revocation"} {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, nil)
			synctest.Test(t, func(t *testing.T) {
				running := "1.0.0"
				if name == "revocation" {
					running = "0.9.0"
				}
				u, installer, _ := newUpdater(t, r, running, &atomic.Bool{})
				tr := &artifactRetryTransport{release: r, artifact: func(int) (int, io.Reader, error) {
					return http.StatusServiceUnavailable, http.NoBody, nil
				}}
				u.opts.HTTP = &http.Client{Transport: tr}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					done <- u.Handle(ctx, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"})
				}()
				synctest.Wait()
				if tr.artifactGets != 1 || len(done) != 0 {
					t.Fatalf("retry wait: artifact GETs %d, command finished %t", tr.artifactGets, len(done) != 0)
				}
				wantGets := 1
				var wantErr error
				switch name {
				case "cancellation":
					cancel()
					wantErr = context.Canceled
				case "withdrawal":
					if err := u.Withdraw(); err != nil {
						t.Fatal(err)
					}
					wantErr = ErrSuperseded
				case "supersession":
					if err := update(u, running, false); err != nil {
						t.Fatal(err)
					}
					wantErr = ErrSuperseded
				case "removal":
					u.FenceForRemoval()
					wantErr = ErrRemovalAborted
				case "revocation":
					checkRevokedRetryGate(t, u)
					wantErr = downloadStatusError(http.StatusServiceUnavailable)
					wantGets = 2
				}
				if err := <-done; !errors.Is(err, wantErr) {
					t.Fatalf("update error = %v, want %v", err, wantErr)
				}
				if tr.artifactGets != wantGets || u.artifact != "" {
					t.Fatalf("artifact GETs = %d, want %d, queued artifact = %q", tr.artifactGets, wantGets, u.artifact)
				}
				if name == "revocation" {
					checkRevokedRetryGate(t, u)
				}
				if ok, err := u.TryInstall(); ok || err != nil || installer.installed.Load() != 0 {
					t.Fatalf("install = (%t, %v), calls %d", ok, err, installer.installed.Load())
				}
			})
		})
	}
}

func checkRevokedRetryGate(t *testing.T, u *Updater) {
	t.Helper()
	if state, target := u.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("revoked update state = (%v, %q)", state, target)
	}
	if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
		t.Fatalf("create on revoked version = %v", err)
	}
}
