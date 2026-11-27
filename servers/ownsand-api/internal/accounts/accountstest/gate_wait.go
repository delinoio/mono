// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"testing"
	"time"
)

const gateWaitTimeout = 5 * time.Second

// WaitForEntry waits for the RPC to reach the gate. If the worker has no
// error result, it can close done on exit.
func (g *Gate) WaitForEntry(t testing.TB, done <-chan error) {
	t.Helper()
	timer := time.NewTimer(gateWaitTimeout)
	defer timer.Stop()
	select {
	case <-g.Entered:
	case err := <-done:
		t.Fatalf("worker completed before %s entered the gate (result: %v)", g.method, err)
	case <-timer.C:
		t.Fatalf("timed out waiting for %s to enter the gate", g.method)
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
