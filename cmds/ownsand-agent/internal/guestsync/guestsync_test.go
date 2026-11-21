// SPDX-License-Identifier: FSL-1.1-ALv2

package guestsync

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// fakeGuest answers every exec with a fixed exited event.
type fakeGuest struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	exited *ownsandv1.ExecExited
}

func (g *fakeGuest) Serve(_ context.Context, s *connect.BidiStream[ownsandv1.ServeRequest, ownsandv1.ServeResponse]) error {
	for {
		req, err := s.Receive()
		if err != nil {
			return nil
		}
		exec := req.GetFrame().GetExecRequest()
		if exec == nil {
			continue
		}
		if err := s.Send(&ownsandv1.ServeResponse{Frame: datapath.MustNewFrame(req.GetFrame().GetStreamId(), exec.GetSandboxId(),
			&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: g.exited}})}); err != nil {
			return nil
		}
	}
}

func TestSyncExitStatus(t *testing.T) {
	cases := []struct {
		name    string
		exited  *ownsandv1.ExecExited
		wantErr string
	}{
		{"zero exit", &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}, ""},
		{"nonzero exit", &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 3}}, "exited with 3"},
		{"signal", &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_Signal{Signal: 9}}, "signal 9"},
		{"missing status", &ownsandv1.ExecExited{}, "without a status"},
		{"timed out", &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}, TimedOut: true}, "timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle(ownsandv1connect.NewGuestServiceHandler(&fakeGuest{exited: tc.exited}))
			var protocols http.Protocols
			protocols.SetUnencryptedHTTP2(true)
			srv := httptest.NewUnstartedServer(mux)
			srv.Config.Protocols = &protocols
			srv.Start()
			t.Cleanup(srv.Close)
			dial := func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}
			err := Sync(context.Background(), "sandbox", dial)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Sync() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Sync() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
