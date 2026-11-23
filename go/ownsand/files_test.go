// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadFileBothTransports(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			want := randomBytes(t, 3*datapath.MaxFileChunkSize+123)
			cp.agent.setFile("/tmp/a", want)
			s := cp.handle(t, cp.client(t, tr))
			got, err := s.ReadFileBytes(context.Background(), "/tmp/a")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

func TestReadFileResumesWithSameReadID(t *testing.T) {
	cases := map[string]func(cp *fakeCP){
		"relay disconnect after a chunk": func(cp *fakeCP) {
			cp.relayScript = func(_ string, n int) relayAction {
				if n == 1 {
					return relayAction{abortAfter: 2}
				}
				return relayAction{}
			}
		},
		"relay rotation": func(cp *fakeCP) {
			cp.relayScript = func(string, int) relayAction { return relayAction{endAfter: 1} }
		},
		"accepted continuations without data": func(cp *fakeCP) {
			cp.relayScript = func(_ string, n int) relayAction {
				if n == 1 {
					return relayAction{abortAfter: 1}
				}
				if n <= 3 {
					return relayAction{stallAccepted: true}
				}
				return relayAction{}
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.readChunk = 10
			want := randomBytes(t, 95)
			cp.agent.setFile("/f", want)
			setup(cp)
			s := cp.handle(t, cp.client(t, TransportRelay))
			got, err := s.ReadFileBytes(context.Background(), "/f")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("content differs")
			}
			cp.agent.mu.Lock()
			reads := len(cp.agent.reads)
			cp.agent.mu.Unlock()
			if reads != 1 {
				t.Fatalf("%d read snapshots, want 1 (one read_id)", reads)
			}
		})
	}
}

func TestReadFileStreamClosedWithoutAcceptedResumesAtNextOffset(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	want := randomBytes(t, 95)
	cp.agent.setFile("/f", want)
	cp.relayScript = func(_ string, n int) relayAction {
		switch n {
		case 1:
			return relayAction{abortAfter: 2}
		case 2:
			return relayAction{noAccept: true}
		}
		return relayAction{}
	}
	s := cp.handle(t, cp.client(t, TransportRelay))
	var out bytes.Buffer
	size, err := s.ReadFile(context.Background(), "/f", &out)
	if err != nil || size != int64(len(want)) || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("read %d bytes: %v", size, err)
	}
	if n := cp.relayCount("ReadFile"); n != 3 {
		t.Fatalf("%d ReadFile calls, want 3", n)
	}
	cp.agent.mu.Lock()
	reads := len(cp.agent.reads)
	cp.agent.mu.Unlock()
	if reads != 1 {
		t.Fatalf("%d read snapshots, want 1 (one read_id)", reads)
	}
}

func TestReadFileStreamClosedWithoutAcceptedEndsWithLastErrorAtWindow(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.setFile("/f", randomBytes(t, 20))
	cp.relayScript = func(string, int) relayAction { return relayAction{noAccept: true} }
	c := cp.client(t, TransportRelay)
	s := cp.handle(t, c)
	start := time.Now()
	_, err := s.ReadFileBytes(context.Background(), "/f")
	var dp *DataPathError
	// The window usually ends in a back-off or a stream attempt, and the
	// operation ends with the last stream error. On a slow runner it can end
	// while the renewal before the next attempt is in flight; the SDK then
	// cancels that AuthorizeRelay call and ends with errAuthorizeDeadline
	// (see TestStalledAuthorizeRelayEndsWithTheWindow).
	if !errors.As(err, &dp) || !(errors.Is(err, errNotAccepted) || errors.Is(err, errAuthorizeDeadline)) {
		t.Fatalf("want a DataPathError with errNotAccepted or errAuthorizeDeadline, got %v", err)
	}
	if time.Since(start) > c.timing.recoveryWindow+time.Second {
		t.Fatal("unaccepted streams must end within the recovery window")
	}
	if n := cp.relayCount("ReadFile"); n < 3 {
		t.Fatalf("%d ReadFile calls, want repeated attempts", n)
	}
}

func TestReadFileContinuationNotAcceptedEndsAtRecoveryDeadline(t *testing.T) {
	for name, act := range map[string]relayAction{
		"blocked receive": {holdOpen: true},
		"keepalives only": {keepalivesOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.readChunk = 10
			cp.agent.setFile("/f", randomBytes(t, 95))
			cp.relayScript = func(_ string, n int) relayAction {
				if n == 1 {
					return relayAction{abortAfter: 2}
				}
				return act
			}
			c := recoveryDeadlineClient(t, cp, 300*time.Millisecond)
			s := cp.handle(t, c)
			start := time.Now()
			var out bytes.Buffer
			_, err := s.ReadFile(context.Background(), "/f", &out)
			elapsed := time.Since(start)
			var dp *DataPathError
			if !errors.As(err, &dp) || !errors.Is(err, errRecoveryDeadline) {
				t.Fatalf("want a DataPathError with errRecoveryDeadline, got %v", err)
			}
			if elapsed > c.timing.recoveryWindow+time.Second {
				t.Fatalf("operation took %v, want about the recovery window %v", elapsed, c.timing.recoveryWindow)
			}
			held := cp.heldDurations()
			if len(held) != 1 || held[0] > c.timing.recoveryWindow+500*time.Millisecond {
				t.Fatalf("held streams %v, want one that the SDK cancelled within the recovery window", held)
			}
			if n := cp.relayCount("ReadFile"); n != 2 {
				t.Fatalf("%d ReadFile calls, want 2: no continuation starts after expiry", n)
			}
			if out.Len() == 0 || out.Len() >= 95 {
				t.Fatalf("delivered %d bytes before the interruption", out.Len())
			}
		})
	}
}

