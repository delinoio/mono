// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtestslot limits how many Postgres-backed tests use one test
// server at the same time.
//
// Tests that create their own databases can run in parallel, and `go test`
// also runs several test binaries at once. Every running test holds pooled
// connections, so on a machine with many CPUs the tests can use more
// connections than the server allows (`max_connections`), and then fail with
// "too many clients". A test takes a slot before it creates a database and
// keeps it until the test ends. A slot is a session-level advisory lock on
// the admin database, so the limit applies to all test processes that use the
// same admin database.
//
// Each test process opens one slot connection to each admin database. That
// connection holds the slot locks of all tests of the process, so tests that
// wait for a slot or hold one use no other connection for it. When the
// process ends, Postgres frees its slot locks.
//
// All databases of one top-level test, including those of its subtests, share
// one slot. A test that holds a slot therefore never waits for a second slot,
// and the tests cannot deadlock on slots.
package dbtestslot

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockNamespace is the first key of the slot advisory locks. It keeps the
// slot locks apart from other advisory locks on the admin database.
const lockNamespace int32 = 0x64627473 // "dbts"

// connectionsPerSlot is the connection budget of one running test: the admin
// connections that create and drop its databases and the pooled connections
// of the test. Most tests use 7 or fewer at a time; a test can open more for
// a short time, and the budget holds for the average of the running tests.
const connectionsPerSlot = 8

// reservedConnections stays free for the slot connections of the test
// processes, the superuser reserve of Postgres and clients that take no slot,
// such as a developer's psql session.
const reservedConnections = 20

// pollInterval is the mean wait before a test tries the slots again.
const pollInterval = 25 * time.Millisecond

// Slots returns the number of slots for a server that accepts maxConnections
// connections. It is always at least 1.
func Slots(maxConnections int) int {
	return max(1, (maxConnections-reservedConnections)/connectionsPerSlot)
}

// Locker takes one slot. It returns a function that frees the slot.
type Locker func(ctx context.Context) (release func(), err error)

// hold is the slot of one top-level test.
type hold struct {
	refs    int
	ready   chan struct{}
	err     error
	release func()
}

// Limiter shares one slot between all callers of one top-level test.
type Limiter struct {
	mu    sync.Mutex
	holds map[string]*hold
}

var (
	std Limiter

	serversMu sync.Mutex
	servers   = map[string]*server{}
)

// Acquire takes a slot for the top-level test of t on the server of the admin
// URL, and frees it in a cleanup of t when no other database of that
// top-level test still uses it. Call it before the test creates a database,
// so that the cleanup frees the slot after the database is dropped. The wait
// for a free slot has no time limit of its own; the `go test` timeout ends
// it.
func Acquire(t testing.TB, admin string) {
	t.Helper()
	std.Acquire(t, func(ctx context.Context) (func(), error) {
		srv, err := serverFor(admin)
		if err != nil {
			return nil, err
		}
		return srv.lock(ctx)
	})
}

// Acquire is the Acquire function of the package with an explicit locker.
func (l *Limiter) Acquire(t testing.TB, lock Locker) {
	t.Helper()
	root, _, _ := strings.Cut(t.Name(), "/")
	l.acquireAs(t, root, lock)
}

// acquireAs takes the slot of the top-level test named root.
func (l *Limiter) acquireAs(t testing.TB, root string, lock Locker) {
	t.Helper()
	l.mu.Lock()
	if l.holds == nil {
		l.holds = map[string]*hold{}
	}
	h := l.holds[root]
	if h == nil {
		h = &hold{ready: make(chan struct{})}
		l.holds[root] = h
		h.refs++
		l.mu.Unlock()
		h.release, h.err = lock(t.Context())
		close(h.ready)
	} else {
		h.refs++
		l.mu.Unlock()
		<-h.ready
	}
	t.Cleanup(func() { l.unref(root, h) })
	if h.err != nil {
		t.Fatalf("dbtestslot: take a test database slot: %v", h.err)
	}
}

