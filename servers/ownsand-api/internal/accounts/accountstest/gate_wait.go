// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"fmt"
	"testing"
	"time"
)

const gateWaitTimeout = 5 * time.Second

// WaitForEntry fails the test if the worker returns or the gate times out
// before the RPC reaches it.
func (g *Gate) WaitForEntry(t testing.TB, results <-chan error) {
	t.Helper()
	if err := g.AwaitEntry(results, gateWaitTimeout); err != nil {
		t.Fatal(err)
	}
}

// AwaitEntry waits up to timeout for the RPC to reach the gate. If the worker
// returns first, AwaitEntry consumes and reports its result, even if it is nil.
func (g *Gate) AwaitEntry(results <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-g.Entered:
		return nil
	case err := <-results:
		return fmt.Errorf("worker completed before %s entered the gate (result: %v)", g.method, err)
	case <-timer.C:
		return fmt.Errorf("timed out waiting for %s to enter the gate", g.method)
	}
}

// WaitForResult waits for the worker that calls the held method to finish.
func (g *Gate) WaitForResult(t testing.TB, done <-chan error) error {
	t.Helper()
	timer := time.NewTimer(gateWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		t.Fatalf("timed out waiting for %s worker result", g.method)
		return nil
	}
}
