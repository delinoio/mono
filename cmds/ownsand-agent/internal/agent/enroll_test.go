// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

// stalledEnrollSetup returns a store and a client whose first Enroll call
// the fake accepts and never answers.
func stalledEnrollSetup(t *testing.T, timeout time.Duration) (*testcp.Server, *store.Store, *cpclient.Client) {
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
	client.AuthTimeout = timeout
	cp.StallNextEnroll()
	return cp, st, client
}

// requirePendingKey checks that the identity file holds a key and no device
// ID, and returns the public key.
func requirePendingKey(t *testing.T, st *store.Store) []byte {
	t.Helper()
	pending, err := st.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if pending.DeviceID != "" || pending.PrivateKeySeed == nil {
		t.Fatalf("pending identity: %+v", pending)
	}
	key, err := identity.FromSeed(pending.PrivateKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	return key.PublicKey()
}

// TestEnrollTimesOut checks that a control plane that accepts the Enroll
// call and never answers ends it with the enrollment deadline, keeps the
// pending key on disk, and that a retry sends the same public key.
func TestEnrollTimesOut(t *testing.T) {
	const timeout = 300 * time.Millisecond
	cp, st, client := stalledEnrollSetup(t, timeout)

	began := time.Now()
	_, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if !errors.Is(err, ErrEnrollTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("enroll error = %v, want ErrEnrollTimeout and DeadlineExceeded", err)
	}
	if took := time.Since(began); took < timeout || took > 10*timeout {
		t.Fatalf("enroll took %s, want about %s", took, timeout)
	}
	pubKey := requirePendingKey(t, st)

	id, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if id.DeviceID == "" {
		t.Fatalf("retry identity: %+v", id)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || !bytes.Equal(keys[0], pubKey) || !bytes.Equal(keys[1], pubKey) {
		t.Fatalf("public keys sent: %x, want twice %x", keys, pubKey)
	}
}

// TestEnrollCallerCancellationEndsRequest checks that a cancelled caller
// context ends the call long before the enrollment deadline, with the
// caller's error and not the timeout error, and keeps the pending key.
func TestEnrollCallerCancellationEndsRequest(t *testing.T) {
	cp, st, client := stalledEnrollSetup(t, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel once the fake has the request.
		for len(cp.EnrollPublicKeys()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	began := time.Now()
	_, err := Enroll(ctx, st, client, testcp.Token, "host", quietLogger())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEnrollTimeout) {
		t.Fatalf("enroll error = %v, want a cancellation", err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("enroll took %s after cancellation", took)
	}
	pubKey := requirePendingKey(t, st)

	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || !bytes.Equal(keys[1], pubKey) {
		t.Fatalf("retry public keys: %x, want %x", keys, pubKey)
	}
}

// TestEnrollEarlierCallerDeadlineWins checks that a caller deadline that is
// shorter than the enrollment deadline ends the call as the caller's
// deadline, not as the enrollment timeout.
func TestEnrollEarlierCallerDeadlineWins(t *testing.T) {
	_, st, client := stalledEnrollSetup(t, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := Enroll(ctx, st, client, testcp.Token, "host", quietLogger())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrEnrollTimeout) {
		t.Fatalf("enroll error = %v, want the caller's deadline", err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("enroll took %s", took)
	}
	requirePendingKey(t, st)
}

// TestEnrollDeadlineClassification checks, without timers racing, which
// deadline an error from the Enroll call is attributed to. The server can
// answer deadline_exceeded before the client's own timers fire, so the
// caller's context can still be live when the error arrives.
func TestEnrollDeadlineClassification(t *testing.T) {
	serverDeadline := connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded"))
	other := connect.NewError(connect.CodeUnavailable, errors.New("down"))

	t.Run("earlier caller deadline with a live context and server code", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rctx, rcancel := context.WithTimeout(ctx, time.Hour)
		defer rcancel()
		if enrollDeadlineEnded(ctx, rctx, serverDeadline) {
			t.Fatal("a server deadline_exceeded under an earlier caller deadline counted as the enrollment timeout")
		}
	})
	t.Run("equal deadlines belong to the caller", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
		defer cancel()
		deadline, _ := ctx.Deadline()
		rctx, rcancel := context.WithDeadline(ctx, deadline)
		defer rcancel()
		if enrollDeadlineEnded(ctx, rctx, serverDeadline) {
			t.Fatal("equal deadlines counted as the enrollment timeout")
		}
	})
	t.Run("expired caller context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		rctx, rcancel := context.WithTimeout(ctx, time.Hour)
		defer rcancel()
		cancel()
		if enrollDeadlineEnded(ctx, rctx, context.Canceled) {
			t.Fatal("a cancelled caller context counted as the enrollment timeout")
		}
	})
	t.Run("no caller deadline and server code", func(t *testing.T) {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Hour)
		defer rcancel()
		if !enrollDeadlineEnded(context.Background(), rctx, serverDeadline) {
			t.Fatal("the enrollment deadline answered by the server did not count as the enrollment timeout")
		}
	})
	t.Run("earlier enrollment deadline with a later caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		rctx, rcancel := context.WithTimeout(ctx, time.Minute)
		defer rcancel()
		if !enrollDeadlineEnded(ctx, rctx, serverDeadline) {
			t.Fatal("the earlier enrollment deadline did not count as the enrollment timeout")
		}
	})
	t.Run("other errors", func(t *testing.T) {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Hour)
		defer rcancel()
		if enrollDeadlineEnded(context.Background(), rctx, other) {
			t.Fatal("an unrelated error counted as the enrollment timeout")
		}
	})
}
