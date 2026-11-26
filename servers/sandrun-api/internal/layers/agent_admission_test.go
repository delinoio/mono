// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/cursor/cursorfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/devin/devinfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestCursorAdmissionCommitOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault admissionFault
	}{
		{"acknowledged", admissionNoFault},
		{"acknowledgement_lost", admissionDropAck},
		{"initially_in_progress", admissionDelayCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newAdmissionProxy(t, tc.fault)
			var kicks atomic.Int32
			fake := cursorfake.New(t, cursorKey, "sandrun-pool")
			w := startCursorWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, agent.Timing{}, fake, proxy.option(t, &kicks))
			eventually(t, "the Cursor stream", func() bool { return fake.Streams() == 1 })
			kicks.Store(0)
			const key = "cursor-admission"
			fake.AddRequest(key, true)
			if tc.fault == admissionDelayCommit {
				eventually(t, "admission returns with an unresolved commit", func() bool {
					return strings.Contains(w.env.Logs.String(), "agent_admit_failed")
				})
				w.assertNoRequestsOrClaims(t)
				if got := fake.Released(key); got != 0 {
					t.Fatalf("release count while the transaction is open = %d", got)
				}
				if got := kicks.Load(); got != 0 {
					t.Fatalf("dispatch notifications while the transaction is open = %d", got)
				}
				proxy.finish()
			}

			req := w.requestBySource(t, key)
			eventually(t, "a dispatch notification or a vendor release", func() bool {
				return kicks.Load() > 0 || fake.Released(key) > 0
			})
			if proxy.armed.Load() || kicks.Load() != 1 || fake.Released(key) != 0 {
				t.Fatalf("fault armed = %t, dispatch notifications = %d, vendor releases = %d", proxy.armed.Load(), kicks.Load(), fake.Released(key))
			}
			c, err := w.l.Agents.Claim(t.Context(), req.GetRequestId())
			if err != nil || c.State != agent.ClaimHeld {
				t.Fatalf("persisted claim = %+v, %v", c, err)
			}
			if worker, held := fake.Claim(key); !held || worker != c.WorkerID {
				t.Fatalf("Cursor claim = %q, held = %t, want %q", worker, held, c.WorkerID)
			}
			stays(t, "one request and no release after commit", func() bool {
				return len(w.requests(t)) == 1 && fake.Released(key) == 0 && kicks.Load() == 1
			})
		})
	}
}

func TestDevinAdmissionConfirmedRollback(t *testing.T) {
	proxy := newAdmissionProxy(t, admissionDelayRollback)
	var kicks atomic.Int32
	fake := devinfake.New(t, devinAPIToken, "sandrun-outpost")
	w := createDevinWorld(t, sandrunv1.GuestOs_GUEST_OS_LINUX, fake, proxy.option(t, &kicks))
	eventually(t, "the Devin stream", func() bool { return fake.Streams() == 1 })
	kicks.Store(0)
	const key = "devin-admission"
	fake.AddSession(key, true)
	eventually(t, "admission returns with an unresolved commit", func() bool {
		return strings.Contains(w.env.Logs.String(), "agent_admit_failed")
	})
	w.assertNoRequestsOrClaims(t)
	if got := fake.ReleaseAttempts(key); got != 0 {
		t.Fatalf("release count while the transaction is open = %d", got)
	}
	// The release of a terminal session leaves no second pending entry.
	fake.SetSessionStatus(key, "terminated")
	proxy.finish()
	eventually(t, "release after rollback", func() bool { return fake.Releases(key) == 1 })
	stays(t, "one release and no dispatch notification", func() bool {
		return fake.ReleaseAttempts(key) == 1 && kicks.Load() == 0
	})
	w.assertNoRequestsOrClaims(t)
}

type admissionFault uint8

const (
	admissionNoFault admissionFault = iota
	admissionDropAck
	admissionDelayCommit
	admissionDelayRollback
)

