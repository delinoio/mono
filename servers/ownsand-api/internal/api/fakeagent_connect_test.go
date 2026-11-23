// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// failureRecorder is a testing.TB for a fake agent that connects on its own
// goroutine. A fatal failure of the agent's wait is recorded and ends that
// goroutine, instead of calling FailNow away from the test goroutine, so the
// test asserts the failure itself.
type failureRecorder struct {
	testing.TB
	mu    sync.Mutex
	fatal string
}

func (r *failureRecorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatal = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	runtime.Goexit()
}

func (r *failureRecorder) failure() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatal
}

// connectInBackground runs agent.Connect on its own goroutine with a failure
// recorder and returns a channel that closes when Connect ended, by returning
// or by a recorded failure.
func connectInBackground(agent *testenv.FakeAgent, t testing.TB) (<-chan struct{}, *failureRecorder) {
	rec := &failureRecorder{TB: t}
	agent.T = rec
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Connect()
	}()
	return done, rec
}

func awaitDone(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestConnectOfReplacementWaitsForItsOwnReconcile checks that Connect of a
// replacement agent is not satisfied by the earlier channel of the device,
// which stays live in the hub until the control plane admits the
// replacement. The admission is paused, so the earlier channel is live for
// as long as the test wants. Connect must stay pending then, and return only
// after the server acknowledged the replacement's own state report and the
// replacement channel is the live channel of the device.
func TestConnectOfReplacementWaitsForItsOwnReconcile(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev, original := setupAgent(t, env, org, nil)
	hub := env.App.Service.Hub
	before := hub.Live(dev.ID)
	if before == nil {
		t.Fatal("the original channel is not live")
	}

	gate := env.GateChannelAdmission()
	gate.Arm()
	replacement := env.NewFakeAgent(dev)
	done, rec := connectInBackground(replacement, t)

	select {
	case <-gate.Entered():
	case <-done:
		t.Fatalf("Connect ended before the admission of the replacement was paused: %q", rec.failure())
	case <-time.After(30 * time.Second):
		t.Fatal("the replacement channel did not reach its admission")
	}
	// The admission is paused, so only this window can end Connect early. A
	// Connect that waits for the device alone returns at once, because the
	// original channel is live. The window decides nothing else: a slow
	// machine only makes the check weaker, never fails it.
	select {
	case <-done:
		t.Fatalf("Connect returned while the replacement was not admitted (failure %q)", rec.failure())
	case <-time.After(250 * time.Millisecond):
	}
	if hub.Live(dev.ID) != before {
		t.Fatal("the original channel is not the live channel while the replacement is paused")
	}
	select {
	case <-original.Closed():
		t.Fatal("the original channel closed before the replacement was admitted")
	default:
	}
	if acks := stateAcks(replacement); len(acks) != 0 {
		t.Fatalf("state acknowledgements before the admission = %v, want none", acks)
	}

	gate.Release()
	awaitDone(t, "Connect of the replacement", done)
	if msg := rec.failure(); msg != "" {
		t.Fatalf("Connect of the replacement failed: %s", msg)
	}
	if acks := stateAcks(replacement); len(acks) != 1 {
		t.Fatalf("state acknowledgements of the replacement = %v, want one", acks)
	}
	live := hub.Live(dev.ID)
	if live == nil || live == before {
		t.Fatal("the replacement channel is not the live channel of the device after Connect")
	}
	select {
	case <-original.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("the original channel stayed open after the replacement was admitted")
	}
}

// TestConnectOfFirstChannelBecomesReady checks that a device without an
// earlier channel is ready after Connect: its own state report was
// acknowledged and its channel is live.
func TestConnectOfFirstChannelBecomesReady(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	if env.App.Service.Hub.Get(dev.ID) != nil {
		t.Fatal("the device has a channel before it connected")
	}
	agent := env.NewFakeAgent(dev)
	agent.Connect()
	if acks := stateAcks(agent); len(acks) != 1 {
		t.Fatalf("state acknowledgements = %v, want one", acks)
	}
	if env.App.Service.Hub.Live(dev.ID) == nil {
		t.Fatal("the device is not live after Connect")
	}
}

// TestConnectWithoutAcknowledgementFails checks that a connection whose
// first state report is never acknowledged cannot end in a successful
// Connect. The first report fails in the database, so the server closes the
// channel without an acknowledgement, and Connect fails when it sees the
// close.
func TestConnectWithoutAcknowledgementFails(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
	fault := initialStateReportFault(t, env)
	fault.on("40001", 1)

	agent := env.NewFakeAgent(dev)
	done, rec := connectInBackground(agent, t)
	awaitDone(t, "the failure of Connect", done)

	msg := rec.failure()
	if !strings.Contains(msg, "the channel closed while waiting for") {
		t.Fatalf("Connect did not fail on the closed channel: %q", msg)
	}
	if acks := stateAcks(agent); len(acks) != 0 {
		t.Fatalf("state acknowledgements = %v, want none", acks)
	}
	if env.App.Service.Hub.Live(dev.ID) != nil {
		t.Fatal("the device is live after its first state report failed")
	}
}