func (l *Limiter) unref(root string, h *hold) {
	l.mu.Lock()
	h.refs--
	last := h.refs == 0
	if last {
		delete(l.holds, root)
	}
	l.mu.Unlock()
	if last && h.release != nil {
		h.release()
	}
}

// server is the slot connection of this process to one admin database.
type server struct {
	config *pgx.ConnConfig
	// namespace is the first key of the slot advisory locks.
	namespace int32

	// mu serializes the use of conn, which is not safe for concurrent use.
	mu    sync.Mutex
	conn  *pgx.Conn
	slots int
	// generation counts the connections. A lock taken on an earlier
	// connection ended with that connection's session.
	generation int
	// held has the slots that this process holds on conn. A session can
	// take an advisory lock that it already holds again, so the process
	// must not try those slots for another test.
	held map[int32]bool
}

func serverFor(admin string) (*server, error) {
	serversMu.Lock()
	defer serversMu.Unlock()
	if srv := servers[admin]; srv != nil {
		return srv, nil
	}
	// The pool parser keeps pool options of the URL out of the startup
	// parameters of the slot connection.
	config, err := pgxpool.ParseConfig(admin)
	if err != nil {
		// The parser error can contain credentials from the URL.
		return nil, errors.New("invalid admin database URL")
	}
	srv := &server{config: config.ConnConfig, namespace: lockNamespace, held: map[int32]bool{}}
	servers[admin] = srv
	return srv, nil
}

// lock waits until this process holds a free slot and returns the function
// that frees it.
func (s *server) lock(ctx context.Context) (func(), error) {
	for {
		slot, generation, ok, err := s.tryLock(ctx)
		if err != nil {
			return nil, err
		}
		if ok {
			return func() { s.unlock(slot, generation) }, nil
		}
		wait := pollInterval/2 + rand.N(pollInterval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// tryLock tries every slot that this process does not hold, starting at a
// random slot so that waiting processes do not all compete for the same one.
func (s *server) tryLock(ctx context.Context) (slot int32, generation int, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if err := s.connect(ctx); err != nil {
			return 0, 0, false, err
		}
	}
	first := rand.IntN(s.slots)
	for i := range s.slots {
		slot := int32((first + i) % s.slots)
		if s.held[slot] {
			continue
		}
		var locked bool
		if err := s.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", s.namespace, slot).Scan(&locked); err != nil {
			s.closeLocked()
			if ctx.Err() != nil {
				return 0, 0, false, ctx.Err()
			}
			return 0, 0, false, errors.New("take a slot advisory lock failed")
		}
		if locked {
			s.held[slot] = true
			return slot, s.generation, true, nil
		}
	}
	return 0, 0, false, nil
}

func (s *server) connect(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, s.config)
	if err != nil {
		// A pgx connect error can contain the host, the user or the URL.
		return errors.New("connect to the admin database failed")
	}
	var maxConnections int
	if err := conn.QueryRow(ctx, "SELECT current_setting('max_connections')::int").Scan(&maxConnections); err != nil {
		closeConn(conn)
		return errors.New("read max_connections failed")
	}
	s.conn = conn
	s.slots = Slots(maxConnections)
	s.generation++
	s.held = map[int32]bool{}
	return nil
}

func (s *server) unlock(slot int32, generation int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || generation != s.generation || !s.held[slot] {
		// The connection that held the lock ended, and the lock with it.
		return
	}
	delete(s.held, slot)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.conn.Exec(ctx, "SELECT pg_advisory_unlock($1, $2)", s.namespace, slot); err != nil {
		// Closing the connection frees every lock of its session; the next
		// test opens a new connection.
		s.closeLocked()
	}
}

// closeLocked closes the slot connection. The caller holds s.mu.
func (s *server) closeLocked() {
	closeConn(s.conn)
	s.conn = nil
	s.held = map[int32]bool{}
}

func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