func TestReadFileContinuationAcceptedBeforeDeadlineStaysOpenPastIt(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	want := randomBytes(t, 95)
	cp.agent.setFile("/f", want)
	cp.relayScript = func(_ string, n int) relayAction {
		if n == 1 {
			return relayAction{abortAfter: 2}
		}
		return relayAction{acceptedKeepalives: 700 * time.Millisecond}
	}
	c := recoveryDeadlineClient(t, cp, 300*time.Millisecond)
	s := cp.handle(t, c)
	start := time.Now()
	got, err := s.ReadFileBytes(context.Background(), "/f")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read: %v", err)
	}
	if time.Since(start) < 2*c.timing.recoveryWindow {
		t.Fatalf("read took %v; the accepted continuation did not outlive the window", time.Since(start))
	}
}

func TestReadFileInterruptedAfterAcceptanceResumesSavedBudget(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	cp.agent.setFile("/f", randomBytes(t, 95))
	cp.relayScript = func(_ string, n int) relayAction {
		switch n {
		case 1:
			return relayAction{abortAfter: 2}
		case 2:
			return relayAction{delay: 300 * time.Millisecond, acceptedKeepalives: 800 * time.Millisecond, abortAfterKeepalives: true}
		}
		return relayAction{holdOpen: true}
	}
	c := recoveryDeadlineClient(t, cp, 600*time.Millisecond)
	s := cp.handle(t, c)
	_, err := s.ReadFileBytes(context.Background(), "/f")
	if !errors.Is(err, errRecoveryDeadline) {
		t.Fatalf("want errRecoveryDeadline, got %v", err)
	}
	held := cp.heldDurations()
	if len(held) != 1 || held[0] > 480*time.Millisecond {
		t.Fatalf("held streams %v, want one bounded by the saved budget", held)
	}
}

func TestReadFileResumesOnNewSession(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	want := randomBytes(t, 95)
	cp.agent.setFile("/f", want)
	cp.p2pScript = func(n int) p2pAction {
		if n == 1 {
			return p2pAction{endAfter: 3, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED}
		}
		return p2pAction{}
	}
	s := cp.handle(t, cp.client(t, TransportAuto))
	got, err := s.ReadFileBytes(context.Background(), "/f")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read: %v", err)
	}
	if cp.openSessionCount() != 2 {
		t.Fatalf("%d sessions", cp.openSessionCount())
	}
}

// checkReadRequests checks that every read request that reached the agent
// used one read_id and the offsets of want, in order.
func checkReadRequests(t *testing.T, cp *fakeCP, want []int64) {
	t.Helper()
	reqs := cp.agent.readRequests()
	if len(reqs) != len(want) {
		t.Fatalf("%d read requests, want %d", len(reqs), len(want))
	}
	for i, r := range reqs {
		if r.GetReadId() != reqs[0].GetReadId() {
			t.Fatalf("request %d used read_id %s, want %s", i, r.GetReadId(), reqs[0].GetReadId())
		}
		if r.GetOffset() != want[i] {
			t.Fatalf("request %d used offset %d, want %d", i, r.GetOffset(), want[i])
		}
	}
}

