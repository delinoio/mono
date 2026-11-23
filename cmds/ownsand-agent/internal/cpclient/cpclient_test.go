// SPDX-License-Identifier: FSL-1.1-ALv2

package cpclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// authTestTimeout is the shortened attempt deadline of these tests. The
// production default is DefaultAuthTimeout.
const authTestTimeout = 300 * time.Millisecond

// authTestSlack is how far below authTestTimeout an attempt may end. The
// deadline timer and the test's own clock readings are separate clocks: on
// Windows the coarse timer granularity let an attempt end about 0.3 ms before
// the measured 300 ms. The slack keeps the lower bound a check that the
// attempt waited for its deadline instead of failing fast, without asserting
// timer precision. Keep it well under authTestTimeout.
const authTestSlack = 50 * time.Millisecond

// authTestMaxElapsed is the generous upper bound of a stalled attempt. It only
// proves that the attempt ends instead of hanging.
const authTestMaxElapsed = 10 * time.Second

// enrolled returns a client with a short attempt deadline and a device that
// the fake control plane knows.
func enrolled(t *testing.T) (*testcp.Server, *Client, string, *identity.Key) {
	t.Helper()
	cp := testcp.New(t)
	c, err := New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.AuthTimeout = authTestTimeout
	key, _, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Agent.Enroll(context.Background(), connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: testcp.Token,
		PublicKey:       key.PublicKey(),
		Name:            "host",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return cp, c, resp.Msg.GetDeviceId(), key
}

func TestDefaultAuthTimeout(t *testing.T) {
	if got := (&Client{}).authTimeout(); got != DefaultAuthTimeout {
		t.Fatalf("zero AuthTimeout gives %s, want %s", got, DefaultAuthTimeout)
	}
	if got := (&Client{AuthTimeout: time.Second}).authTimeout(); got != time.Second {
		t.Fatalf("AuthTimeout 1s gives %s", got)
	}
}

func TestAuthenticateEndsWhenTheControlPlaneStalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall func(*testcp.Server)
	}{
		{"create challenge", (*testcp.Server).StallNextCreateChallenge},
		{"authenticate", (*testcp.Server).StallNextAuthenticate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp, c, id, key := enrolled(t)
			tc.stall(cp)

			start := time.Now()
			res, err := c.Authenticate(context.Background(), id, key, "")
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("Authenticate of a stalled attempt returned %+v", res)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error %v does not match context.DeadlineExceeded", err)
			}
			if errors.Is(err, ErrUnknownDevice) {
				t.Fatalf("a stalled attempt was reported as an unknown device: %v", err)
			}
			if elapsed < authTestTimeout-authTestSlack || elapsed > authTestMaxElapsed {
				t.Fatalf("Authenticate returned after %s, want about %s", elapsed, authTestTimeout)
			}

			// The next attempt is normal and is not bound to the expired deadline.
			res, err = c.Authenticate(context.Background(), id, key, "")
			if err != nil {
				t.Fatalf("Authenticate after a stalled attempt: %v", err)
			}
			if res.Session == nil || res.Session.Credential == "" {
				t.Fatalf("Authenticate returned no session: %+v", res)
			}
		})
	}
}

func TestAuthenticateKeepsTheCallerContext(t *testing.T) {
	cp, c, id, key := enrolled(t)
	c.AuthTimeout = time.Hour
	cp.StallNextAuthenticate()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := c.Authenticate(ctx, id, key, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not match context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a canceled caller was reported as a deadline: %v", err)
	}
}

func TestAuthenticateUnknownDeviceIsNotATimeout(t *testing.T) {
	cp, c, id, key := enrolled(t)
	cp.Forget(id)
	_, err := c.Authenticate(context.Background(), id, key, "")
	if !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("error %v does not match ErrUnknownDevice", err)
	}
}

func TestConfirmRemovalEndsWhenTheControlPlaneStalls(t *testing.T) {
	cp, c, id, key := enrolled(t)
	cp.StallNextCreateChallenge()
	start := time.Now()
	err := c.ConfirmRemoval(context.Background(), id, key)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v does not match context.DeadlineExceeded", err)
	}
	if elapsed < authTestTimeout-authTestSlack || elapsed > authTestMaxElapsed {
		t.Fatalf("ConfirmRemoval returned after %s, want about %s", elapsed, authTestTimeout)
	}
}

func TestEnrollContextDeadline(t *testing.T) {
	c := &Client{}
	if got := c.EnrollTimeout(); got != DefaultAuthTimeout {
		t.Fatalf("zero AuthTimeout gives EnrollTimeout %s, want %s", got, DefaultAuthTimeout)
	}
	c.AuthTimeout = time.Minute
	ctx, cancel := c.EnrollContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
		t.Fatalf("deadline = %v, %v; want about one minute", deadline, ok)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	ctx2, cancel2 := c.EnrollContext(parent)
	defer cancel2()
	if d, _ := ctx2.Deadline(); time.Until(d) > time.Second {
		t.Fatalf("an earlier caller deadline must win, got %v", d)
	}
}
