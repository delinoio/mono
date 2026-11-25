// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// requireRefusedOffer fails unless the answer is the error answer of an offer
// whose session ended before it was answered.
func requireRefusedOffer(t *testing.T, a *ownsandv1.SignalingAnswer) {
	t.Helper()
	if a.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION || a.GetSdp() != "" {
		t.Fatalf("answer %v, want a failed_precondition error answer without an SDP body", a)
	}
}

// A revocation that reaches the agent before the offer reserves a slot
// is remembered: the offer is refused, no session or peer connection remains
// and the tombstone is consumed.
func TestRevokeBeforeOfferRefusesTheOffer(t *testing.T) {
	w := newWorld(t)
	dp := newPlane(w, Timing{})
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	_, sdp := newCaller(t, DataChannelLabel)
	id := newID(t)
	dp.Revoke(ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
	offer := &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp}
	requireRefusedOffer(t, dp.Offer(context.Background(), ch, offer))
	requireNoSession(t, dp)
	dp.mu.Lock()
	n := dp.revoked.len()
	dp.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d tombstones left, want the offer to consume its own", n)
	}
	// A revocation of one session ID does not affect another offer.
	w.session(t, dp, ch, time.Hour)
	if dp.sessionCount() != 1 {
		t.Fatalf("sessions after an unrelated offer = %d, want 1", dp.sessionCount())
	}
}

// statesFunc adapts a function to the sandbox-state source of the data plane.
type statesFunc func(sandboxID string) (ownsandv1.SandboxState, bool)

func (f statesFunc) State(sandboxID string) (ownsandv1.SandboxState, bool) { return f(sandboxID) }

// A revocation while Offer reads the sandbox state cancels the reservation
// before Offer constructs a peer connection.
func TestRevokeWhileCheckingSandboxState(t *testing.T) {
	w := newWorld(t)
	var dp *DataPlane
	var ch *fakeChannel
	id := newID(t)
	dp = New(Options{Dispatcher: w.d, Logger: logger(), IncludeLoopback: true, InterfaceFilter: loopbackOnly,
		Sandboxes: statesFunc(func(string) (ownsandv1.SandboxState, bool) {
			dp.Revoke(ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
			return ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, true
		})})
	t.Cleanup(dp.Close)
	ch = newChannel(1)
	dp.ChannelOpened(ch)
	_, sdp := newCaller(t, DataChannelLabel)
	offer := &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp}
	requireRefusedOffer(t, dp.Offer(context.Background(), ch, offer))
	requireNoSession(t, dp)
	dp.mu.Lock()
	n := dp.revoked.len()
	dp.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d tombstones left, want the offer to consume its own", n)
	}
}
