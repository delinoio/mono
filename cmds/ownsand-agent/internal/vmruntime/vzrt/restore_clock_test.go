// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

func TestRestoreClock(t *testing.T) {
	clockFailure := errors.New("clock RPC failed")
	for _, tc := range []struct {
		name        string
		cancel      bool
		rpcError    error
		wait        bool
		parentLimit time.Duration
		want        error
		wantWarning bool
	}{
		{name: "parent canceled", cancel: true, rpcError: context.Canceled, want: context.Canceled},
		{name: "parent canceled as clock succeeds", cancel: true, want: context.Canceled},
		{name: "parent deadline", wait: true, parentLimit: time.Second, want: context.DeadlineExceeded},
		{name: "clock deadline only", wait: true, wantWarning: true},
		{name: "clock error only", rpcError: clockFailure, wantWarning: true},
		{name: "clock succeeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.parentLimit > 0 {
					var stopTimer context.CancelFunc
					ctx, stopTimer = context.WithTimeout(ctx, tc.parentLimit)
					defer stopTimer()
				}
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				err := syncRestoreClock(ctx, logger, "sandbox", func(clockCtx context.Context, _ time.Time) error {
					if ctx.Err() != nil {
						t.Fatal("create context ended before the RPC")
					}
					if tc.cancel {
						cancel()
					}
					if tc.wait {
						<-clockCtx.Done()
						return clockCtx.Err()
					}
					return tc.rpcError
				})
				if !errors.Is(err, tc.want) || !errors.Is(ctx.Err(), tc.want) {
					t.Fatalf("clock and parent errors = %v and %v, want %v", err, ctx.Err(), tc.want)
				}
				if warned := strings.Contains(logs.String(), "level=WARN msg=\"set guest clock after memory restore failed\""); warned != tc.wantWarning {
					t.Fatalf("clock logs = %q, want warning = %t", &logs, tc.wantWarning)
				}
			})
		})
	}
}

type gatedClockRPC struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	entered chan struct{}
}

func (g gatedClockRPC) SetClock(ctx context.Context, _ *connect.Request[ownsandv1.SetClockRequest]) (*connect.Response[ownsandv1.SetClockResponse], error) {
	close(g.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRestoreClockLiveRPC(t *testing.T) {
	// Use the production guest client against a local Connect HTTP/2 server.
	gate := gatedClockRPC{entered: make(chan struct{})}
	_, handler := ownsandv1connect.NewGuestServiceHandler(gate)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, Protocols: &protocols}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	dialer := &net.Dialer{}
	client := guestclient.New(func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", listener.Addr().String())
	})
	t.Cleanup(client.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- syncRestoreClock(ctx, slog.New(slog.NewTextHandler(&logs, nil)), "sandbox", client.SetClock)
	}()
	select {
	case <-gate.entered:
		cancel()
	case err := <-result:
		t.Fatalf("clock exited before the server received the RPC: %v", err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) || logs.Len() != 0 {
		t.Fatalf("clock result = %v, logs = %q, want parent cancellation with no warning", err, &logs)
	}
}
