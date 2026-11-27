// SPDX-License-Identifier: FSL-1.1-ALv2

package testcp

import (
	"testing"
	"testing/synctest"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestSendRejectsClosedChannel(t *testing.T) {
	env := &ownsandv1.AgentEnvelope{}
	var accepted, sendBuffered, sendOnBuffered int
	for range 100 {
		ch := &Channel{out: make(chan *ownsandv1.AgentEnvelope, 2), done: make(chan struct{})}
		s := &Server{channel: ch}
		ch.Close()

		if s.Send(env) {
			accepted++
		}
		queued := len(ch.out)
		sendBuffered += queued
		s.sendOn(ch, env)
		sendOnBuffered += len(ch.out) - queued
	}
	if accepted != 0 || sendBuffered != 0 {
		t.Errorf("Send accepted %d sends and queued %d envelopes after close", accepted, sendBuffered)
	}
	if sendOnBuffered != 0 {
		t.Errorf("sendOn queued %d envelopes after close", sendOnBuffered)
	}
}

func TestSendWithOpenOrMissingChannel(t *testing.T) {
	s := &Server{}
	env := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{
		HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: 42},
	}}
	if s.Send(env) {
		t.Fatal("Send accepted an envelope without a current channel")
	}

	ch := &Channel{out: make(chan *ownsandv1.AgentEnvelope, 1), done: make(chan struct{})}
	s.channel = ch
	if !s.Send(env) {
		t.Fatal("Send rejected an envelope on an open channel")
	}
	if len(ch.out) != 1 || <-ch.out != env {
		t.Fatal("Send did not queue the supplied envelope")
	}
	s.sendOn(ch, env)
	if len(ch.out) != 1 || <-ch.out != env {
		t.Fatal("sendOn did not queue the supplied envelope")
	}
}

func TestSendWakesOnCloseWithFullQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := &Channel{out: make(chan *ownsandv1.AgentEnvelope, 1), done: make(chan struct{})}
		s := &Server{channel: ch}
		defer ch.Close()
		original := &ownsandv1.AgentEnvelope{}
		ch.out <- original
		result := make(chan bool, 1)
		go func() { result <- s.Send(&ownsandv1.AgentEnvelope{}) }()

		synctest.Wait()
		select {
		case got := <-result:
			t.Fatalf("Send returned %t before the full channel closed", got)
		default:
		}

		ch.Close()
		synctest.Wait()
		select {
		case got := <-result:
			if got {
				t.Fatal("Send accepted an envelope after the full channel closed")
			}
		default:
			t.Fatal("Send remained blocked after the full channel closed")
		}
		if len(ch.out) != 1 || <-ch.out != original {
			t.Fatal("the full queue did not retain its original envelope")
		}
	})
}
