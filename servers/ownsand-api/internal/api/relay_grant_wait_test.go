package api_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var (
	errGrantWaitEarlyReturn = errors.New("AuthorizeRelay returned before a relay grant")
	errGrantWaitTimeout     = errors.New("no relay grant arrived before the deadline")
)

// awaitRelayGrant waits for the grant that the fake agent received. A result
// of the AuthorizeRelay call before a grant is a failure, and the RPC result
// (an error or a nil error) is wrapped so that it appears in the diagnostic.
// The deadline channel bounds the wait so that a stalled RPC or a missing
// grant fails the test instead of blocking until the go test timeout.
func awaitRelayGrant(grants <-chan *ownsandv1.RelayGrant, rpcResult <-chan error, deadline <-chan time.Time) (*ownsandv1.RelayGrant, error) {
	select {
	case g := <-grants:
		return g, nil
	case err := <-rpcResult:
		if err == nil {
			return nil, fmt.Errorf("%w: the call succeeded", errGrantWaitEarlyReturn)
		}
		return nil, fmt.Errorf("%w: %w", errGrantWaitEarlyReturn, err)
	case <-deadline:
		return nil, errGrantWaitTimeout
	}
}

func TestAwaitRelayGrant(t *testing.T) {
	closed := make(chan time.Time)
	close(closed)
	never := make(chan time.Time)

	t.Run("grant", func(t *testing.T) {
		grants := make(chan *ownsandv1.RelayGrant, 1)
		want := &ownsandv1.RelayGrant{GrantId: "g"}
		grants <- want
		got, err := awaitRelayGrant(grants, make(chan error), never)
		if err != nil || got != want {
			t.Fatalf("grant not returned: %v %v", got, err != nil)
		}
	})
	t.Run("early error", func(t *testing.T) {
		rpc := make(chan error, 1)
		cause := connect.NewError(connect.CodeInvalidArgument, errors.New("request_digest is required"))
		rpc <- cause
		_, err := awaitRelayGrant(make(chan *ownsandv1.RelayGrant), rpc, never)
		if !errors.Is(err, errGrantWaitEarlyReturn) || !errors.Is(err, cause) {
			t.Fatal("an early RPC error is not reported with its cause")
		}
	})
	t.Run("early success", func(t *testing.T) {
		rpc := make(chan error, 1)
		rpc <- nil
		_, err := awaitRelayGrant(make(chan *ownsandv1.RelayGrant), rpc, never)
		if !errors.Is(err, errGrantWaitEarlyReturn) {
			t.Fatal("an early RPC success is not reported")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		_, err := awaitRelayGrant(make(chan *ownsandv1.RelayGrant), make(chan error), closed)
		if !errors.Is(err, errGrantWaitTimeout) {
			t.Fatal("a missing grant does not fail at the deadline")
		}
	})
}
