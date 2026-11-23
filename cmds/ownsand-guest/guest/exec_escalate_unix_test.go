// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// escalationProbe replaces the grace timer and the process-group operations of
// a pending SIGKILL. The timer is held until release, and the signal calls are
// recorded and never sent, so a test controls the order of the events without
// a wall clock.
type escalationProbe struct {
	timerStarted chan time.Duration
	fire         chan time.Time
	alive        atomic.Bool
	probes       atomic.Int64
	timerStops   atomic.Int64
	mu           sync.Mutex
	kills        []*os.Process
}

func newEscalationProbe(h *harness, alive bool) *escalationProbe {
	p := &escalationProbe{
		timerStarted: make(chan time.Duration, 1),
		fire:         make(chan time.Time, 1),
	}
	p.alive.Store(alive)
	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = time.Hour
	// A short poll lets the test observe that the escalation is running
	// while its timer is held.
	h.srv.execs.groupPoll = time.Millisecond
	h.srv.execs.escalationHooks = escalationHooks{
		graceTimer: func(d time.Duration) (<-chan time.Time, func() bool) {
			p.timerStarted <- d
			return p.fire, func() bool { p.timerStops.Add(1); return true }
		},
		groupAlive: func(*os.Process) bool {
			p.probes.Add(1)
			return p.alive.Load()
		},
		killGroup: func(proc *os.Process) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.kills = append(p.kills, proc)
		},
	}
	h.srv.execs.mu.Unlock()
	// A test that fails while the timer is held must not leave the
	// escalation goroutine behind.
	h.t.Cleanup(func() {
		select {
		case p.fire <- time.Time{}:
		default:
		}
		h.srv.execs.escalations.Wait()
	})
	return p
}

func (p *escalationProbe) killCalls() []*os.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*os.Process(nil), p.kills...)
}

// waitFor polls cond until it holds or a generous failure bound passes. The
// bound only separates a hung test from a slow one; no assertion depends on
// how long cond took.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// runExecWithin runs req like harness.runExec but fails the test, and does
// not block it, when the terminal event does not arrive within bound. The
// events are read in a helper goroutine, because a blocked Receive does not
// return on a canceled context in every transport state. The goroutine ends
// when the guest closes the stream, which the test cleanup guarantees.
func (h *harness) runExecWithin(req *ownsandv1.ExecRequest, bound time.Duration) []*ownsandv1.ExecEvent {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	s := h.open(ctx)
	s.send(1, req)
	type result struct {
		evs []*ownsandv1.ExecEvent
		err error
	}
	done := make(chan result, 1)
	go func() {
		var evs []*ownsandv1.ExecEvent
		for {
			resp, err := s.s.Receive()
			if err != nil {
				done <- result{evs, err}
				return
			}
			ev := resp.GetFrame().GetExecEvent()
			if ev.GetKeepalive() != nil {
				continue
			}
			evs = append(evs, ev)
			switch ev.GetEvent().(type) {
			case *ownsandv1.ExecEvent_Exited, *ownsandv1.ExecEvent_Failed:
				done <- result{evs, nil}
				return
			}
		}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			h.t.Fatalf("receive: %v", r.err)
		}
		_ = s.s.CloseRequest()
		return r.evs
	case <-time.After(bound):
		h.t.Fatalf("no terminal event within %v", bound)
		return nil
	}
}

// timedOutLeaderRequest returns an exec whose leader exits on SIGTERM when its
// timeout fires. The probe, not the leader, decides whether the group lives.
func timedOutLeaderRequest(t *testing.T) *ownsandv1.ExecRequest {
	req := execReq(t, "/bin/sh", "-c", `trap 'exit 0' TERM; echo ready; while :; do sleep 0.05; done`)
	req.Timeout = durationpb.New(300 * time.Millisecond)
	return req
}

// The group SIGKILL must wait for the grace timer, and the terminal event must
// not wait for the escalation.
func TestExecTimeoutEscalationKillsGroupOnlyAfterGraceTimer(t *testing.T) {
	h := newHarness(t, nil)
	p := newEscalationProbe(h, true)
	evs := h.runExecWithin(timedOutLeaderRequest(t), 10*time.Second)

	// The terminal event is in hand and the grace timer is still held, so the
	// event was published before the escalation could finish.
	ex := evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() {
		t.Fatalf("exited %v", ex)
	}
	select {
	case d := <-p.timerStarted:
		if d != time.Hour {
			t.Fatalf("grace timer started with %v", d)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the escalation did not start its grace timer")
	}
	// The escalation polls the group while its timer is held, and it must
	// not signal the group in that time.
	base := p.probes.Load()
	waitFor(t, "group polls while the timer is held", func() bool { return p.probes.Load() >= base+3 })
	if n := len(p.killCalls()); n != 0 {
		t.Fatalf("%d SIGKILL calls while the grace timer was held", n)
	}
	if n := p.timerStops.Load(); n != 0 {
		t.Fatalf("grace timer released %d times before it fired", n)
	}

	p.fire <- time.Now()
	h.srv.execs.escalations.Wait()
	kills := p.killCalls()
	if len(kills) != 1 || kills[0] == nil || kills[0].Pid <= 0 {
		t.Fatalf("kill calls after the grace timer fired: %v", kills)
	}
}

// A group that is gone when the grace timer fires gets no SIGKILL, because its
// id can name an unrelated group by then.
func TestExecTimeoutEscalationSkipsKillWhenGroupGoneAtGraceTimer(t *testing.T) {
	h := newHarness(t, nil)
	p := newEscalationProbe(h, true)
	evs := h.runExecWithin(timedOutLeaderRequest(t), 10*time.Second)
	if ex := evs[len(evs)-1].GetExited(); !ex.GetTimedOut() {
		t.Fatalf("exited %v", ex)
	}
	<-p.timerStarted
	base := p.probes.Load()
	waitFor(t, "group polls while the timer is held", func() bool { return p.probes.Load() >= base+1 })
	// The escalation can end on its own once a poll finds the group empty;
	// both endings must leave no SIGKILL behind.
	p.alive.Store(false)
	p.fire <- time.Now()
	h.srv.execs.escalations.Wait()
	if n := len(p.killCalls()); n != 0 {
		t.Fatalf("%d SIGKILL calls for an empty group", n)
	}
}
