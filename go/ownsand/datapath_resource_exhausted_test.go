// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// resourceExhaustedStatus is the status that the control plane forwards when
// the agent answers an offer with ERROR_CODE_RESOURCE_EXHAUSTED because its
// peer-to-peer session capacity is reached.
func resourceExhaustedStatus() error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New("agent peer-to-peer session capacity reached"))
}

// TestSetupResourceExhaustedMovesToRelay checks that a resource_exhausted
// status from GetIceServers or OpenSession is a setup failure like any other:
// with the automatic transport the operation succeeds on the relay and the
// handle moves to the relay.
func TestSetupResourceExhaustedMovesToRelay(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageOpenSession} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			failSetupStage(cp, stage, resourceExhaustedStatus())
			s := cp.handle(t, cp.client(t, TransportAuto))

			res, err := execOnce(t, s)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if res.Transport != relayTransport {
				t.Fatalf("transport %v, want relay", res.Transport)
			}
			if !s.usesRelay() {
				t.Fatal("the handle did not move to the relay")
			}
			if n := len(cp.authRequests()); n == 0 {
				t.Fatal("the relay was not used")
			}
		})
	}
}

// TestForcedPeerToPeerSetupResourceExhaustedReturnsTheError checks that a
// forced peer-to-peer handle returns the resource_exhausted status with its
// code and asks for no relay grant.
func TestForcedPeerToPeerSetupResourceExhaustedReturnsTheError(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageOpenSession} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			failSetupStage(cp, stage, resourceExhaustedStatus())
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))

			_, err := execOnce(t, s)
			if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
				t.Fatalf("code %v, want resource_exhausted (err %v)", got, err)
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls, want 0", n)
			}
		})
	}
}
