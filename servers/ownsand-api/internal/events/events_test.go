// SPDX-License-Identifier: FSL-1.1-ALv2

package events_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// poll reads the events after the cursor the way Control.processEvents does
// and returns the device IDs of the events and the advanced cursor.
func poll(t *testing.T, ctx context.Context, q db.Querier, cursor int64) ([]string, int64) {
	t.Helper()
	batch, err := events.After(ctx, q, cursor, 500)
	if err != nil {
		t.Fatal(err)
	}
	var devices []string
	for _, e := range batch {
		devices = append(devices, e.DeviceID)
		cursor = e.ID
	}
	return devices, cursor
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func begin(t *testing.T, ctx context.Context, d *db.DB) pgx.Tx {
	t.Helper()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rollback(t, tx) })
	return tx
}

func rollback(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("rollback event writer: %v", err)
	}
}

func write(t *testing.T, ctx context.Context, tx pgx.Tx, device string) {
	t.Helper()
	if err := events.Write(ctx, tx, events.Event{Kind: events.KindDeviceConfigChanged, DeviceID: device}); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func sameDevices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestReverseCommitOrderIsDeliveredOnce commits two event transactions in
// the reverse order of their inserts with a poll in between. The event of
// the transaction that commits second has the lower insert-time sequence
// number, and the cursor must not skip it.
func TestReverseCommitOrderIsDeliveredOnce(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	start, err := events.LatestID(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devA, devB := newID(t), newID(t)

	txA, txB := begin(t, ctx, d), begin(t, ctx, d)
	write(t, ctx, txA, devA) // the lower insert-time ID
	write(t, ctx, txB, devB)
	commit(t, ctx, txB)

	got, cursor := poll(t, ctx, d.Pool, start)
	if !sameDevices(got, []string{devB}) {
		t.Fatalf("first poll = %v, want [B]", got)
	}
	commit(t, ctx, txA)
	got, cursor = poll(t, ctx, d.Pool, cursor)
	if !sameDevices(got, []string{devA}) {
		t.Fatalf("second poll = %v, want [A]: the event that committed later was skipped", got)
	}
	// Nothing replays.
	got, _ = poll(t, ctx, d.Pool, cursor)
	if len(got) != 0 {
		t.Fatalf("third poll = %v, want no events", got)
	}
}

// TestRolledBackEventIsNeverDelivered checks that a rollback leaves no
// event and does not stall the events that commit around it.
func TestRolledBackEventIsNeverDelivered(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	start, err := events.LatestID(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devA, devB, devC := newID(t), newID(t), newID(t)

	txA, txB := begin(t, ctx, d), begin(t, ctx, d)
	write(t, ctx, txA, devA)
	write(t, ctx, txB, devB)
	if err := txA.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	commit(t, ctx, txB)
	got, cursor := poll(t, ctx, d.Pool, start)
	if !sameDevices(got, []string{devB}) {
		t.Fatalf("poll after rollback = %v, want [B]", got)
	}
	txC := begin(t, ctx, d)
	write(t, ctx, txC, devC)
	commit(t, ctx, txC)
	got, _ = poll(t, ctx, d.Pool, cursor)
	if !sameDevices(got, []string{devC}) {
		t.Fatalf("poll after a later commit = %v, want [C]", got)
	}
}

// TestSeveralEventsInOneTransaction checks that a transaction with several
// events delivers each of them once, in insert order.
func TestSeveralEventsInOneTransaction(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	start, err := events.LatestID(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devs := []string{newID(t), newID(t), newID(t)}
	tx := begin(t, ctx, d)
	for _, dev := range devs {
		write(t, ctx, tx, dev)
	}
	commit(t, ctx, tx)
	got, cursor := poll(t, ctx, d.Pool, start)
	if !sameDevices(got, devs) {
		t.Fatalf("poll = %v, want %v", got, devs)
	}
	if got, _ = poll(t, ctx, d.Pool, cursor); len(got) != 0 {
		t.Fatalf("replayed %v", got)
	}
}

// The bigint advisory lock key from 0006_control_events_delivery_id.sql.
const eventDeliveryLockID int64 = 0x6f776e5f6374

// PostgreSQL splits a bigint advisory lock key across classid and objid.
// objsubid = 1 identifies the bigint key space.
func waitForAdvisoryLockWait(t *testing.T, ctx context.Context, observer *pgx.Conn, pid int, committed <-chan struct{}, commitErr *error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := observer.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE pid = $1 AND locktype = 'advisory' AND NOT granted AND objsubid = 1
				AND classid = ($2::bigint >> 32)::oid AND objid = ($2::bigint & 4294967295)::oid
		)`, pid, eventDeliveryLockID).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe backend %d waiting for advisory event lock %#x: %v", pid, eventDeliveryLockID, err)
		}
		if waiting {
			return
		}
		select {
		case <-committed:
			t.Fatalf("backend %d finished committing before it waited for advisory event lock %#x: %v", pid, eventDeliveryLockID, *commitErr)
		case <-ctx.Done():
			t.Fatalf("observe backend %d waiting for advisory event lock %#x: %v", pid, eventDeliveryLockID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// TestCommitsOfEventWritersAreSerialized forces the trigger of one
// transaction to run before its commit (SET CONSTRAINTS ALL IMMEDIATE): that
// transaction then holds the advisory lock until it commits, so the commit
// of a second event transaction waits, and a poll cannot see the second
// event before the first one.
func TestCommitsOfEventWritersAreSerialized(t *testing.T) {
	d := dbtest.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start, err := events.LatestID(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devA, devB := newID(t), newID(t)

	txA, txB := begin(t, ctx, d), begin(t, ctx, d)
	write(t, ctx, txA, devA)
	if _, err := txA.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, txB, devB)
	var pidB int
	if err := txB.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pidB); err != nil {
		t.Fatalf("read backend PID for writer B: %v", err)
	}

	// A and B can occupy the entire writer pool. Use a dedicated connection
	// to the same database for the lock observation and the pre-commit read.
	observeCtx, cancelObservation := context.WithTimeout(ctx, 10*time.Second)
	defer cancelObservation()
	observer, err := pgx.ConnectConfig(observeCtx, d.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect observer for backend %d waiting for advisory event lock %#x: %v", pidB, eventDeliveryLockID, err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(closeCtx); err != nil {
			t.Errorf("close event lock observer: %v", err)
		}
	})

	commitCtx, cancelCommit := context.WithTimeout(ctx, 20*time.Second)
	committed := make(chan struct{})
	var commitErr error
	t.Cleanup(func() {
		cancelCommit()
		rollback(t, txA)
		// The commit has a deadline and cancellation interrupts its I/O.
		// Join it before the cleanup registered by begin can touch B.
		<-committed
	})
	go func() {
		defer close(committed)
		commitErr = txB.Commit(commitCtx)
	}()

	waitForAdvisoryLockWait(t, observeCtx, observer, pidB, committed, &commitErr)
	if got, _ := poll(t, observeCtx, observer, start); len(got) != 0 {
		t.Fatalf("poll while A holds the event lock = %v, want none", got)
	}
	select {
	case <-committed:
		t.Fatalf("B finished committing while A held the event lock: %v", commitErr)
	default:
	}
	cancelObservation()
	commit(t, commitCtx, txA)
	select {
	case <-committed:
		if commitErr != nil {
			t.Fatalf("commit B (backend %d): %v", pidB, commitErr)
		}
	case <-commitCtx.Done():
		t.Fatalf("wait for B (backend %d) to finish committing: %v", pidB, commitCtx.Err())
	}
	got, _ := poll(t, ctx, d.Pool, start)
	if !sameDevices(got, []string{devA, devB}) {
		t.Fatalf("poll = %v, want [A B]", got)
	}
}
