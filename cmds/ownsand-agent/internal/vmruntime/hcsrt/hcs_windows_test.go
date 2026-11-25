// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsNotFoundOrStopped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		// HcsOpenComputeSystem of computecore.dll returns 0x8037010e for an
		// unknown compute system ID.
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0x8037010e}, true},
		{&hcsError{op: "HcsTerminateComputeSystem", hr: 0x80370110}, true},
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0xc037010e}, true},
		{&hcsError{op: "HcsTerminateComputeSystem", hr: 0xc0370110}, true},
		{fmt.Errorf("wrapped: %w", &hcsError{op: "HcsOpenComputeSystem", hr: 0x8037010e}), true},
		{&hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}, false},
		{errors.New("other"), false},
	} {
		if got := isNotFoundOrStopped(tc.err); got != tc.want {
			t.Errorf("isNotFoundOrStopped(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestConfirmedSystemExit(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       bool
		wantErr    bool
	}{
		{"absent", `[]`, true, false},
		{"stopped", `[{"Id":"OWNSAND-VM","State":"Stopped"}]`, true, false},
		{"stopped flag", `[{"Id":"ownsand-vm","Stopped":true}]`, true, false},
		{"running", `[{"Id":"ownsand-vm","State":"Running"}]`, false, false},
		{"paused", `[{"Id":"ownsand-vm","State":"Paused"}]`, false, false},
		{"created", `[{"Id":"ownsand-vm","State":"Created","Stopped":true}]`, false, false},
		{"unknown state", `[{"Id":"ownsand-vm"}]`, false, false},
		{"missing result", `null`, false, true},
		{"wrong ID", `[{"Id":"another-vm","State":"Stopped"}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var list []systemProperties
			if err := json.Unmarshal([]byte(tc.json), &list); err != nil {
				t.Fatal(err)
			}
			got, err := confirmedSystemExit("ownsand-vm", list)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("confirmedSystemExit = %v, %v, want confirmed %v and error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestVMWatcherReleasesHandleDuringStatusQuery(t *testing.T) {
	sys := fakeSystem(neverExits)
	registerFakeCallback(t, sys)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	sys.verifyExitFn = func(string, time.Time) (bool, error) {
		close(started)
		<-release
		return false, nil
	}
	v := &vm{exited: make(chan struct{})}
	v.attachSystem(sys)
	notifySystem(sys, hcsEventServiceDisconnect)
	waitForSignal(t, started, "status query")
	sys.close()
	waitForSignal(t, v.watchDone, "exit watcher")
	requireOpenSignal(t, v.Exited(), "VM exit")
	notifySystem(sys, hcsEventSystemExited)
	requireOpenSignal(t, sys.exited, "system exit after callback registration removal")
}

func TestWaitExitBoundsDisconnectQuery(t *testing.T) {
	sys := fakeSystem(neverExits)
	registerFakeCallback(t, sys)
	type request struct {
		id       string
		deadline time.Time
	}
	query := make(chan request, 1)
	sys.verifyExitFn = func(id string, deadline time.Time) (bool, error) {
		query <- request{id, deadline}
		return false, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	wantDeadline, _ := ctx.Deadline()
	notifySystem(sys, hcsEventServiceDisconnect)
	exited := make(chan error, 1)
	go func() { exited <- sys.waitExit(ctx) }()
	select {
	case got := <-query:
		if got.id != sys.id || !got.deadline.Equal(wantDeadline) {
			t.Errorf("query = %+v, want ID %s and deadline %v", got, sys.id, wantDeadline)
		}
	case <-ctx.Done():
		t.Fatal("waitExit did not query HCS after disconnect")
	}
	notifySystem(sys, hcsEventSystemExited)
	if err := <-exited; err != nil {
		t.Fatalf("waitExit = %v after exit notification", err)
	}
}
