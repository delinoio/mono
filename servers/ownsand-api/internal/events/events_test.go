// SPDX-License-Identifier: FSL-1.1-ALv2

package events_test

import (
	"context"
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
func poll(t *testing.T, d *db.DB, cursor int64) ([]string, int64) {
	t.Helper()
	batch, err := events.After(context.Background(), d.Pool, cursor, 500)
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

func begin(t *testing.T, d *db.DB) pgx.Tx {
	t.Helper()
	tx, err := d.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func write(t *testing.T, tx pgx.Tx, device string) {
	t.Helper()
	if err := events.Write(context.Background(), tx, events.Event{Kind: events.KindDeviceConfigChanged, DeviceID: device}); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if err := tx.Commit(context.Background()); err != nil {
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

	txA, txB := begin(t, d), begin(t, d)
	write(t, txA, devA) // the lower insert-time ID
	write(t, txB, devB)
	commit(t, txB)

	got, cursor := poll(t, d, start)
	if !sameDevices(got, []string{devB}) {
		t.Fatalf("first poll = %v, want [B]", got)
	}
	commit(t, txA)
	got, cursor = poll(t, d, cursor)
	if !sameDevices(got, []string{devA}) {
		t.Fatalf("second poll = %v, want [A]: the event that committed later was skipped", got)
	}
	// Nothing replays.
	got, _ = poll(t, d, cursor)
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

	txA, txB := begin(t, d), begin(t, d)
	write(t, txA, devA)
	write(t, txB, devB)
	if err := txA.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	commit(t, txB)
	got, cursor := poll(t, d, start)
	if !sameDevices(got, []string{devB}) {
		t.Fatalf("poll after rollback = %v, want [B]", got)
	}
	txC := begin(t, d)
	write(t, txC, devC)
	commit(t, txC)
	got, _ = poll(t, d, cursor)
	if !sameDevices(got, []string{devC}) {
		t.Fatalf("poll after a later commit = %v, want [C]", got)
	}
}

// TestSeveralEventsInOneTransaction checks that a transaction with several
// events delivers each of them once, in insert order.
func TestSeveralEventsInOneTransaction(t *testing.T) {
	d := dbtest.New(t)
	start, err := events.LatestID(context.Background(), d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devs := []string{newID(t), newID(t), newID(t)}
	tx := begin(t, d)
	for _, dev := range devs {
		write(t, tx, dev)
	}
	commit(t, tx)
	got, cursor := poll(t, d, start)
	if !sameDevices(got, devs) {
		t.Fatalf("poll = %v, want %v", got, devs)
	}
	if got, _ = poll(t, d, cursor); len(got) != 0 {
		t.Fatalf("replayed %v", got)
	}
}

// waitForAdvisoryLockWait polls pg_stat_activity from a separate connection
// until the backend pid waits for an advisory lock. It fails the test when
// the commit finishes first or the wait is not observed within the deadline.
func waitForAdvisoryLockWait(t *testing.T, d *db.DB, pid int, committed <-chan error) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waitType, waitEvent *string
		describe := func(v *string) string {
			if v == nil {
				return "none"
			}
			return *v
		}
		err := d.Pool.QueryRow(ctx,
			"SELECT wait_event_type, wait_event FROM pg_stat_activity WHERE pid = $1", pid,
		).Scan(&waitType, &waitEvent)
		if err != nil {
			t.Fatalf("read the wait event of backend %d: %v", pid, err)
		}
		if waitType != nil && waitEvent != nil && *waitType == "Lock" && *waitEvent == "advisory" {
			return
		}
		select {
		case err := <-committed:
			t.Fatalf("B committed without waiting for the event lock while A held it: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d did not wait for the advisory event lock within 10s (wait event %s/%s)", pid, describe(waitType), describe(waitEvent))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCommitsOfEventWritersAreSerialized forces the trigger of one
// transaction to run before its commit (SET CONSTRAINTS ALL IMMEDIATE): that
// transaction then holds the advisory lock until it commits, so the commit
// of a second event transaction waits, and a poll cannot see the second
// event before the first one.
func TestCommitsOfEventWritersAreSerialized(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	start, err := events.LatestID(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	devA, devB := newID(t), newID(t)

	txA, txB := begin(t, d), begin(t, d)
	write(t, txA, devA)
	if _, err := txA.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	write(t, txB, devB)
	var pidB int
	if err := txB.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pidB); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() { committed <- txB.Commit(ctx) }()
	// Wait until the backend of B reports that it waits for an advisory
	// lock. A sleep alone would also pass when B committed only after A.
	waitForAdvisoryLockWait(t, d, pidB, committed)
	select {
	case err := <-committed:
		t.Fatalf("B committed while A held the event lock: %v", err)
	default:
	}
	if got, _ := poll(t, d, start); len(got) != 0 {
		t.Fatalf("poll while A is committing = %v, want none", got)
	}
	commit(t, txA)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	got, _ := poll(t, d, start)
	if !sameDevices(got, []string{devA, devB}) {
		t.Fatalf("poll = %v, want [A B]", got)
	}
}
