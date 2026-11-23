// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

// commitProxy is a Postgres wire proxy that loses the response of a commit:
// for each armed commit, it forwards the COMMIT query to Postgres, waits for
// Postgres to answer (so the commit has run), drops that answer and closes
// the client connection. The client then sees a broken connection after it
// sent the commit, which is the outcome that Metering.Flush cannot classify.
type commitProxy struct {
	addr     string
	upstream string
	armed    atomic.Int32
	lost     atomic.Int32
	wg       sync.WaitGroup
	ln       net.Listener
}

func newCommitProxy(t *testing.T, upstream string) *commitProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &commitProxy{addr: ln.Addr().String(), upstream: upstream, ln: ln}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go func() { defer p.wg.Done(); p.serve(c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.wg.Wait() })
	return p
}

// loseNext arms the proxy to lose the response of the next n commits.
func (p *commitProxy) loseNext(n int32) { p.armed.Store(n) }

// claim reports whether this commit is one of the armed ones.
func (p *commitProxy) claim() bool {
	for {
		n := p.armed.Load()
		if n <= 0 {
			return false
		}
		if p.armed.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

func (p *commitProxy) serve(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	var dropping atomic.Bool
	done := make(chan struct{})
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = client.Close(); _ = server.Close(); close(done) }) }
	defer closeBoth()

	// Server to client: framed messages. While a commit response is being
	// dropped, discard messages up to ReadyForQuery, then cut the connection.
	go func() {
		defer closeBoth()
		for {
			typ, body, err := readTyped(server)
			if err != nil {
				return
			}
			if dropping.Load() {
				if typ == 'Z' {
					return
				}
				continue
			}
			if err := writeTyped(client, typ, body); err != nil {
				return
			}
		}
	}()

	// Client to server. The proxy refuses SSL and GSS encryption itself, so
	// every message of the server side is framed from the first byte.
	for {
		code, body, err := readStartup(client)
		if err != nil {
			return
		}
		switch code {
		case 80877103, 80877104:
			if _, err := client.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(body)+8))
		var cd [4]byte
		binary.BigEndian.PutUint32(cd[:], code)
		if _, err := server.Write(append(append(hdr[:], cd[:]...), body...)); err != nil {
			return
		}
		break
	}
	for {
		typ, body, err := readTyped(client)
		if err != nil {
			return
		}
		if typ == 'Q' && strings.EqualFold(string(bytes.TrimRight(body, "\x00")), "commit") && p.claim() {
			dropping.Store(true)
			p.lost.Add(1)
			if err := writeTyped(server, typ, body); err != nil {
				return
			}
			<-done
			return
		}
		if err := writeTyped(server, typ, body); err != nil {
			return
		}
	}
}

func readStartup(r io.Reader) (uint32, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	body := make([]byte, int(n)-8)
	_, err := io.ReadFull(r, body)
	return binary.BigEndian.Uint32(hdr[4:]), body, err
}

func readTyped(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	body := make([]byte, int(binary.BigEndian.Uint32(hdr[1:]))-4)
	_, err := io.ReadFull(r, body)
	return hdr[0], body, err
}

func writeTyped(w io.Writer, typ byte, body []byte) error {
	buf := make([]byte, 5, 5+len(body))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:], uint32(len(body)+4))
	_, err := w.Write(append(buf, body...))
	return err
}

// proxiedFixture is a fixture whose Metering writes through a commitProxy,
// while the test reads the same database directly.
type proxiedFixture struct {
	*fixture
	proxy *commitProxy
	// writer is the pool of the proxied Metering instances.
	writer *db.DB
}

