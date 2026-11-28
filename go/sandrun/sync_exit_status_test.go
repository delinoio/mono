// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestSyncLockMissingExitStatus(t *testing.T) {
	testSyncMissingExitStatus(t, opLock)
}

func TestSyncApplyMissingExitStatus(t *testing.T) {
	testSyncMissingExitStatus(t, opApply)
}

func testSyncMissingExitStatus(t *testing.T, op guestOp) {
	t.Helper()
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	params := SyncParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		LocalDir: newRepo(t, map[string]string{"previous.txt": "previous"}, nil, ""),
	}
	if _, err := c.Sync(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	manifestPath := ds.GetWorkspacePath() + manifestSuffix
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(params.LocalDir, "pending.txt"), []byte("pending"), 0o644); err != nil {
		t.Fatal(err)
	}

	lock := ds.GetWorkspacePath() + lockSuffix
	owner := filepath.Join(lock, "owner")
	wantOps := []guestOp{opLock, opApply, opCleanup}
	wantWrites := 1 // The archive is uploaded before apply.
	if op == opLock {
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(owner, []byte("other sync"), 0o644); err != nil {
			t.Fatal(err)
		}
		wantOps, wantWrites = []guestOp{opLock}, 0
	}

	c, err = New(Options{
		BaseURL: newGuestExitStatusServer(t, srv.URL, op), APIKey: srv.AccessToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	execs, writes := len(srv.ExecRequests()), srv.Calls(writeProc)
	res, syncErr := c.Sync(t.Context(), params)
	if res != nil || connect.CodeOf(syncErr) != connect.CodeInternal || connect.IsWireError(syncErr) ||
		!strings.Contains(syncErr.Error(), "guest "+op.String()+" command exited without an exit status") {
		t.Errorf("Sync = %+v, %v, want a local internal error for the guest %s command", res, syncErr, op)
	}
	requests := srv.ExecRequests()[execs:]
	if len(requests) != len(wantOps) {
		t.Errorf("guest commands = %d, want %d", len(requests), len(wantOps))
	} else {
		for i, want := range wantOps {
			cmd, err := guestCommand(ds.GetGuestOs(), want)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(requests[i].GetCommand(), cmd) {
				t.Errorf("guest command %d must be %s", i, want)
			}
		}
	}
	if got := srv.Calls(writeProc) - writes; got != wantWrites {
		t.Errorf("guest file writes = %d, want %d", got, wantWrites)
	}
	if after, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(before, after) {
		t.Errorf("guest manifest changed, read error = %v", err)
	}
	if op == opLock {
		if got, err := os.ReadFile(owner); err != nil || string(got) != "other sync" {
			t.Errorf("other sync's lock marker = %q, %v", got, err)
		}
	} else if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("own lock after cleanup: stat error = %v, want not exist", err)
	}
}

// The fixture removes the selected terminal status before Connect encodes
// the response. It relays all other calls to the fake that runs the guest scripts.
func newGuestExitStatusServer(t *testing.T, baseURL string, op guestOp) string {
	t.Helper()
	target, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	command, err := guestCommand(sandrunv1.GuestOs_GUEST_OS_LINUX, op)
	if err != nil {
		t.Fatal(err)
	}
	_, execHandler := sandrunv1connect.NewDevSessionServiceHandler(&guestExitStatusHandler{
		upstream: sandrunv1connect.NewDevSessionServiceClient(http.DefaultClient, baseURL),
		command:  command,
	})
	proxy := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.Handle(execProc, execHandler)
	mux.Handle("/", proxy)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	return server.URL
}

type guestExitStatusHandler struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	upstream sandrunv1connect.DevSessionServiceClient
	command  []string
}

func (h *guestExitStatusHandler) ExecDevSession(ctx context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], output *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	forward := connect.NewRequest(req.Msg)
	forward.Header().Set("Authorization", req.Header().Get("Authorization"))
	stream, err := h.upstream.ExecDevSession(ctx, forward)
	if err != nil {
		return err
	}
	defer stream.Close()
	dropStatus := slices.Equal(h.command, req.Msg.GetCommand())
	for stream.Receive() {
		response := stream.Msg()
		if exited := response.GetEvent().GetExited(); dropStatus && exited != nil {
			exited.Status = nil
		}
		if err := output.Send(response); err != nil {
			return err
		}
	}
	return stream.Err()
}
