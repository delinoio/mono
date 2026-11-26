// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type revokedTokenError struct{}

func (*revokedTokenError) Error() string { return "revoked refresh token" }

func TestTerminalTokenSourceFailure(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	session := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	wait := WaitDevSessionParams{OrganizationID: org, DevSessionID: session.GetDevSessionId()}
	revoked := &revokedTokenError{}

	operations := []struct {
		name      string
		procedure string
		call      func(context.Context, func(QueueEvent) error) error
	}{
		{"unary", sandrunv1connect.OrganizationServiceListOrganizationsProcedure, func(ctx context.Context, _ func(QueueEvent) error) error {
			_, err := c.Organizations().ListOrganizations(ctx, connect.NewRequest(&sandrunv1.ListOrganizationsRequest{}))
			return err
		}},
		{"wait", sandrunv1connect.DevSessionServiceGetDevSessionProcedure, func(ctx context.Context, _ func(QueueEvent) error) error {
			_, err := c.WaitDevSession(ctx, wait)
			return err
		}},
		{"watch", sandrunv1connect.WorkServiceWatchQueueProcedure, func(ctx context.Context, handle func(QueueEvent) error) error {
			return c.WatchQueue(ctx, org, handle)
		}},
		{"exec", sandrunv1connect.DevSessionServiceExecDevSessionProcedure, func(ctx context.Context, _ func(QueueEvent) error) error {
			_, err := c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: wait.DevSessionID, Command: []string{"unused"}})
			return err
		}},
		{"read", sandrunv1connect.DevSessionServiceReadDevSessionFileProcedure, func(ctx context.Context, _ func(QueueEvent) error) error {
			_, err := c.readGuestFile(ctx, session, "unused", 100, io.Discard, nil)
			return err
		}},
		{"write", sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure, func(ctx context.Context, _ func(QueueEvent) error) error {
			_, err := c.writeGuestFile(ctx, session, "unused", 0o644, strings.NewReader("test"), nil)
			return err
		}},
	}
	for _, source := range []struct {
		name  string
		err   error
		cause error
	}{
		{"revoked", fmt.Errorf("refresh: %w", revoked), revoked},
		{"empty", nil, nil},
		{"provider EOF", fmt.Errorf("read credential: %w", io.EOF), io.EOF},
	} {
		for _, op := range operations {
			t.Run(source.name+"/"+op.name, func(t *testing.T) {
				calls := 0
				c.credential = tokenCredential(func(context.Context) (string, error) {
					calls++
					return "", source.err
				})
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				err := op.call(ctx, func(ev QueueEvent) error {
					t.Errorf("unexpected queue event %v", ev.Kind)
					return nil
				})
				var credentialErr *CredentialError
				if connect.CodeOf(err) != connect.CodeUnauthenticated || !errors.As(err, &credentialErr) || connect.IsWireError(err) {
					t.Fatalf("error = %v, want local unauthenticated CredentialError", err)
				}
				if credentialErr.Unwrap() != source.err || (source.cause != nil && !errors.Is(err, source.cause)) {
					t.Fatalf("error = %v, want cause %v", err, source.err)
				}
				if source.cause == revoked {
					var cause *revokedTokenError
					if !errors.As(err, &cause) || cause != revoked {
						t.Fatalf("error = %v, want original revokedTokenError", err)
					}
				}
				var streamErr *ExecStreamError
				if errors.As(err, &streamErr) {
					t.Errorf("unexpected ExecStreamError: %v", err)
				}
				if calls != 1 {
					t.Errorf("token source called %d times, want 1", calls)
				}
				if n := srv.Calls(op.procedure); n != 0 {
					t.Errorf("%s received %d requests, want 0", op.name, n)
				}
				if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); n != 0 {
					t.Errorf("attach received %d requests, want 0", n)
				}
			})
		}
	}
}

func TestExecReturnsTokenSourceInterruptionBeforeSend(t *testing.T) {
	c, srv, org := newTestClient(t, "")
	session := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	cause := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("network interrupted")}
	providerErr := fmt.Errorf("refresh: %w", cause)
	tokenCalls := 0
	c.credential = tokenCredential(func(context.Context) (string, error) {
		tokenCalls++
		if tokenCalls == 1 {
			return "", providerErr
		}
		return srv.AccessToken, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	result, err := c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: session.GetDevSessionId(), Command: []string{"unused"}})
	var streamErr *ExecStreamError
	if result != nil || !errors.Is(err, providerErr) || errors.As(err, &streamErr) {
		t.Errorf("Exec = (%v, %v), want no result and the provider error", result, err)
	}
	if tokenCalls != 1 {
		t.Errorf("token source called %d times, want 1", tokenCalls)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); n != 0 {
		t.Errorf("start received %d requests, want 0", n)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); n != 0 {
		t.Errorf("attach received %d requests, want 0", n)
	}
}

func TestWaitAndWatchRecoverAfterTokenSourceInterruption(t *testing.T) {
	for _, cause := range []error{
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("network interrupted")},
		context.Canceled,
		context.DeadlineExceeded,
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			session := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
			interruption := fmt.Errorf("refresh: %w", cause)
			calls := 0
			c.credential = tokenCredential(func(context.Context) (string, error) {
				calls++
				if calls == 1 {
					return "", interruption
				}
				return srv.AccessToken, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got, err := c.WaitDevSession(ctx, WaitDevSessionParams{OrganizationID: org, DevSessionID: session.GetDevSessionId()})
			if err != nil || got.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING || calls != 2 {
				t.Fatalf("wait: state = %v, error = %v, token calls = %d", got.GetState(), err, calls)
			}

			calls = 0
			stop := errors.New("snapshot received")
			gaps := 0
			err = c.WatchQueue(ctx, org, func(ev QueueEvent) error {
				switch ev.Kind {
				case QueueEventGap:
					gaps++
					if !errors.Is(ev.Err, cause) {
						t.Errorf("gap error = %v, want %v", ev.Err, cause)
					}
				case QueueEventSnapshotComplete:
					return stop
				}
				return nil
			})
			if err != stop || calls != 2 || gaps != 1 {
				t.Fatalf("watch: error = %v, token calls = %d, gaps = %d", err, calls, gaps)
			}
		})
	}
}