// TestReadFileServedSessionRetirementsDoNotHitFailureCap checks that a read
// outlives more session retirements than the failure cap of 5 allows when the
// agent served the read of every retired session (here with a keepalive
// event): a retirement is not a failure. The replacement session stays
// peer-to-peer and continues the same read_id at the same offset.
func TestReadFileServedSessionRetirementsDoNotHitFailureCap(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	want := randomBytes(t, 95)
	cp.agent.setFile("/f", want)
	s := cp.handle(t, cp.client(t, TransportAuto))
	retire := retireInstalledSession(s)
	cp.p2pScript = func(n int) p2pAction {
		if n <= quietRotations {
			return p2pAction{afterReadKeepalive: retire}
		}
		return p2pAction{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := s.ReadFileBytes(ctx, "/f")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read: %v", err)
	}
	if cp.openSessionCount() != quietRotations+1 {
		t.Fatalf("%d sessions, want %d", cp.openSessionCount(), quietRotations+1)
	}
	if s.Transport() != p2pTransport {
		t.Fatalf("transport %v, want peer-to-peer", s.Transport())
	}
	if len(cp.authRequests()) != 0 {
		t.Fatal("a retirement moved the read to the relay")
	}
	offsets := make([]int64, quietRotations+1)
	checkReadRequests(t, cp, offsets)
}

// TestReadFileServedRetirementsContinueAtDeliveredOffset checks that the
// retirements of served sessions after some bytes arrived continue the read at
// the next undelivered offset, so the caller receives each byte once.
func TestReadFileServedRetirementsContinueAtDeliveredOffset(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.readChunk = 10
	want := randomBytes(t, 95)
	cp.agent.setFile("/f", want)
	s := cp.handle(t, cp.client(t, TransportAuto))
	retire := retireInstalledSession(s)
	cp.p2pScript = func(n int) p2pAction {
		switch {
		case n == 1:
			// Deliver the first chunk (10 bytes), then end the session.
			return p2pAction{endAfter: 1, reason: ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED}
		case n <= quietRotations+1:
			return p2pAction{afterReadKeepalive: retire}
		}
		return p2pAction{}
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	size, err := s.ReadFile(ctx, "/f", &buf)
	if err != nil || size != int64(len(want)) || !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("read: size %d, %v", size, err)
	}
	if cp.openSessionCount() != quietRotations+2 {
		t.Fatalf("%d sessions, want %d", cp.openSessionCount(), quietRotations+2)
	}
	if len(cp.authRequests()) != 0 {
		t.Fatal("a retirement moved the read to the relay")
	}
	offsets := make([]int64, quietRotations+2)
	for i := 1; i < len(offsets); i++ {
		offsets[i] = 10
	}
	checkReadRequests(t, cp, offsets)
}

// TestReadFileRepeatedUnservedSessionRetirementsHitFailureCap checks that a
// retirement before the agent served the read still counts as a failure: six
// of them in a row end the read with a data path error.
func TestReadFileRepeatedUnservedSessionRetirementsHitFailureCap(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.setFile("/f", randomBytes(t, 20))
	s := cp.handle(t, cp.client(t, TransportAuto))
	retire := retireInstalledSession(s)
	cp.p2pScript = func(int) p2pAction { return p2pAction{onRequest: retire} }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.ReadFileBytes(ctx, "/f")
	var dp *DataPathError
	if !errors.As(err, &dp) {
		t.Fatalf("want a DataPathError, got %v", err)
	}
	var retired *errSessionRetired
	if !errors.As(err, &retired) {
		t.Fatalf("want the retirement as the cause, got %v", err)
	}
	if got := cp.openSessionCount(); got != 6 {
		t.Fatalf("%d sessions, want 6 (the sixth failure ends the read)", got)
	}
	if got := s.Transport(); got != p2pTransport {
		t.Fatalf("transport %v, want peer-to-peer", got)
	}
}

// prefixWriter accepts at most accept bytes on its failing call, then returns
// err with the accepted count, as an io.Writer may (a short write with an
// error). Earlier calls accept everything.
type prefixWriter struct {
	buf         bytes.Buffer
	failOnCall  int
	accept      int
	err         error
	calls       int
	acceptedSum int
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failOnCall {
		n := min(w.accept, len(p))
		w.buf.Write(p[:n])
		w.acceptedSum += n
		return n, w.err
	}
	w.buf.Write(p)
	w.acceptedSum += len(p)
	return len(p), nil
}

func TestReadFileCallbackErrorCountsAcceptedPrefix(t *testing.T) {
	writeErr := errors.New("writer full")
	for _, tr := range []Transport{TransportRelay, TransportPeerToPeer} {
		for _, accept := range []int{0, 3} {
			t.Run(fmt.Sprintf("%s/accept-%d", tr, accept), func(t *testing.T) {
				cp := newFakeCP(t)
				cp.agent.readChunk = 10
				want := randomBytes(t, 95)
				cp.agent.setFile("/f", want)
				s := cp.handle(t, cp.client(t, tr))
				// The writer takes the first chunk (10 bytes) and a strict
				// prefix of the second.
				w := &prefixWriter{failOnCall: 2, accept: accept, err: writeErr}
				size, err := s.ReadFile(context.Background(), "/f", w)
				if !errors.Is(err, writeErr) {
					t.Fatalf("want the writer's error, got %v", err)
				}
				var dp *DataPathError
				if errors.As(err, &dp) {
					t.Fatalf("callback error must not be wrapped: %v", err)
				}
				if wantSize := int64(10 + accept); size != wantSize || size != int64(w.acceptedSum) {
					t.Fatalf("returned %d bytes, writer accepted %d, want %d", size, w.acceptedSum, wantSize)
				}
				if !bytes.Equal(w.buf.Bytes(), want[:size]) {
					t.Fatal("writer content is not a prefix of the file")
				}
				if tr == TransportRelay {
					if n := cp.relayCount("ReadFile"); n != 1 {
						t.Fatalf("%d ReadFile calls, want 1: a callback error is not retried", n)
					}
				}
			})
		}
	}
}

func TestReadFileCompleteChunksCountOnce(t *testing.T) {
	for _, tr := range []Transport{TransportRelay, TransportPeerToPeer} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.readChunk = 10
			want := randomBytes(t, 95)
			cp.agent.setFile("/f", want)
			s := cp.handle(t, cp.client(t, tr))
			w := &prefixWriter{failOnCall: -1}
			size, err := s.ReadFile(context.Background(), "/f", w)
			if err != nil || size != int64(len(want)) || !bytes.Equal(w.buf.Bytes(), want) {
				t.Fatalf("read %d bytes: %v", size, err)
			}
		})
	}
}