func newProxiedFixture(t *testing.T, start time.Time) *proxiedFixture {
	t.Helper()
	direct := dbtest.NewURL(t)
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newCommitProxy(t, u.Host)
	u.Host = proxy.addr
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	ctx := context.Background()
	writer, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	reader, err := db.Open(ctx, direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	f := newFixture(t, start)
	f.db = reader
	return &proxiedFixture{fixture: f, proxy: proxy, writer: writer}
}

func (f *proxiedFixture) meteringViaProxy() *billing.Metering {
	return f.metering(nil, func(o *billing.Options) { o.DB = f.writer })
}

func (f *proxiedFixture) receipts(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM usage_flush_receipts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestFlushLostCommitResponseCountsOnce: Postgres commits the batch, the
// caller sees a connection error, and the retry adds nothing more. Counts
// that arrive while the batch is pending reach the counter exactly once.
func TestFlushLostCommitResponseCountsOnce(t *testing.T) {
	f := newProxiedFixture(t, t0.Add(time.Minute))
	m := f.meteringViaProxy()
	ctx := context.Background()

	m.Add(org1, classA, t0.Add(time.Minute), 3)
	m.Add(org2, relay, t0.Add(time.Minute), gib)
	f.proxy.loseNext(1)
	if err := m.Flush(ctx); err == nil {
		t.Fatal("flush succeeded although its commit response was lost")
	}
	if f.proxy.lost.Load() != 1 {
		t.Fatalf("lost commits = %d, want 1", f.proxy.lost.Load())
	}
	// The commit ran: the counters hold the batch although the caller saw an error.
	if a, b := f.counter(t, org1, classA, t0), f.counter(t, org2, relay, t0); a != 3 || b != gib {
		t.Fatalf("counters after the lost response = %d %d, want 3 %d", a, b, gib)
	}
	if m.Pending() != 2 {
		t.Fatalf("pending = %d, want the 2 counters of the unacknowledged batch", m.Pending())
	}
	// Usage recorded while the batch is pending.
	m.Add(org1, classA, t0.Add(2*time.Minute), 4)
	m.Add(org1, classB, t0.Add(2*time.Minute), 1)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Pending() != 0 {
		t.Fatalf("pending after the retry = %d", m.Pending())
	}
	if a, b, c := f.counter(t, org1, classA, t0), f.counter(t, org2, relay, t0), f.counter(t, org1, classB, t0); a != 7 || b != gib || c != 1 {
		t.Fatalf("counters after the retry = %d %d %d, want 7 %d 1", a, b, c, gib)
	}
	// The retried batch and the new batch each left one receipt.
	if n := f.receipts(t); n != 2 {
		t.Fatalf("receipts = %d, want 2", n)
	}
	if !strings.Contains(f.logText(), "billing_usage_flush_batch_already_applied") {
		t.Fatal("the retry of the committed batch was not logged")
	}
	// A further flush has nothing to write.
	if err := m.Flush(ctx); err != nil || f.counter(t, org1, classA, t0) != 7 {
		t.Fatalf("idle flush: %v", err)
	}
}

// TestFlushRolledBackBatchAppliesOnce: the first attempt writes the counters
// and fails before the commit, so nothing and no receipt stays; the retry
// applies the whole batch once, together with the counts of the meantime.
func TestFlushRolledBackBatchAppliesOnce(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	m := f.metering(nil)
	ctx := context.Background()
	// A trigger fails the counter write after its rows were written, so the
	// transaction rolls back, receipt included.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_usage_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`,
		`CREATE TRIGGER fail_usage_write BEFORE INSERT OR UPDATE ON usage_counters FOR EACH ROW EXECUTE FUNCTION fail_usage_write()`,
	} {
		if _, err := f.db.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	m.Add(org1, classA, t0.Add(time.Minute), 3)
	m.Add(org1, classB, t0.Add(time.Minute), 5)
	if err := m.Flush(ctx); err == nil {
		t.Fatal("flush with a failing write succeeded")
	}
	if got := f.counter(t, org1, classA, t0); got != 0 {
		t.Fatalf("counter after the rollback = %d", got)
	}
	var receipts int
	if err := f.db.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_flush_receipts").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("receipts after the rollback = %d, %v", receipts, err)
	}
	m.Add(org1, classA, t0.Add(2*time.Minute), 4) // recorded while the batch is pending
	if _, err := f.db.Pool.Exec(ctx, "DROP TRIGGER fail_usage_write ON usage_counters"); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if a, b := f.counter(t, org1, classA, t0), f.counter(t, org1, classB, t0); a != 7 || b != 5 {
		t.Fatalf("counters after the retry = %d %d, want 7 5", a, b)
	}
	if m.Pending() != 0 {
		t.Fatalf("pending = %d", m.Pending())
	}
}

// TestFlushLostCommitThenSealAddsNothingToTheOpenWindow: the batch committed,
// its window is sealed before the retry, and the retry must not add the
// quantity to the oldest open window.
func TestFlushLostCommitThenSealAddsNothingToTheOpenWindow(t *testing.T) {
	f := newProxiedFixture(t, t0.Add(time.Minute))
	m := f.meteringViaProxy()
	ctx := context.Background()
	m.Add(org1, classA, t0.Add(time.Minute), 3)
	f.proxy.loseNext(1)
	if err := m.Flush(ctx); err == nil {
		t.Fatal("flush succeeded although its commit response was lost")
	}
	f.clock.Set(t0.Add(6 * time.Minute))
	if n, err := f.metering(nil).Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	s, ok := f.outbox(t, org1, classA, t0)
	if !ok || s.quantity != 3 {
		t.Fatalf("sealed row = %+v, %v; want quantity 3", s, ok)
	}
	for _, w := range []time.Time{t0, t0.Add(5 * time.Minute), t0.Add(10 * time.Minute)} {
		if got := f.counter(t, org1, classA, w); got != 0 {
			t.Fatalf("counter of window %s = %d, want 0 (the batch was counted already)", w, got)
		}
	}
	if strings.Contains(f.logText(), "billing_usage_late_count_moved") {
		t.Fatal("a committed batch was moved as a late count")
	}
}

// TestFlushRolledBackBatchAfterSealMovesLateCounts: the first attempt rolled
// back and the window is sealed before the retry, so the whole batch goes to
// the oldest open window and the sealed quantity stays what it was.
func TestFlushRolledBackBatchAfterSealMovesLateCounts(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	m := f.metering(nil)
	ctx := context.Background()
	m.Add(org1, classA, t0.Add(time.Minute), 2)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	m.Add(org1, classA, t0.Add(2*time.Minute), 3)
	if err := m.Flush(canceled); err == nil {
		t.Fatal("flush with a canceled context succeeded")
	}
	f.clock.Set(t0.Add(6 * time.Minute))
	if n, err := m.Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if s, ok := f.outbox(t, org1, classA, t0); !ok || s.quantity != 2 {
		t.Fatalf("sealed row = %+v, %v; want the 2 that were flushed before the seal", s, ok)
	}
	if got := f.counter(t, org1, classA, t0.Add(5*time.Minute)); got != 3 {
		t.Fatalf("late count in the oldest open window = %d, want 3", got)
	}
	if !strings.Contains(f.logText(), "billing_usage_late_count_moved") {
		t.Fatal("the late count was not logged")
	}
}

// TestFlushFinalRetriesTheLostBatchOnce: a lost commit response of the final
// flush leaves the batch pending, and a repeated final flush applies nothing
// twice. A count after the close is dropped.
func TestFlushFinalRetriesTheLostBatchOnce(t *testing.T) {
	f := newProxiedFixture(t, t0.Add(time.Minute))
	m := f.meteringViaProxy()
	ctx := context.Background()
	m.Add(org1, classA, t0.Add(time.Minute), 3)
	f.proxy.loseNext(1)
	if err := m.FlushFinal(ctx); err == nil {
		t.Fatal("final flush succeeded although its commit response was lost")
	}
	m.Add(org1, classA, t0.Add(time.Minute), 9) // the recorder is closed
	if err := m.FlushFinal(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.counter(t, org1, classA, t0); got != 3 {
		t.Fatalf("counter = %d, want 3", got)
	}
	if m.Pending() != 0 {
		t.Fatalf("pending = %d", m.Pending())
	}
}

// TestFlushLostCommitsWithConcurrentAdds: usage recorded from many goroutines
// while commit responses are lost reaches the counter exactly once.
func TestFlushLostCommitsWithConcurrentAdds(t *testing.T) {
	f := newProxiedFixture(t, t0.Add(time.Minute))
	m := f.meteringViaProxy()
	ctx := context.Background()
	const writers, adds = 8, 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Every other flush loses its commit response.
			f.proxy.loseNext(1)
			_ = m.Flush(ctx)
			time.Sleep(time.Millisecond)
		}
	}()
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range adds {
				m.Add(org1, classA, t0.Add(time.Minute), 1)
				time.Sleep(50 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-flushed
	f.proxy.loseNext(0)
	// Drain: a flush after the last loss applies or acknowledges what is left.
	for range 3 {
		if err := m.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.counter(t, org1, classA, t0); got != writers*adds {
		t.Fatalf("counter = %d, want %d (lost commit responses: %d)", got, writers*adds, f.proxy.lost.Load())
	}
	if f.proxy.lost.Load() == 0 {
		t.Fatal("no commit response was lost; the test did not exercise the retry")
	}
}

// TestFlushLogsAStaleBatchRetry: a retry of a batch that is older than the
// receipt retention runs without the protection of a receipt that the janitor
// may have deleted, and is logged.
func TestFlushLogsAStaleBatchRetry(t *testing.T) {
	f := newProxiedFixture(t, t0.Add(time.Minute))
	m := f.meteringViaProxy()
	ctx := context.Background()
	m.Add(org1, classA, t0.Add(time.Minute), 3)
	f.proxy.loseNext(1)
	if err := m.Flush(ctx); err == nil {
		t.Fatal("flush succeeded although its commit response was lost")
	}
	f.clock.Advance(billing.FlushReceiptRetention + time.Hour)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.logText(), "billing_usage_flush_batch_stale") {
		t.Fatal("the stale batch retry was not logged")
	}
}
