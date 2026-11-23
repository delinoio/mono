// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// errGatheringTimeout reports that ICE gathering did not complete in time.
var errGatheringTimeout = errors.New("ICE gathering did not complete before the deadline")

// offerPeer is the part of a peer connection that createLocalOffer uses. The
// seam lets a unit test drive the failure and timeout paths, which a real
// WebRTC peer connection does not produce on demand.
type offerPeer interface {
	CreateOffer(*webrtc.OfferOptions) (webrtc.SessionDescription, error)
	SetLocalDescription(webrtc.SessionDescription) error
	// GatheringComplete returns a channel that closes when ICE gathering
	// ends. It must be called before SetLocalDescription starts gathering.
	GatheringComplete() <-chan struct{}
	LocalDescription() *webrtc.SessionDescription
}

// pionOfferPeer adapts a pion peer connection to offerPeer.
type pionOfferPeer struct{ *webrtc.PeerConnection }

func (p pionOfferPeer) GatheringComplete() <-chan struct{} {
	return webrtc.GatheringCompletePromise(p.PeerConnection)
}

// createLocalOffer creates an offer, sets it as the local description and
// waits for ICE gathering, so that the returned SDP carries every candidate.
// A CreateOffer or SetLocalDescription error returns at once, before any wait
// for gathering; the wait ends with errGatheringTimeout after gatherTimeout.
func createLocalOffer(peer offerPeer, gatherTimeout time.Duration) (string, error) {
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	gathered := peer.GatheringComplete()
	if err := peer.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	timer := time.NewTimer(gatherTimeout)
	defer timer.Stop()
	select {
	case <-gathered:
	case <-timer.C:
		return "", fmt.Errorf("wait for ICE gathering: %w", errGatheringTimeout)
	}
	local := peer.LocalDescription()
	if local == nil {
		return "", errors.New("local description is empty after ICE gathering")
	}
	return local.SDP, nil
}

// fakeOfferPeer scripts the calls of createLocalOffer.
type fakeOfferPeer struct {
	createErr, setErr error
	gathered          chan struct{}
	sdp               string
	calls             []string
}

func (f *fakeOfferPeer) CreateOffer(*webrtc.OfferOptions) (webrtc.SessionDescription, error) {
	f.calls = append(f.calls, "CreateOffer")
	return webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: f.sdp}, f.createErr
}

func (f *fakeOfferPeer) SetLocalDescription(webrtc.SessionDescription) error {
	f.calls = append(f.calls, "SetLocalDescription")
	return f.setErr
}

func (f *fakeOfferPeer) GatheringComplete() <-chan struct{} {
	f.calls = append(f.calls, "GatheringComplete")
	return f.gathered
}

func (f *fakeOfferPeer) LocalDescription() *webrtc.SessionDescription {
	return &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: f.sdp}
}

// An operation error is reported as that error, not as a gathering timeout:
// the gathering channel stays open in the error cases, so an implementation
// that kept waiting would report errGatheringTimeout instead.
func TestCreateLocalOfferOperationErrors(t *testing.T) {
	createErr := errors.New("create refused")
	setErr := errors.New("set refused")
	tests := []struct {
		name      string
		peer      *fakeOfferPeer
		wantErr   error
		wantOp    string
		wantCalls string
	}{
		{"create offer", &fakeOfferPeer{createErr: createErr, gathered: make(chan struct{})}, createErr, "create offer", "CreateOffer"},
		{"set local description", &fakeOfferPeer{setErr: setErr, gathered: make(chan struct{})}, setErr, "set local description", "CreateOffer,GatheringComplete,SetLocalDescription"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sdp, err := createLocalOffer(tc.peer, 50*time.Millisecond)
			if !errors.Is(err, tc.wantErr) || errors.Is(err, errGatheringTimeout) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), tc.wantOp+": ") {
				t.Fatalf("error %q does not name the operation %q", err, tc.wantOp)
			}
			if sdp != "" {
				t.Fatalf("sdp %q on error", sdp)
			}
			if got := strings.Join(tc.peer.calls, ","); got != tc.wantCalls {
				t.Fatalf("calls %s, want %s", got, tc.wantCalls)
			}
		})
	}
}

// Gathering that never completes ends with errGatheringTimeout.
func TestCreateLocalOfferGatheringTimeout(t *testing.T) {
	peer := &fakeOfferPeer{gathered: make(chan struct{}), sdp: "v=0"}
	sdp, err := createLocalOffer(peer, time.Millisecond)
	if !errors.Is(err, errGatheringTimeout) {
		t.Fatalf("error %v, want a gathering timeout", err)
	}
	if sdp != "" {
		t.Fatalf("sdp %q on timeout", sdp)
	}
}

// Completed gathering returns the local SDP.
func TestCreateLocalOfferSuccess(t *testing.T) {
	gathered := make(chan struct{})
	close(gathered)
	peer := &fakeOfferPeer{gathered: gathered, sdp: "v=0 fixture"}
	sdp, err := createLocalOffer(peer, time.Minute)
	if err != nil || sdp != "v=0 fixture" {
		t.Fatalf("createLocalOffer = %q, %v", sdp, err)
	}
	if got := strings.Join(peer.calls, ","); got != "CreateOffer,GatheringComplete,SetLocalDescription" {
		t.Fatalf("calls %s", got)
	}
}