func TestReadFileNotFoundIsVisible(t *testing.T) {
	cp := newFakeCP(t)
	for _, tr := range []Transport{TransportRelay, TransportPeerToPeer} {
		s := cp.handle(t, cp.client(t, tr))
		_, err := s.ReadFileBytes(context.Background(), "/missing")
		// Callers see the code through the public Connect error API on
		// both transports, not only through SDK internals.
		var dp *DataPathError
		if !errors.As(err, &dp) || connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("%v: want not_found, got %v", tr, err)
		}
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
			t.Fatalf("%v: want a *connect.Error, got %v", tr, err)
		}
		if tr == TransportPeerToPeer {
			// The error frame's ErrorDetail is a Connect error detail, as
			// the control plane attaches it on the relay.
			var detail *ownsandv1.ErrorDetail
			for _, d := range ce.Details() {
				if v, derr := d.Value(); derr == nil {
					if ed, ok := v.(*ownsandv1.ErrorDetail); ok {
						detail = ed
					}
				}
			}
			if detail == nil || detail.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				t.Fatalf("%v: want the ErrorDetail as a Connect detail, got %v", tr, ce.Details())
			}
		}
	}
}

func TestWriteFileChunkedBothTransports(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			want := randomBytes(t, 2*datapath.MaxFileChunkSize+77)
			s := cp.handle(t, cp.client(t, tr))
			n, err := s.WriteFile(context.Background(), "/out", bytes.NewReader(want), WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(want)) || !bytes.Equal(must(cp.agent.file("/out")), want) {
				t.Fatalf("wrote %d bytes", n)
			}
			reqs := cp.agent.writeRequests()
			if len(reqs) != 3 {
				t.Fatalf("%d chunks, want 3", len(reqs))
			}
			for i, r := range reqs {
				if len(r.GetData()) > datapath.MaxFileChunkSize {
					t.Fatal("chunk above 1 MiB")
				}
				if (i == 0) != (r.GetPath() != "") {
					t.Fatal("only the first chunk carries the path")
				}
				if (i == 2) != (r.GetFinish() != nil) {
					t.Fatal("only the last chunk finishes")
				}
			}
		})
	}
}

// countingReader counts Read calls and runs onRead, when set, with the call
// number before it serves the bytes.
type countingReader struct {
	r      io.Reader
	reads  int
	onRead func(n int)
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	if c.onRead != nil {
		c.onRead(c.reads)
	}
	return c.r.Read(p)
}

func TestWriteFileObservesCancellationBeforeReadingTheSource(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			chunk := datapath.MaxFileChunkSize
			t.Run("already canceled", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				src := &countingReader{r: bytes.NewReader([]byte("data"))}
				n, err := s.WriteFile(ctx, "/out", src, WriteOptions{})
				if !errors.Is(err, context.Canceled) || n != 0 {
					t.Fatalf("got %d, %v, want 0 and context.Canceled", n, err)
				}
				if src.reads != 0 {
					t.Fatalf("source read %d times, want none", src.reads)
				}
				if got := len(cp.agent.writeRequests()); got != 0 {
					t.Fatalf("%d write requests reached the agent", got)
				}
			})
			t.Run("canceled before the read-ahead", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				src := &countingReader{r: bytes.NewReader(randomBytes(t, 3*chunk)), onRead: func(n int) {
					if n == 1 {
						cancel()
					}
				}}
				n, err := s.WriteFile(ctx, "/out", src, WriteOptions{})
				if !errors.Is(err, context.Canceled) || n != 0 {
					t.Fatalf("got %d, %v, want 0 and context.Canceled", n, err)
				}
				if src.reads != 1 {
					t.Fatalf("source read %d times, want only the first read", src.reads)
				}
			})
			t.Run("returns the acknowledged offset", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				src := &countingReader{r: bytes.NewReader(randomBytes(t, 3*chunk))}
				// Cancel once the first chunk is acknowledged, before the
				// loop reads ahead for the third chunk.
				n, err := s.WriteFile(ctx, "/out", src, WriteOptions{OnProgress: func(int64) { cancel() }})
				if !errors.Is(err, context.Canceled) || n != int64(chunk) {
					t.Fatalf("got %d, %v, want %d and context.Canceled", n, err, chunk)
				}
				if src.reads != 2 {
					t.Fatalf("source read %d times, want 2 (first chunk and one read-ahead)", src.reads)
				}
			})
		})
	}
}

