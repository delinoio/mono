package ownsand

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

// loopbackOnly restricts pion to loopback UDP candidates, so the
// peer-to-peer tests need no network.
func loopbackOnly(se *webrtc.SettingEngine) {
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
}

type env struct {
	t   *testing.T
	srv *ownsandtest.Server
	c   *Client
	sb  *Sandbox
	ctx context.Context
}

func newEnv(t *testing.T, mode TransportMode) *env {
	t.Helper()
	srv := ownsandtest.New(t, ownsandtest.Options{})
	if mode != TransportRelay {
		srv.EnablePeerToPeer(ownsandtest.PeerOptions{Setup: loopbackOnly})
	}
	c, err := New(Options{Origin: srv.URL, APIKey: ownsandtest.DefaultAPIKey, OrganizationID: srv.Org, Clock: srv.Clock, Transport: mode, peerSetup: loopbackOnly})
	if err != nil {
		t.Fatal(err)
	}
	info := srv.AddSandbox(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	sb, err := c.Sandbox(info.GetSandboxId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return &env{t: t, srv: srv, c: c, sb: sb, ctx: ctx}
}

// run executes a command and collects its events and output.
func (e *env) run(o ExecOptions) (*ExecResult, []*ExecEvent, string, string, error) {
	e.t.Helper()
	st, err := e.sb.Exec(e.ctx, o)
	if err != nil {
		return nil, nil, "", "", err
	}
	return collect(st)
}

func collect(st *ExecStream) (*ExecResult, []*ExecEvent, string, string, error) {
	var evs []*ExecEvent
	var out, errb bytes.Buffer
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, evs, out.String(), errb.String(), err
		}
		evs = append(evs, ev)
		switch ev.Kind {
		case ExecEventStdout:
			out.Write(ev.Data)
		case ExecEventStderr:
			errb.Write(ev.Data)
		}
	}
	res, err := st.Result()
	return res, evs, out.String(), errb.String(), err
}

func kinds(evs []*ExecEvent) []ExecEventKind {
	var ks []ExecEventKind
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return ks
}

func count(evs []*ExecEvent, k ExecEventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// program registers a fake command.
func (e *env) program(name string, p ownsandtest.Program) {
	e.srv.Agent.Programs[name] = p
}

// sleepV waits for d of virtual time on the fake clock.
func (e *env) sleepV(ctx context.Context, d time.Duration) {
	t := e.srv.Clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C():
	}
}

func relayCallsOf(calls []ownsandtest.RelayCall, rpc ownsandtest.RelayRPC) []ownsandtest.RelayCall {
	var out []ownsandtest.RelayCall
	for _, c := range calls {
		if c.RPC == rpc {
			out = append(out, c)
		}
	}
	return out
}
