// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"net"
	"testing"
	"time"
)

// TestFixtureIgnoresInheritedMetricsAddr checks that a METRICS_ADDR inherited
// from the developer's environment (for example from a running local dev
// server) neither blocks the control plane of the fixture nor makes it bind
// the inherited address: the fixture starts the control plane with the
// metrics listener disabled.
func TestFixtureIgnoresInheritedMetricsAddr(t *testing.T) {
	requireE2E(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := held.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Setenv("METRICS_ADDR", held.Addr().String())

	s := newStack(t, stackOptions{noSandboxes: true})

	// "server_started" carries the address of the metrics listener; it is
	// empty when the listener is off.
	if !s.apiHasLineContaining("server_started") {
		t.Fatal("the control plane did not log server_started")
	}
	if s.apiHasLineContaining(held.Addr().String()) {
		t.Errorf("the control plane used the inherited metrics address %s", held.Addr())
	}
	// A restart reuses the environment of the first start (and a start that
	// is not isolated would fail on the held address).
	s.killAPI()
	s.restartAPI()
	select {
	case <-accepted:
		t.Error("the control plane connected to the inherited metrics address")
	case <-time.After(100 * time.Millisecond):
	}
}