// scriptedReader serves one scripted read per call. A read after the script
// returns the sentinel error errScriptedReaderExhausted so that an unexpected
// extra source call is visible.
type scriptedReader struct {
	steps []func(p []byte) (int, error)
	reads int
}

var errScriptedReaderExhausted = errors.New("scripted reader: unexpected extra read")

func (s *scriptedReader) Read(p []byte) (int, error) {
	i := s.reads
	s.reads++
	if i >= len(s.steps) {
		return 0, errScriptedReaderExhausted
	}
	return s.steps[i](p)
}

// fullChunk fills p with the byte b and reports a read that fills p.
func fullChunk(b byte) func(p []byte) (int, error) {
	return func(p []byte) (int, error) {
		for i := range p {
			p[i] = b
		}
		return len(p), nil
	}
}

func TestWriteFileObservesCancellationInsideAChunkFill(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			t.Run("partial first read", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				src := &scriptedReader{steps: []func([]byte) (int, error){
					func(p []byte) (int, error) { p[0] = 'x'; cancel(); return 1, nil },
				}}
				n, err := s.WriteFile(ctx, "/out", src, WriteOptions{})
				if !errors.Is(err, context.Canceled) || n != 0 {
					t.Fatalf("got %d, %v, want 0 and context.Canceled", n, err)
				}
				if src.reads != 1 {
					t.Fatalf("source read %d times, want 1", src.reads)
				}
				if got := len(cp.agent.writeRequests()); got != 0 {
					t.Fatalf("%d write requests reached the agent", got)
				}
			})
			t.Run("partial read-ahead after an acknowledged chunk", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				src := &scriptedReader{steps: []func([]byte) (int, error){
					fullChunk('a'),
					fullChunk('b'),
					func(p []byte) (int, error) { p[0] = 'c'; cancel(); return 1, nil },
				}}
				n, err := s.WriteFile(ctx, "/out", src, WriteOptions{})
				if !errors.Is(err, context.Canceled) || n != int64(datapath.MaxFileChunkSize) {
					t.Fatalf("got %d, %v, want %d and context.Canceled", n, err, datapath.MaxFileChunkSize)
				}
				if src.reads != 3 {
					t.Fatalf("source read %d times, want 3", src.reads)
				}
				reqs := cp.agent.writeRequests()
				if len(reqs) != 1 || reqs[0].GetFinish() != nil {
					t.Fatalf("got %d write requests, want one nonfinal chunk", len(reqs))
				}
			})
			t.Run("short reads without cancellation upload everything", func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				want := randomBytes(t, 3*datapath.MaxFileChunkSize/2)
				// iotest.OneByteReader-style behavior at a larger step: every
				// Read returns at most 4096 bytes.
				src := &shortReader{r: bytes.NewReader(want), max: 4096}
				n, err := s.WriteFile(context.Background(), "/out", src, WriteOptions{})
				if err != nil || n != int64(len(want)) {
					t.Fatalf("got %d, %v, want %d and no error", n, err, len(want))
				}
			})
		})
	}
}

// shortReader returns at most max bytes per Read.
type shortReader struct {
	r   io.Reader
	max int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if len(p) > s.max {
		p = p[:s.max]
	}
	return s.r.Read(p)
}

func TestWriteFileRejectsOutOfRangeModeBeforeUpload(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, tr))
			for _, call := range []func() error{
				func() error {
					_, err := s.WriteFile(context.Background(), "/out", bytes.NewReader([]byte("x")), WriteOptions{Mode: 0o10000})
					return err
				},
				func() error {
					return s.WriteFileBytes(context.Background(), "/out", nil, WriteOptions{Mode: 0o10000})
				},
			} {
				err := call()
				if !validate.IsValidationError(err) || !strings.Contains(err.Error(), "finish.mode") {
					t.Fatalf("got %v, want a validation error for finish.mode", err)
				}
			}
			if n := len(cp.agent.writeRequests()); n != 0 {
				t.Fatalf("%d write requests reached the agent", n)
			}
			if n := cp.relayCount("WriteFile") + len(cp.authRequests()); n != 0 {
				t.Fatalf("%d relay calls or grants for a rejected write", n)
			}
		})
	}
}

func TestWriteFileModeLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode uint32
		want uint32
	}{{"default", 0, 0o644}, {"maximum", 0o7777, 0o7777}} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportRelay))
			if err := s.WriteFileBytes(context.Background(), "/m", []byte("x"), WriteOptions{Mode: tc.mode}); err != nil {
				t.Fatal(err)
			}
			reqs := cp.agent.writeRequests()
			if got := reqs[len(reqs)-1].GetFinish().GetMode(); got != tc.want {
				t.Fatalf("finish mode %#o, want %#o", got, tc.want)
			}
		})
	}
}

