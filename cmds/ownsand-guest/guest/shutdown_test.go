// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// shutdownRig is a server whose Run the test ends itself.
type shutdownRig struct {
	srv    *Server
	client ownsandv1connect.GuestServiceClient
	stop   context.CancelFunc
	// result receives the return value of Run.
	result chan error
}

func startShutdownRig(t *testing.T) *shutdownRig {
	t.Helper()
	srv, err := New(Config{StateDir: t.TempDir(), Version: "test", Clock: ClockModeIgnore, KeepaliveInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- srv.Run(ctx, l)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return at cleanup")
		}
		srv.execs.wg.Wait()
		// Release the state directory lock: Windows cannot delete the open
		// lock file when t.TempDir removes the state directory. This
		// cleanup runs after the one of t.TempDir was registered, so it
		// runs before it. Close is safe to call more than once.
		srv.Close()
	})
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", l.Addr().String())
		},
	}}
	return &shutdownRig{srv: srv, client: ownsandv1connect.NewGuestServiceClient(hc, "http://guest"), stop: cancel, result: result}
}

// setShutdownGrace replaces the grace period for the test.
func setShutdownGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := shutdownGrace
	shutdownGrace = d
	t.Cleanup(func() { shutdownGrace = orig })
}

func TestRunReturnsPromptlyWhenIdle(t *testing.T) {
	rig := startShutdownRig(t)
	start := time.Now()
	rig.stop()
	select {
	case err := <-rig.result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for an idle server")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle Run took %v to return", d)
	}
}