// admissionProxy relays the PG wire protocol to the real database. For one
// admission COMMIT it can discard the server's reply or keep the server
// transaction open after the client connection drops.
type admissionProxy struct {
	ctx     context.Context
	fault   admissionFault
	armed   atomic.Bool
	proceed chan struct{}
	finish  func()
}

func newAdmissionProxy(t *testing.T, fault admissionFault) *admissionProxy {
	proceed := make(chan struct{})
	p := &admissionProxy{ctx: t.Context(), fault: fault, proceed: proceed, finish: sync.OnceFunc(func() { close(proceed) })}
	p.armed.Store(fault != admissionNoFault)
	return p
}

func (p *admissionProxy) option(t *testing.T, kicks *atomic.Int32) testenv.Option {
	t.Helper()
	return func(opts *app.Options) {
		if p.fault != admissionNoFault {
			cfg := opts.DB.Pool.Config()
			if cfg.ConnConfig.TLSConfig != nil || len(cfg.ConnConfig.Fallbacks) != 0 {
				t.Skip("the PostgreSQL wire fault fixture requires sslmode=disable")
			}
			cfg.ConnConfig.DialFunc = p.dial
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			opts.DB = &db.DB{Pool: pool}
		}
		opts.Extensions = append(opts.Extensions, func(r *app.Registry) error {
			kick := r.Service.OnWorkChange
			r.Service.OnWorkChange = func(orgID string) {
				kicks.Add(1)
				kick(orgID)
			}
			return nil
		})
	}
}

func (p *admissionProxy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	upstream, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	driver, relay := net.Pipe()
	go p.relay(relay, upstream)
	return driver, nil
}

func (p *admissionProxy) relay(driver, postgres net.Conn) {
	defer driver.Close()
	defer postgres.Close()
	startup, err := readPGFrame(driver, false)
	// Ignore pgx's CancelRequest for this disconnected socket. Only this
	// fixture decides when the original backend may finish.
	if err != nil || binary.BigEndian.Uint32(startup[4:8]) == 80877102 {
		return
	}
	if _, err := postgres.Write(startup); err != nil {
		return
	}

	var dropAck atomic.Bool
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer driver.Close()
		for {
			msg, err := readPGFrame(postgres, true)
			if err != nil {
				return
			}
			if dropAck.Load() {
				if msg[0] == 'Z' { // ReadyForQuery follows the COMMIT result.
					return
				}
			} else if _, err := driver.Write(msg); err != nil {
				return
			}
		}
	}()
	isAdmission := false
	for {
		msg, err := readPGFrame(driver, true)
		if err != nil {
			return
		}
		if msg[0] == 'P' && bytes.Contains(msg, []byte("INSERT INTO agent_claims")) {
			isAdmission = true
		}
		if isAdmission && msg[0] == 'Q' && string(msg[5:]) == "commit\x00" && p.armed.CompareAndSwap(true, false) {
			dropAck.Store(true)
			if p.fault != admissionDropAck {
				_ = driver.Close()
				select {
				case <-p.proceed:
				case <-p.ctx.Done():
					return
				}
				if p.fault == admissionDelayRollback {
					return // Disconnect the backend without forwarding COMMIT.
				}
			}
			if _, err := postgres.Write(msg); err == nil {
				<-serverDone
			}
			return
		}
		if _, err := postgres.Write(msg); err != nil {
			return
		}
	}
}

func readPGFrame(r io.Reader, typed bool) ([]byte, error) {
	lengthOffset := 0
	if typed {
		lengthOffset = 1
	}
	header := make([]byte, lengthOffset+4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[lengthOffset:])
	if length < 4 || length > 1<<24 || (!typed && length < 8) {
		return nil, errors.New("invalid PostgreSQL frame length")
	}
	msg := make([]byte, int(length)+lengthOffset)
	copy(msg, header)
	_, err := io.ReadFull(r, msg[len(header):])
	return msg, err
}
