// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"testing"
	"testing/synctest"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestSendWriteRequestTerminalBeforeCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewConn(t.Context(), "device", 1, testLimits)
		s := openStream(t, c)
		<-c.out
		<-c.slots

		done := make(chan error, 1)
		write := datapath.MustNewFrame(0, "", &ownsandv1.WriteFileRequest{Data: []byte("chunk")})
		go func() { done <- s.SendWriteRequest(t.Context(), write, nil) }()
		synctest.Wait()

		terminal := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.SessionEnd{})
		if err := c.dispatch(terminal); err != nil {
			t.Fatal(err)
		}
		credit := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: 8 << 20})
		if err := c.dispatch(credit); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("send failed before the handler could read the terminal frame: %v", err)
			}
		default:
			t.Fatal("send still pending after the terminal frame")
		}
		select {
		case f := <-c.out:
			t.Fatalf("the relay queued %s after the terminal frame", f.frame.GetKind())
		default:
		}
		f, err := s.Next(t.Context(), nil)
		if err != nil || f != terminal {
			t.Fatalf("Next = %v, %v, want the queued terminal frame", f, err)
		}
	})
}
