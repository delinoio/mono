// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
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
