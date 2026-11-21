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
