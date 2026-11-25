// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

// openSecond opens another Store on the directory of st, as a second enroll
// command does, and a client for cp.
func openSecond(t *testing.T, st *store.Store, cp *testcp.Server) (*store.Store, *cpclient.Client) {
	t.Helper()
	other, err := store.Open(st.Dir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return other, client
}

type enrollResult struct {
	id  *store.Identity
	err error
}

// enrollAsync runs Enroll in the background and returns its result channel.
func enrollAsync(t *testing.T, ctx context.Context, st *store.Store, client *cpclient.Client) <-chan enrollResult {
	t.Helper()
	out := make(chan enrollResult, 1)
	go func() {
		id, err := Enroll(ctx, st, client, testcp.Token, "host", quietLogger())
		out <- enrollResult{id, err}
	}()
	return out
}

func waitForEnrollArrival(hold *testcp.EnrollHold, result <-chan enrollResult) error {
	select {
	case <-hold.Arrived():
		return nil
	case r := <-result:
		if r.err != nil {
			return fmt.Errorf("enrollment returned before the held RPC was observed: %w", r.err)
		}
		return errors.New("enrollment succeeded before the held RPC was observed")
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for enrollment to reach the control plane")
	}
}

func newEnrollFixture(t *testing.T) (*testcp.Server, *store.Store, *cpclient.Client) {
	t.Helper()
	cp := testcp.New(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cp, st, client
}

func TestEnrollArrivalReportsEarlyError(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	ctx := t.Context()
	if _, err := Enroll(ctx, st, client, testcp.Token, "host", quietLogger()); err != nil {
		t.Fatalf("enroll fixture: %v", err)
	}
	hold := cp.HoldNextEnroll()
	result := enrollAsync(t, ctx, st, client)
	if err := waitForEnrollArrival(hold, result); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("wait for enrollment: got %v, want ErrAlreadyEnrolled", err)
	}
	if calls := len(cp.EnrollPublicKeys()); calls != 1 {
		t.Fatalf("the repeated attempt reached the control plane: got %d calls, want 1", calls)
	}
}

// TestEnrollWaitingAttemptKeepsCompletedIdentity starts a second enrollment
// on one state directory while the first is in its Enroll call. The second
// waits for the lock, then finds the completed identity, ends with
// ErrAlreadyEnrolled and leaves the identity of the first unchanged. The
// control plane sees one Enroll call, with the key of the first.
func TestEnrollWaitingAttemptKeepsCompletedIdentity(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	if err := waitForEnrollArrival(hold, first); err != nil {
		t.Fatal(err)
	}
	pubKey := requirePendingKey(t, st)

	st2, client2 := openSecond(t, st, cp)
	second := enrollAsync(t, context.Background(), st2, client2)
	// The second command cannot have finished while the first holds the lock.
	select {
	case r := <-second:
		t.Fatalf("second enrollment returned while the first runs: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	if got := requirePendingKey(t, st); !bytes.Equal(got, pubKey) {
		t.Fatal("the pending key changed while the first enrollment runs")
	}

	hold.Release()
	r1 := <-first
	if r1.err != nil || r1.id.DeviceID == "" {
		t.Fatalf("first enrollment: %+v", r1)
	}
	r2 := <-second
	if !errors.Is(r2.err, ErrAlreadyEnrolled) {
		t.Fatalf("second enrollment error = %v, want ErrAlreadyEnrolled", r2.err)
	}
	stored, err := st.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeviceID != r1.id.DeviceID || !bytes.Equal(stored.PrivateKeySeed, r1.id.PrivateKeySeed) {
		t.Fatalf("stored identity %+v differs from the completed %+v", stored, r1.id)
	}
	if keys := cp.EnrollPublicKeys(); len(keys) != 1 || !bytes.Equal(keys[0], pubKey) {
		t.Fatalf("public keys sent: %x, want once %x", keys, pubKey)
	}
}

// TestEnrollWaitingAttemptReusesPendingKey checks that a second command
// that waits while the first fails after it wrote the pending key uses that
// same key, so both attempts of one enrollment send one public key.
func TestEnrollWaitingAttemptReusesPendingKey(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	cp.ConsumeEnrollToken()
	cp.DropNextEnrollResponse()
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	if err := waitForEnrollArrival(hold, first); err != nil {
		t.Fatal(err)
	}
	pubKey := requirePendingKey(t, st)

	st2, client2 := openSecond(t, st, cp)
	second := enrollAsync(t, context.Background(), st2, client2)
	hold.Release()

	// The first attempt commits and loses its reply.
	if r := <-first; r.err == nil {
		t.Fatalf("first enrollment succeeded; want the lost reply: %+v", r.id)
	}
	// The waiting attempt reuses the pending key and recovers the device.
	r2 := <-second
	if r2.err != nil || r2.id.DeviceID == "" {
		t.Fatalf("second enrollment: %+v", r2)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || !bytes.Equal(keys[0], pubKey) || !bytes.Equal(keys[1], pubKey) {
		t.Fatalf("public keys sent: %x, want twice %x", keys, pubKey)
	}
}

// TestEnrollRetryAfterLostResponseRecoversDevice checks, against a control
// plane that consumes the token on the first call, that a retry with the
// same token after a committed enrollment with a lost response sends the
// preserved key and records the committed device.
func TestEnrollRetryAfterLostResponseRecoversDevice(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	cp.ConsumeEnrollToken()
	cp.DropNextEnrollResponse()

	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err == nil {
		t.Fatal("enrollment succeeded; want the lost reply")
	}
	pubKey := requirePendingKey(t, st)

	id, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || !bytes.Equal(keys[0], pubKey) || !bytes.Equal(keys[1], pubKey) {
		t.Fatalf("public keys sent: %x, want twice %x", keys, pubKey)
	}
	stored, err := st.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeviceID == "" || stored.DeviceID != id.DeviceID {
		t.Fatalf("stored identity %+v, returned %+v", stored, id)
	}
	// A later attempt cannot replace the identity that has a device ID.
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("later enrollment error = %v, want ErrAlreadyEnrolled", err)
	}
	if len(cp.EnrollPublicKeys()) != 2 {
		t.Fatal("a later enrollment reached the control plane")
	}
}

// TestEnrollGivesUpWhileAnotherEnrollmentRuns checks that an attempt whose
// context ends while it waits for the lock fails with ErrEnrollmentBusy and
// writes nothing, so it cannot touch the pending key of the running one.
func TestEnrollGivesUpWhileAnotherEnrollmentRuns(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	if err := waitForEnrollArrival(hold, first); err != nil {
		t.Fatal(err)
	}
	pubKey := requirePendingKey(t, st)

	st2, client2 := openSecond(t, st, cp)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Enroll(ctx, st2, client2, testcp.Token, "host", quietLogger())
	if !errors.Is(err, store.ErrEnrollmentBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second enrollment error = %v, want ErrEnrollmentBusy", err)
	}
	if got := requirePendingKey(t, st); !bytes.Equal(got, pubKey) {
		t.Fatal("the pending key changed")
	}
	if len(cp.EnrollPublicKeys()) != 1 {
		t.Fatal("the second enrollment reached the control plane")
	}
	hold.Release()
	if r := <-first; r.err != nil {
		t.Fatalf("first enrollment: %v", r.err)
	}
}

// TestEnrollStateDirectoriesAreIndependent checks that an enrollment that
// holds the lock of one state directory does not delay an enrollment in
// another, and that the identity file stays private.
func TestEnrollStateDirectoriesAreIndependent(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	if err := waitForEnrollArrival(hold, first); err != nil {
		t.Fatal(err)
	}

	otherStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := Enroll(ctx, otherStore, client, testcp.Token, "host", quietLogger())
	if err != nil || id.DeviceID == "" {
		t.Fatalf("enrollment in another directory: %v", err)
	}
	info, err := os.Stat(otherStore.Path(store.IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v, want 0600", info.Mode().Perm())
	}
	hold.Release()
	if r := <-first; r.err != nil {
		t.Fatalf("first enrollment: %v", r.err)
	}
}

func TestResetWaitsForEnrollment(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hold := cp.HoldNextEnroll()
	enrollment := enrollAsync(t, ctx, st, client)
	select {
	case <-hold.Arrived():
	case r := <-enrollment:
		t.Fatalf("enrollment ended before reaching the control plane: %v", r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	key := requirePendingKey(t, st)

	reset := make(chan error, 1)
	go func() { reset <- store.ResetDir(st.Dir()) }()
	select {
	case err := <-reset:
		t.Fatalf("reset returned while enrollment held the lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if got := requirePendingKey(t, st); !bytes.Equal(got, key) {
		t.Fatal("reset changed the pending key")
	}
	if err := store.ResetDir(t.TempDir()); err != nil {
		t.Fatalf("reset in a different directory: %v", err)
	}

	hold.Release()
	if r := <-enrollment; r.err != nil || r.id.DeviceID == "" {
		t.Fatalf("enrollment failed: %v", r.err)
	}
	select {
	case err := <-reset:
		if err != nil {
			t.Fatalf("reset after enrollment: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	requireEmptyStateDir(t, st)
}