// truncatedReader yields data and then err, in one Read call per piece.
type truncatedReader struct {
	data []byte
	err  error
}

func (r *truncatedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestWriteFileSourceEnd(t *testing.T) {
	chunk := datapath.MaxFileChunkSize
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		for _, size := range []int{10, chunk + 10} {
			t.Run(tr.String(), func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				data := randomBytes(t, size)
				_, err := s.WriteFile(context.Background(), "/trunc", &truncatedReader{data: data, err: io.ErrUnexpectedEOF}, WriteOptions{})
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("size %d: want io.ErrUnexpectedEOF, got %v", size, err)
				}
				for _, r := range cp.agent.writeRequests() {
					if r.GetFinish() != nil {
						t.Fatalf("size %d: finish sent for a truncated source", size)
					}
				}
				if _, ok := cp.agent.file("/trunc"); ok {
					t.Fatalf("size %d: file written", size)
				}

				// A clean end of a short stream finishes with exactly those bytes.
				n, err := s.WriteFile(context.Background(), "/clean", &truncatedReader{data: data, err: io.EOF}, WriteOptions{})
				if err != nil || n != int64(size) || !bytes.Equal(must(cp.agent.file("/clean")), data) {
					t.Fatalf("size %d: clean EOF wrote %d bytes, err %v", size, n, err)
				}
			})
		}
	}
}

