// SPDX-License-Identifier: Apache-2.0

package execlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestBufferKeepsLastBytes(t *testing.T) {
	b := NewBuffer(4)
	b.Write([]byte("ab"))
	b.Write([]byte("cdef"))
	if b.Start() != 2 || b.End() != 6 {
		t.Fatalf("start %d end %d", b.Start(), b.End())
	}
	if got := b.Read(0, 10); string(got) != "cdef" {
		t.Fatalf("read from 0 = %q", got)
	}
	if got := b.Read(4, 1); string(got) != "e" {
		t.Fatalf("read from 4 = %q", got)
	}
	b.Write([]byte("0123456789"))
	if b.Start() != 12 || string(b.Read(12, 10)) != "6789" {
		t.Fatalf("large write: start %d data %q", b.Start(), b.Read(12, 10))
	}
	if b.Read(16, 1) != nil {
		t.Fatal("read at end returned data")
	}
}

func TestBufferCompactionKeepsData(t *testing.T) {
	b := NewBuffer(8)
	var all []byte
	for i := range 100 {
		chunk := []byte{byte(i), byte(i + 1), byte(i + 2)}
		all = append(all, chunk...)
		b.Write(chunk)
		if cap(b.data) > 64 {
			t.Fatalf("buffer grew to %d", cap(b.data))
		}
	}
	if got, want := b.Read(b.Start(), 100), all[len(all)-8:]; !bytes.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func collect(t *testing.T, c *Cursor) []*ownsandv1.ExecEvent {
	t.Helper()
	var out []*ownsandv1.ExecEvent
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		ev, err := c.Next(ctx, 0)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func exited(code int64) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: code}}}}
}

func TestCursorSequence(t *testing.T) {
	l := New("e", 1<<20)
	now := time.Unix(100, 0)
	l.Start(now)
	l.WriteStdout(bytes.Repeat([]byte("a"), ChunkSize+10))
	l.WriteStderr([]byte("err"))
	l.Finish(exited(3), now)
	evs := collect(t, l.Cursor(0, 0))
	if evs[0].GetStarted().GetExecId() != "e" {
		t.Fatalf("first event %v", evs[0])
	}
	var out, errb []byte
	for _, ev := range evs[1 : len(evs)-1] {
		switch {
		case ev.GetStdout() != nil:
			if int64(len(out)) != ev.GetStdout().GetOffset() {
				t.Fatalf("stdout offset %d after %d bytes", ev.GetStdout().GetOffset(), len(out))
			}
			if len(ev.GetStdout().GetData()) > ChunkSize {
				t.Fatal("chunk too large")
			}
			out = append(out, ev.GetStdout().GetData()...)
		case ev.GetStderr() != nil:
			errb = append(errb, ev.GetStderr().GetData()...)
		default:
			t.Fatalf("unexpected event %v", ev)
		}
	}
	if len(out) != ChunkSize+10 || string(errb) != "err" {
		t.Fatalf("stdout %d bytes stderr %q", len(out), errb)
	}
	if evs[len(evs)-1].GetExited().GetExitCode() != 3 {
		t.Fatalf("last event %v", evs[len(evs)-1])
	}
}

func TestCursorTruncatedAfterStarted(t *testing.T) {
	l := New("e", 4)
	l.Start(time.Now())
	l.WriteStdout([]byte("0123456789"))
	l.Finish(exited(0), time.Now())
	evs := collect(t, l.Cursor(2, 0))
	if evs[1].GetOutputTruncated() == nil || evs[1].GetOutputTruncated().GetStdoutOffset() != 6 {
		t.Fatalf("second event %v", evs[1])
	}
	if string(evs[2].GetStdout().GetData()) != "6789" || evs[2].GetStdout().GetOffset() != 6 {
		t.Fatalf("third event %v", evs[2])
	}
	// From a current offset there is no truncation event.
	evs = collect(t, l.Cursor(8, 0))
	if evs[1].GetStdout().GetOffset() != 8 {
		t.Fatalf("attach from 8: %v", evs[1])
	}
}

func TestCursorWaitsAndKeepalive(t *testing.T) {
	l := New("e", 64)
	c := l.Cursor(0, 0)
	ctx := context.Background()
	go func() {
		time.Sleep(20 * time.Millisecond)
		l.Start(time.Now())
	}()
	ev, err := c.Next(ctx, 0)
	if err != nil || ev.GetStarted() == nil {
		t.Fatalf("started: %v %v", ev, err)
	}
	ev, err = c.Next(ctx, 10*time.Millisecond)
	if err != nil || ev.GetKeepalive() == nil {
		t.Fatalf("keepalive: %v %v", ev, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Next(cctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestApplyDeduplicatesAndFillsGaps(t *testing.T) {
	l := New("e", 64)
	now := time.Now()
	l.Apply(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 0, Data: []byte("abc")}}}, now)
	l.Apply(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 1, Data: []byte("bcd")}}}, now)
	if so, _ := l.Offsets(); so != 4 {
		t.Fatalf("stdout end %d", so)
	}
	l.Apply(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Offset: 10, Data: []byte("x")}}}, now)
	if so, _ := l.Offsets(); so != 11 {
		t.Fatalf("stdout end after gap %d", so)
	}
	l.Apply(exited(1), now)
	evs := collect(t, l.Cursor(0, 0))
	if evs[0].GetStarted() == nil || evs[1].GetOutputTruncated().GetStdoutOffset() != 10 {
		t.Fatalf("events %v", evs)
	}
	if !IsFinal(evs[len(evs)-1]) || l.Final() == nil {
		t.Fatal("missing final")
	}
}
