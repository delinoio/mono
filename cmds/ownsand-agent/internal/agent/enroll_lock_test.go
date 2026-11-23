// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"errors"
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

// TestEnrollWaitingAttemptKeepsCompletedIdentity starts a second enrollment
// on one state directory while the first is in its Enroll call. The second
// waits for the lock, then finds the completed identity, ends with
// ErrAlreadyEnrolled and leaves the identity of the first unchanged. The
// control plane sees one Enroll call, with the key of the first.
func TestEnrollWaitingAttemptKeepsCompletedIdentity(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	<-hold.Arrived()
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

// TestEnrollGivesUpWhileAnotherEnrollmentRuns checks that an attempt whose
// context ends while it waits for the lock fails with ErrEnrollmentBusy and
// writes nothing, so it cannot touch the pending key of the running one.
func TestEnrollGivesUpWhileAnotherEnrollmentRuns(t *testing.T) {
	cp, st, client := newEnrollFixture(t)
	hold := cp.HoldNextEnroll()
	first := enrollAsync(t, context.Background(), st, client)
	<-hold.Arrived()
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
	<-hold.Arrived()

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