func TestWriteEmptyFile(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportRelay))
	if err := s.WriteFileBytes(context.Background(), "/empty", nil, WriteOptions{Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if f, ok := cp.agent.file("/empty"); !ok || len(f) != 0 {
		t.Fatal("empty file not written")
	}
}

func TestWriteFileRetries(t *testing.T) {
	cases := map[string]func(n int) relayAction{
		"deadline exceeded": func(n int) relayAction {
			if n == 2 {
				return relayAction{err: connect.NewError(connect.CodeDeadlineExceeded, errors.New("call limit"))}
			}
			return relayAction{}
		},
		"lost response": func(n int) relayAction {
			if n == 1 {
				return relayAction{abortAfter: 1}
			}
			return relayAction{}
		},
		"no response before the SDK deadline": func(n int) relayAction {
			if n == 1 {
				return relayAction{delay: 800 * time.Millisecond}
			}
			return relayAction{}
		},
		"agent capacity": func(n int) relayAction {
			if n <= 2 {
				return relayAction{err: connect.NewError(connect.CodeResourceExhausted, errors.New("capacity"))}
			}
			return relayAction{}
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			cp := newFakeCP(t)
			cp.relayScript = func(_ string, n int) relayAction { return script(n) }
			want := randomBytes(t, datapath.MaxFileChunkSize+10)
			s := cp.handle(t, cp.client(t, TransportRelay))
			if err := s.WriteFileBytes(context.Background(), "/w", want, WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(must(cp.agent.file("/w")), want) {
				t.Fatal("content differs")
			}
			ids := map[string]bool{}
			for _, r := range cp.agent.writeRequests() {
				ids[r.GetWriteId()] = true
			}
			if len(ids) != 1 {
				t.Fatal("retries must keep the write_id")
			}
		})
	}
}

func TestWriteFileRenewsGrantBeforeChunk(t *testing.T) {
	cp := newFakeCP(t)
	// The grant would expire during the next call (write call limit plus
	// 30 seconds, scaled), so each chunk renews it first.
	cp.expiresIn = 550 * time.Millisecond
	want := randomBytes(t, 2*datapath.MaxFileChunkSize+1)
	s := cp.handle(t, cp.client(t, TransportRelay))
	if err := s.WriteFileBytes(context.Background(), "/w", want, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	auths := cp.authRequests()
	if len(auths) != 3 {
		t.Fatalf("%d authorizations, want one per chunk", len(auths))
	}
	for _, a := range auths[1:] {
		if a.ReplacesGrantId == nil {
			t.Fatal("renewal without replaces_grant_id")
		}
	}
}

func TestWriteFileRenewsAfterIdleGrant(t *testing.T) {
	cp := newFakeCP(t)
	c := cp.client(t, TransportRelay)
	c.timing.grantIdle = 150 * time.Millisecond
	s := cp.handle(t, c)
	// A slow reader: the pause before the second chunk is longer than the
	// agent's idle release time.
	r := &slowReader{data: randomBytes(t, 2*datapath.MaxFileChunkSize+5), pauseAt: 2 * datapath.MaxFileChunkSize, pause: 300 * time.Millisecond}
	if _, err := s.WriteFile(context.Background(), "/w", r, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := len(cp.authRequests()); n < 2 {
		t.Fatalf("%d authorizations; an idle grant must be renewed", n)
	}
}

type slowReader struct {
	data    []byte
	off     int
	pauseAt int
	pause   time.Duration
	paused  bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	if r.off >= r.pauseAt && !r.paused {
		r.paused = true
		time.Sleep(r.pause)
	}
	end := len(r.data)
	if r.off < r.pauseAt {
		end = r.pauseAt
	}
	n := copy(p, r.data[r.off:end])
	r.off += n
	return n, nil
}

func TestWriteFileContinuesOnRelayAfterChannelFailure(t *testing.T) {
	cp := newFakeCP(t)
	cp.p2pScript = func(n int) p2pAction {
		if n == 2 {
			return p2pAction{failAfter: 1}
		}
		return p2pAction{}
	}
	want := randomBytes(t, 2*datapath.MaxFileChunkSize)
	s := cp.handle(t, cp.client(t, TransportAuto))
	if err := s.WriteFileBytes(context.Background(), "/w", want, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(must(cp.agent.file("/w")), want) {
		t.Fatal("content differs")
	}
	if cp.relayCount("WriteFile") != 1 {
		t.Fatalf("%d relay chunks, want the unacknowledged one", cp.relayCount("WriteFile"))
	}
}

func must(b []byte, _ bool) []byte { return b }

// lateEOFReader returns one full chunk at once and then waits before it
// reports the end of input, so the lookahead of WriteFile delays the first
// send by that long.
type lateEOFReader struct {
	data  []byte
	off   int
	delay time.Duration
}

func (r *lateEOFReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		time.Sleep(r.delay)
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// firstByteDelayReader waits before it returns its first byte.
type firstByteDelayReader struct {
	data  []byte
	off   int
	delay time.Duration
	slept bool
}

func (r *firstByteDelayReader) Read(p []byte) (int, error) {
	if !r.slept {
		r.slept = true
		time.Sleep(r.delay)
	}
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// TestWriteFileGeneratesWriteIDWhenFirstRequestIsReady checks that the
// write_id is created after chunk 1 and its lookahead are read, so a slow
// source does not age the UUID v7 timestamp before the first send.
func TestWriteFileGeneratesWriteIDWhenFirstRequestIsReady(t *testing.T) {
	const delay = 1500 * time.Millisecond
	const maxAge = 500 * time.Millisecond
	cases := map[string]func() io.Reader{
		"slow first byte": func() io.Reader {
			return &firstByteDelayReader{data: []byte("hello"), delay: delay}
		},
		"full chunk then late end of input": func() io.Reader {
			return &lateEOFReader{data: make([]byte, datapath.MaxFileChunkSize), delay: delay}
		},
	}
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		for name, newReader := range cases {
			t.Run(tr.String()+"/"+name, func(t *testing.T) {
				cp := newFakeCP(t)
				s := cp.handle(t, cp.client(t, tr))
				if _, err := s.WriteFile(context.Background(), "/slow", newReader(), WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				reqs := cp.agent.writeRequests()
				if len(reqs) == 0 {
					t.Fatal("no write request reached the agent")
				}
				sentAt := time.Now()
				id := reqs[0].GetWriteId()
				created, err := uuidv7.TimeOf(id)
				if err != nil {
					t.Fatal(err)
				}
				// The agent saw the first request after the delay; its write_id
				// must be newer than the delay, not from before the read.
				if age := sentAt.Sub(created); age > maxAge {
					t.Fatalf("write_id %s is %v old after the upload; the delayed read must not age it (limit %v)", id, age, maxAge)
				}
				for _, r := range reqs {
					if r.GetWriteId() != id {
						t.Fatal("later chunks must keep the write_id")
					}
				}
			})
		}
	}
}

// TestWriteFileRetryKeepsOperationIdentity checks that the write_id, the
// binding salt and the grant digest are fixed once the first request is ready
// and that a retry after a failure reuses them, also for a slow source.
func TestWriteFileRetryKeepsOperationIdentity(t *testing.T) {
	cp := newFakeCP(t)
	cp.relayScript = func(_ string, n int) relayAction {
		if n == 1 {
			return relayAction{abortAfter: 1}
		}
		if n == 2 {
			return relayAction{err: connect.NewError(connect.CodeDeadlineExceeded, errors.New("call limit"))}
		}
		return relayAction{}
	}
	want := randomBytes(t, datapath.MaxFileChunkSize+10)
	r := &firstByteDelayReader{data: want, delay: 300 * time.Millisecond}
	s := cp.handle(t, cp.client(t, TransportRelay))
	if _, err := s.WriteFile(context.Background(), "/w", r, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(must(cp.agent.file("/w")), want) {
		t.Fatal("content differs")
	}
	reqs := cp.agent.writeRequests()
	ids := map[string]bool{}
	for _, req := range reqs {
		ids[req.GetWriteId()] = true
	}
	if len(ids) != 1 {
		t.Fatalf("%d write_ids across retries, want 1", len(ids))
	}
	first := reqs[0]
	if first.GetPath() == "" || len(first.GetBindingSalt()) == 0 {
		t.Fatal("first request carries no path and salt")
	}
	wantDigest := relaybinding.WriteFileDigest(first)
	auths := cp.authRequests()
	if len(auths) == 0 {
		t.Fatal("no relay authorization")
	}
	for _, a := range auths {
		if a.GetOperationId() != first.GetWriteId() {
			t.Fatalf("authorization for %s, want %s", a.GetOperationId(), first.GetWriteId())
		}
		if !bytes.Equal(a.GetRequestDigest(), wantDigest) {
			t.Fatal("grant digest differs from the digest of the first request")
		}
	}
	for _, req := range reqs {
		if req.GetPath() != "" && !bytes.Equal(req.GetBindingSalt(), first.GetBindingSalt()) {
			t.Fatal("a resent first request must keep the salt")
		}
	}
}

// TestReadFileFromOffsetAcrossCalls checks a read that a relay serves in
// several calls: each call starts at the offset that the previous one
// reached, with the same read_id and salt, and reads the same snapshot.
func TestReadFileFromOffsetAcrossCalls(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			want := randomBytes(t, 2*datapath.MaxFileChunkSize+500)
			cp.agent.setFile("/f", want)
			s := cp.handle(t, cp.client(t, tr))
			readID := mustID(t)
			salt := randomBytes(t, 32)
			ctx := context.Background()
			// The first call stops after the first chunk.
			var first bytes.Buffer
			stop := errors.New("client stream ended")
			_, err := s.ReadFileFrom(ctx, FileReadParams{ReadID: readID, Path: "/f", BindingSalt: salt}, writerFunc(func(p []byte) (int, error) {
				n, _ := first.Write(p)
				return n, stop
			}))
			if !errors.Is(err, stop) || first.Len() == 0 {
				t.Fatalf("first call: %d bytes, %v", first.Len(), err)
			}
			// The file changes; the second call still reads the snapshot.
			cp.agent.setFile("/f", []byte("changed"))
			var rest bytes.Buffer
			size, err := s.ReadFileFrom(ctx, FileReadParams{ReadID: readID, Path: "/f", BindingSalt: salt, Offset: int64(first.Len())}, &rest)
			if err != nil {
				t.Fatal(err)
			}
			got := append(first.Bytes(), rest.Bytes()...)
			if size != int64(len(want)) || !bytes.Equal(got, want) {
				t.Fatalf("size %d, %d bytes", size, len(got))
			}
			if _, err := s.ReadFileFrom(ctx, FileReadParams{ReadID: readID, Path: "/f", BindingSalt: salt, Offset: -1}, &rest); !validate.IsValidationError(err) {
				t.Fatalf("negative offset: %v", err)
			}
		})
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestFileWriteChunksAcrossCalls checks a write whose chunks arrive in
// separate calls and out of order, and that a second FileWrite with the same
// write_id and salt (another process) continues it.
func TestFileWriteChunksAcrossCalls(t *testing.T) {
	for _, tr := range []Transport{TransportPeerToPeer, TransportRelay} {
		t.Run(tr.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, tr))
			ctx := context.Background()
			writeID := mustID(t)
			salt := randomBytes(t, 32)
			data := randomBytes(t, 3000)
			fw, err := s.OpenFileWrite(FileWriteParams{WriteID: writeID, Path: "/out", BindingSalt: salt})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := fw.WriteChunk(ctx, 1000, data[1000:2000]); err != nil || res.GetOffset() != 1000 || res.GetLength() != 1000 {
				t.Fatalf("middle chunk: %v %v", res, err)
			}
			if _, err := fw.WriteChunk(ctx, 0, data[:1000]); err != nil {
				t.Fatal(err)
			}
			// Another FileWrite of the same write (for example after a
			// process restart) finishes it.
			fw2, err := s.OpenFileWrite(FileWriteParams{WriteID: writeID, Path: "/out", BindingSalt: salt})
			if err != nil {
				t.Fatal(err)
			}
			res, err := fw2.Finish(ctx, 2000, data[2000:], int64(len(data)), 0o755)
			if err != nil || !res.GetFinished() || res.GetSizeBytes() != int64(len(data)) {
				t.Fatalf("finish: %v %v", res, err)
			}
			if !bytes.Equal(must(cp.agent.file("/out")), data) {
				t.Fatal("file content")
			}
			reqs := cp.agent.writeRequests()
			if len(reqs) != 3 || reqs[0].GetPath() != "/out" || reqs[1].GetPath() != "" || reqs[2].GetPath() != "/out" || reqs[2].GetFinish().GetMode() != 0o755 {
				t.Fatalf("requests %v", reqs)
			}
			if _, err := s.OpenFileWrite(FileWriteParams{WriteID: "bad", Path: "/out"}); !validate.IsValidationError(err) {
				t.Fatalf("bad write_id: %v", err)
			}
			big, _ := s.OpenFileWrite(FileWriteParams{WriteID: mustID(t), Path: "/big"})
			if _, err := big.WriteChunk(ctx, 0, make([]byte, datapath.MaxFileChunkSize+1)); !validate.IsValidationError(err) {
				t.Fatalf("chunk above 1 MiB: %v", err)
			}
		})
	}
}
