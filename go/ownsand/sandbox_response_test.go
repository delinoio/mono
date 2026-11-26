// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// resourceControlPlane answers every sandbox and snapshot RPC with a
// successful response. It carries the resource only when empty is false, so a
// test can compare a response that lacks the resource with one that has it.
type resourceControlPlane struct {
	ownsandv1connect.UnimplementedSandboxServiceHandler
	ownsandv1connect.UnimplementedSnapshotServiceHandler
	sandboxID   string
	orgID       string
	snapshotID  string
	empty       bool
	createCalls atomic.Int32
	getCalls    atomic.Int32
}

func (f *resourceControlPlane) sandbox(state ownsandv1.SandboxState) *ownsandv1.Sandbox {
	if f.empty {
		return nil
	}
	return &ownsandv1.Sandbox{SandboxId: f.sandboxID, OrganizationId: f.orgID, State: state}
}

func (f *resourceControlPlane) CreateSandbox(context.Context, *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	f.createCalls.Add(1)
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: f.sandbox(ownsandv1.SandboxState_SANDBOX_STATE_CREATING)}), nil
}

func (f *resourceControlPlane) GetSandbox(context.Context, *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	f.getCalls.Add(1)
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: f.sandbox(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)}), nil
}

func (f *resourceControlPlane) PauseSandbox(context.Context, *connect.Request[ownsandv1.PauseSandboxRequest]) (*connect.Response[ownsandv1.PauseSandboxResponse], error) {
	return connect.NewResponse(&ownsandv1.PauseSandboxResponse{Sandbox: f.sandbox(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)}), nil
}

func (f *resourceControlPlane) ResumeSandbox(context.Context, *connect.Request[ownsandv1.ResumeSandboxRequest]) (*connect.Response[ownsandv1.ResumeSandboxResponse], error) {
	return connect.NewResponse(&ownsandv1.ResumeSandboxResponse{Sandbox: f.sandbox(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)}), nil
}

func (f *resourceControlPlane) DeleteSandbox(context.Context, *connect.Request[ownsandv1.DeleteSandboxRequest]) (*connect.Response[ownsandv1.DeleteSandboxResponse], error) {
	return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: f.sandbox(ownsandv1.SandboxState_SANDBOX_STATE_DELETED)}), nil
}

func (f *resourceControlPlane) CreateSnapshot(context.Context, *connect.Request[ownsandv1.CreateSnapshotRequest]) (*connect.Response[ownsandv1.CreateSnapshotResponse], error) {
	if f.empty {
		return connect.NewResponse(&ownsandv1.CreateSnapshotResponse{}), nil
	}
	return connect.NewResponse(&ownsandv1.CreateSnapshotResponse{Snapshot: &ownsandv1.Snapshot{SnapshotId: f.snapshotID}}), nil
}

// startResourceControlPlane serves the fake over h2c and returns a client for
// it and the sandbox handle.
func startResourceControlPlane(t *testing.T, empty bool) (*resourceControlPlane, *Client, *Sandbox) {
	t.Helper()
	cp := &resourceControlPlane{sandboxID: mustID(t), orgID: mustID(t), snapshotID: mustID(t), empty: empty}
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewSandboxServiceHandler(cp))
	mux.Handle(ownsandv1connect.NewSnapshotServiceHandler(cp))
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	c, err := New(Options{BaseURL: srv.URL, APIKey: testAPIKey, OrganizationID: cp.orgID, Transport: TransportRelay, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	s, err := c.Sandbox("", cp.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cp, c, s
}

// TestMissingResourceResponses checks that a successful response without its
// sandbox or snapshot is an error, and that a response with the resource keeps
// the current result.
func TestMissingResourceResponses(t *testing.T) {
	calls := map[string]func(*testing.T, *Client, *Sandbox) (hasResource bool, err error){
		"CreateSandbox": func(t *testing.T, c *Client, _ *Sandbox) (bool, error) {
			sb, err := c.CreateSandbox(context.Background(), validParams())
			if err == nil && (sb == nil || sb.ID() == "") {
				t.Fatalf("CreateSandbox returned an empty handle without an error: %+v", sb)
			}
			return sb != nil, err
		},
		"Get": func(_ *testing.T, _ *Client, s *Sandbox) (bool, error) {
			sb, err := s.Get(context.Background())
			return sb != nil, err
		},
		"Pause": func(_ *testing.T, _ *Client, s *Sandbox) (bool, error) {
			sb, err := s.Pause(context.Background())
			return sb != nil, err
		},
		"Resume": func(_ *testing.T, _ *Client, s *Sandbox) (bool, error) {
			sb, err := s.Resume(context.Background())
			return sb != nil, err
		},
		"Delete": func(_ *testing.T, _ *Client, s *Sandbox) (bool, error) {
			sb, err := s.Delete(context.Background())
			return sb != nil, err
		},
		"Snapshot": func(_ *testing.T, _ *Client, s *Sandbox) (bool, error) {
			snap, err := s.Snapshot(context.Background())
			return snap != nil, err
		},
	}
	for name, call := range calls {
		t.Run(name+"/without resource", func(t *testing.T) {
			cp, c, s := startResourceControlPlane(t, true)
			_, err := call(t, c, s)
			if err == nil {
				t.Fatalf("%s accepted a response without its resource", name)
			}
			if !strings.Contains(err.Error(), "response carried no") {
				t.Fatalf("%s error does not name the missing resource: %v", name, err)
			}
			if kind, _ := classify(context.Background(), err); kind != failTerminal {
				t.Fatalf("%s error is not terminal: %v", name, err)
			}
			if name == "CreateSandbox" && cp.createCalls.Load() != 1 {
				t.Fatalf("CreateSandbox retried a malformed response %d times", cp.createCalls.Load())
			}
			if name != "Snapshot" && name != "CreateSandbox" && s.Info().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
				t.Fatalf("%s replaced the handle record: %v", name, s.Info())
			}
		})
		t.Run(name+"/with resource", func(t *testing.T) {
			_, c, s := startResourceControlPlane(t, false)
			hasResource, err := call(t, c, s)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !hasResource {
				t.Fatalf("%s returned no resource", name)
			}
		})
	}
}

// TestWaitStableStopsOnMissingResource checks that a missing sandbox in a
// GetSandbox response ends WaitStable with the error and does not poll again.
func TestWaitStableStopsOnMissingResource(t *testing.T) {
	cp, _, s := startResourceControlPlane(t, true)
	// The deadline only keeps a regression from hanging the test: without the
	// check, WaitStable polls a nil sandbox forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.WaitStable(ctx); err == nil || !strings.Contains(err.Error(), "response carried no sandbox") {
		t.Fatalf("WaitStable error: %v", err)
	}
	if n := cp.getCalls.Load(); n != 1 {
		t.Fatalf("WaitStable polled %d times", n)
	}
}
