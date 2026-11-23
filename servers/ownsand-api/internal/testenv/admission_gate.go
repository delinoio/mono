// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv

import "sync"

// AdmissionGate pauses the admission of agent channels at the control plane,
// before the channel is registered in the hub. While a channel is paused,
// the channel that the device already has stays the current channel of the
// hub, so a test can observe what a replacement connection does before the
// server admitted it.
type AdmissionGate struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// GateChannelAdmission installs a gate on the agent channel admission of the
// control plane. The gate is open until Arm is called, and a cleanup of the
// test releases it, so a paused admission never outlives the test.
//
// It replaces Service.BaselineReady, which only the channel admission reads,
// so call it while no channel is being admitted, before the connection that
// the test wants to pause. The admission calls BaselineReady with the lease
// ownership lock held, so a paused admission holds that lock and stalls the
// lease renewal and Held until Release. That is acceptable for the short
// pause of a test, because the lease lasts much longer than a test waits,
// but a test must not keep the gate armed for long.
func (e *Env) GateChannelAdmission() *AdmissionGate {
	e.T.Helper()
	svc := e.App.Service
	g := &AdmissionGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	next := svc.BaselineReady
	svc.BaselineReady = func() bool {
		g.mu.Lock()
		armed := g.armed
		g.mu.Unlock()
		if armed {
			select {
			case g.entered <- struct{}{}:
			default:
			}
			<-g.release
		}
		return next == nil || next()
	}
	e.T.Cleanup(g.Release)
	return g
}

// Arm makes the next admissions pause until Release.
func (g *AdmissionGate) Arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

// Entered receives once when an admission reached the gate and is paused.
func (g *AdmissionGate) Entered() <-chan struct{} { return g.entered }

// Release lets the paused admissions continue and disarms the gate. It is
// safe to call more than once.
func (g *AdmissionGate) Release() {
	g.once.Do(func() {
		g.mu.Lock()
		g.armed = false
		g.mu.Unlock()
		close(g.release)
	})
}
