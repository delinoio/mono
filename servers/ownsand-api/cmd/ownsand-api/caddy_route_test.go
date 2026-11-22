// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// These tests verify the Caddy route that scripts/ci/droplet-deploy.sh writes
// for ownsand-api (deploy.upstream "h2c") against a real Caddy binary and the
// real HTTP server of this command (newServer). They are opt-in, because CI
// has no Caddy binary:
//
//	OWNSAND_CADDY_TEST_BINARY=<caddy> go test ./servers/ownsand-api/cmd/ownsand-api/ -run TestCaddyRoute -v
//
// Use the Caddy release that scripts/droplet/provision.sh pins.
// OWNSAND_CADDY_TEST_STREAM_DURATION sets how long the long-lived streams of
// TestCaddyRouteKeepsLongStreams run (Go duration, default 2m). The
// acceptance run uses 15m, longer than the 14-minute relay stream limit.
const (
	envCaddyBinary         = "OWNSAND_CADDY_TEST_BINARY"
	envCaddyStreamDuration = "OWNSAND_CADDY_TEST_STREAM_DURATION"
)

// routeHostname is the production host name of the route. The test client
// sends it as the TLS server name and dials the loopback Caddy listener.
const routeHostname = "ownsand.api.delino.io"

// Production intervals of the streams that pass through the route (see
// docs/servers-ownsand-api-foundation.md and docs/protos-ownsand-api-contract.md).
const (
	// The agent sends a heartbeat on the agent channel every 15 seconds.
	heartbeatInterval = 15 * time.Second
	// Each side of a relay connection sends a keepalive frame after 30
	// seconds without an outbound frame.
	relayKeepaliveInterval = 30 * time.Second
	// A relay server stream sends a keepalive event at least every 60
	// seconds while the command produces no output.
	execKeepaliveInterval = 60 * time.Second
	// The WriteFile call deadline.
	writeFileDeadline = 5 * time.Minute
)

// caddyRoute returns the route file that write_route in
// scripts/ci/droplet-deploy.sh writes for a service. Keep both equal;
// scripts/ci/deploy-scripts.test.mjs checks the script side with the same
// text. The route sets no flush_interval: Caddy flushes a response without
// a Content-Length (every Connect stream) at once by default.
func caddyRoute(service, hostname, upstream string, h2c bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by scripts/ci/droplet-deploy.sh for %s. Do not edit.\n", service)
	fmt.Fprintf(&b, "%s {\n", hostname)
	fmt.Fprintf(&b, "\treverse_proxy %s {\n", upstream)
	b.WriteString("\t\tlb_try_duration 10s\n")
	if h2c {
		b.WriteString("\t\ttransport http {\n")
		b.WriteString("\t\t\tversions h2c\n")
		b.WriteString("\t\t}\n")
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// routeBackend is one ownsand-api HTTP server (newServer) with fake handlers
// of the services whose streams pass through the route.
type routeBackend struct {
	name string
	addr string
	// protoMajor records the HTTP major version of the last proxied request.
	protoMajor atomic.Int32
	// requests counts the RPCs that reached this backend.
	requests atomic.Int64
}

func startRouteBackend(t *testing.T, name string) *routeBackend {
	t.Helper()
	b := &routeBackend{name: name}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	})
	mux.Handle(ownsandv1connect.NewAgentServiceHandler(routeAgentService{}))
	mux.Handle(ownsandv1connect.NewRelayAgentServiceHandler(routeRelayAgentService{}))
	mux.Handle(ownsandv1connect.NewRelayServiceHandler(routeRelayService{}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			b.protoMajor.Store(int32(r.ProtoMajor))
			b.requests.Add(1)
		}
		w.Header().Set("X-Test-Backend", name)
		mux.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b.addr = listener.Addr().String()
	srv := newServer(handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return b
}

// routeAgentService answers every heartbeat on the agent channel with an
// acknowledgement, like the control plane.
type routeAgentService struct {
	ownsandv1connect.UnimplementedAgentServiceHandler
}

func (routeAgentService) Channel(ctx context.Context, stream *connect.BidiStream[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) error {
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ack := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{
			HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: req.GetEnvelope().GetHeartbeat().GetSequence()},
		}}
		if err := stream.Send(&ownsandv1.ChannelResponse{Envelope: ack}); err != nil {
			return err
		}
	}
}

// routeRelayAgentService echoes every frame of a relay connection.
type routeRelayAgentService struct {
	ownsandv1connect.UnimplementedRelayAgentServiceHandler
}

func (routeRelayAgentService) Connect(ctx context.Context, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&ownsandv1.ConnectResponse{Frame: req.GetFrame()}); err != nil {
			return err
		}
	}
}

// routeRelayService streams keepalive events for the duration in the
// request's timeout field (Exec), and answers WriteFile after the duration in
// the request's offset field (milliseconds).
type routeRelayService struct {
	ownsandv1connect.UnimplementedRelayServiceHandler
}

func (routeRelayService) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecResponse]) error {
	end := time.Now().Add(req.Msg.GetTimeout().AsDuration())
	keepalive := &ownsandv1.ExecResponse{Event: &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}}
	if err := stream.Send(keepalive); err != nil {
		return err
	}
	for {
		wait := min(execKeepaliveInterval, time.Until(end))
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if err := stream.Send(keepalive); err != nil {
			return err
		}
	}
	exited := &ownsandv1.ExecResponse{Event: &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{
		Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0},
	}}}}
	return stream.Send(exited)
}

func (routeRelayService) WriteFile(ctx context.Context, req *connect.Request[ownsandv1.WriteFileRequest]) (*connect.Response[ownsandv1.WriteFileResponse], error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Duration(req.Msg.GetOffset()) * time.Millisecond):
	}
	return connect.NewResponse(&ownsandv1.WriteFileResponse{}), nil
}

// caddyProxy is a running Caddy process with the Caddyfile layout of
// scripts/droplet/provision.sh (`import sites/*.caddy`), its local CA in
// place of Let's Encrypt and loopback ports.
type caddyProxy struct {
	t         *testing.T
	bin       string
	dir       string
	caddyfile string
	adminAddr string
	httpsAddr string
	clientTLS *tls.Config
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func startCaddy(t *testing.T, bin string) *caddyProxy {
	t.Helper()
	dir := t.TempDir()
	c := &caddyProxy{t: t, bin: bin, dir: dir}
	c.adminAddr = "127.0.0.1:" + freeLoopbackPort(t)
	httpPort := freeLoopbackPort(t)
	httpsPort := freeLoopbackPort(t)
	c.httpsAddr = "127.0.0.1:" + httpsPort
	if err := os.MkdirAll(filepath.Join(dir, "sites"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.caddyfile = filepath.Join(dir, "Caddyfile")
	// provision.sh writes `{ email <acme-email> }` and `import sites/*.caddy`.
	// The test keeps the import and replaces ACME with the local CA, and the
	// public ports with loopback ports.
	global := strings.Join([]string{
		"{",
		"\tadmin " + c.adminAddr,
		"\tstorage file_system " + filepath.Join(dir, "data"),
		"\tlocal_certs",
		"\tskip_install_trust",
		"\thttp_port " + httpPort,
		"\thttps_port " + httpsPort,
		"\tdefault_bind 127.0.0.1",
		"}",
		"",
		"import sites/*.caddy",
		"",
	}, "\n")
	if err := os.WriteFile(c.caddyfile, []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *caddyProxy) env() []string {
	return append(os.Environ(),
		"HOME="+c.dir,
		"XDG_CONFIG_HOME="+filepath.Join(c.dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(c.dir, "xdg-data"),
	)
}

// writeSite writes one route file, as droplet-deploy.sh does.
func (c *caddyProxy) writeSite(service, route string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, "sites", service+".caddy"), []byte(route), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// run starts Caddy with the current site files and waits until the route
// answers.
func (c *caddyProxy) run() {
	c.t.Helper()
	cmd := exec.Command(c.bin, "run", "--config", c.caddyfile, "--adapter", "caddyfile")
	cmd.Dir = c.dir
	cmd.Env = c.env()
	logFile, err := os.Create(filepath.Join(c.dir, "caddy.log"))
	if err != nil {
		c.t.Fatal(err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		if c.t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil {
				c.t.Logf("caddy log:\n%s", b)
			}
		}
	})
	root := filepath.Join(c.dir, "data", "pki", "authorities", "local", "root.crt")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if pem, err := os.ReadFile(root); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				c.clientTLS = &tls.Config{RootCAs: pool, ServerName: routeHostname, MinVersion: tls.VersionTLS12}
				if c.healthy() {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatal("caddy did not serve the route within 30 seconds")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (c *caddyProxy) healthy() bool {
	client := c.httpClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+routeHostname+"/healthz", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// reload runs the reload command of droplet-deploy.sh against the test
// admin address.
func (c *caddyProxy) reload() {
	c.t.Helper()
	cmd := exec.Command(c.bin, "reload", "--config", c.caddyfile, "--adapter", "caddyfile", "--address", c.adminAddr)
	cmd.Dir = c.dir
	cmd.Env = c.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		c.t.Fatalf("caddy reload: %v\n%s", err, out)
	}
}

// httpClient returns a client with the transport settings of the device
// agent's control plane client (cmds/ownsand-agent/internal/cpclient): HTTP/2
// over TLS with ALPN and HTTP/2 health checks. It dials the loopback Caddy
// listener for the production host name.
func (c *caddyProxy) httpClient() *http.Client {
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, c.httpsAddr)
		},
		TLSClientConfig:     c.clientTLS,
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		Protocols:           &protocols,
		ForceAttemptHTTP2:   true,
	}
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 15 * time.Second}
	return &http.Client{Transport: transport}
}

const routeOrigin = "https://" + routeHostname

func caddyBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv(envCaddyBinary)
	if bin == "" {
		t.Skipf("set %s to a Caddy binary to verify the reverse proxy route", envCaddyBinary)
	}
	return bin
}

// heartbeatRoundTrip sends one heartbeat on the agent channel and waits for
// its acknowledgement.
func heartbeatRoundTrip(stream *connect.BidiStreamForClient[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse], seq uint64, timeout time.Duration) error {
	hb := &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Heartbeat{Heartbeat: &ownsandv1.Heartbeat{Sequence: seq}}}
	if err := stream.Send(&ownsandv1.ChannelRequest{Envelope: hb}); err != nil {
		return fmt.Errorf("send heartbeat %d: %w", seq, err)
	}
	type result struct {
		resp *ownsandv1.ChannelResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := stream.Receive()
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return fmt.Errorf("receive the acknowledgement of heartbeat %d: %w", seq, r.err)
		}
		if got := r.resp.GetEnvelope().GetHeartbeatAck().GetSequence(); got != seq {
			return fmt.Errorf("acknowledgement carries sequence %d, want %d", got, seq)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("no acknowledgement of heartbeat %d within %s (the proxy buffers the stream)", seq, timeout)
	}
}

// relayRoundTrip sends one keepalive frame on a relay connection and waits
// for its echo.
func relayRoundTrip(stream *connect.BidiStreamForClient[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse], id uint64, timeout time.Duration) error {
	frame := &ownsandv1.DataFrame{StreamId: id, Payload: &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: &ownsandv1.RelayKeepalive{}}}
	if err := stream.Send(&ownsandv1.ConnectRequest{Frame: frame}); err != nil {
		return fmt.Errorf("send relay frame %d: %w", id, err)
	}
	type result struct {
		resp *ownsandv1.ConnectResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := stream.Receive()
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return fmt.Errorf("receive the echo of relay frame %d: %w", id, r.err)
		}
		if got := r.resp.GetFrame().GetStreamId(); got != id {
			return fmt.Errorf("echo carries stream ID %d, want %d", got, id)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("no echo of relay frame %d within %s (the proxy buffers the stream)", id, timeout)
	}
}

// TestCaddyRouteCarriesBidirectionalStreams checks that the h2c route
// proxies HTTP/2 to the container and forwards each message of the agent
// channel and the relay connection at once in both directions.
func TestCaddyRouteCarriesBidirectionalStreams(t *testing.T) {
	bin := caddyBinary(t)
	backend := startRouteBackend(t, "a")
	caddy := startCaddy(t, bin)
	caddy.writeSite("ownsand-api", caddyRoute("ownsand-api", routeHostname, backend.addr, true))
	caddy.run()

	client := caddy.httpClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	agent := ownsandv1connect.NewAgentServiceClient(client, routeOrigin)
	channel := agent.Channel(ctx)
	for seq := uint64(1); seq <= 20; seq++ {
		if err := heartbeatRoundTrip(channel, seq, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if got := backend.protoMajor.Load(); got != 2 {
		t.Fatalf("the backend received the agent channel over HTTP/%d, want HTTP/2 (h2c)", got)
	}
	if err := channel.CloseRequest(); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Receive(); !errors.Is(err, io.EOF) {
		t.Fatalf("agent channel end: %v, want EOF", err)
	}
	_ = channel.CloseResponse()

	relay := ownsandv1connect.NewRelayAgentServiceClient(client, routeOrigin)
	conn := relay.Connect(ctx)
	for id := uint64(1); id <= 20; id++ {
		if err := relayRoundTrip(conn, id, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.CloseRequest()
	_ = conn.CloseResponse()

	// A relay server stream from a browser (the TypeScript SDK) carries the
	// browser's Accept-Encoding. The first keepalive event must still arrive
	// at once, not when the next one fills a proxy buffer 60 seconds later.
	req := connect.NewRequest(&ownsandv1.ExecRequest{Timeout: durationpb.New(30 * time.Second)})
	req.Header().Set("Accept-Encoding", "gzip, deflate, br, zstd")
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	exec, err := ownsandv1connect.NewRelayServiceClient(client, routeOrigin).Exec(execCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan bool, 1)
	go func() { first <- exec.Receive() }()
	select {
	case ok := <-first:
		if !ok {
			t.Fatalf("first exec event: %v", exec.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first exec event did not arrive within 5 seconds (the proxy buffers the server stream)")
	}
	execCancel()
	_ = exec.Close()
}

// TestCaddyRouteWithoutH2CFailsBidirectionalStreams is the control case: the
// default route of droplet-deploy.sh proxies HTTP/1.1, which cannot carry the
// agent channel.
func TestCaddyRouteWithoutH2CFailsBidirectionalStreams(t *testing.T) {
	bin := caddyBinary(t)
	backend := startRouteBackend(t, "a")
	caddy := startCaddy(t, bin)
	caddy.writeSite("ownsand-api", caddyRoute("ownsand-api", routeHostname, backend.addr, false))
	caddy.run()

	client := caddy.httpClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	channel := ownsandv1connect.NewAgentServiceClient(client, routeOrigin).Channel(ctx)
	defer func() {
		_ = channel.CloseRequest()
		_ = channel.CloseResponse()
	}()
	err := heartbeatRoundTrip(channel, 1, 10*time.Second)
	if err == nil {
		t.Fatal("the agent channel worked through an HTTP/1.1 route; the h2c transport would not be needed")
	}
	t.Logf("HTTP/1.1 route: %v (backend saw HTTP/%d)", err, backend.protoMajor.Load())
}

// TestCaddyRouteKeepsLongStreams runs the agent channel, a relay connection,
// a relay server stream and a slow unary call through the route at their
// production intervals, and checks that the route ends none of them.
func TestCaddyRouteKeepsLongStreams(t *testing.T) {
	bin := caddyBinary(t)
	duration := 2 * time.Minute
	if v := os.Getenv(envCaddyStreamDuration); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			t.Fatalf("%s=%q is not a positive Go duration", envCaddyStreamDuration, v)
		}
		duration = d
	}
	backend := startRouteBackend(t, "a")
	caddy := startCaddy(t, bin)
	caddy.writeSite("ownsand-api", caddyRoute("ownsand-api", routeHostname, backend.addr, true))
	caddy.run()

	client := caddy.httpClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), duration+2*time.Minute)
	defer cancel()
	t.Logf("streams run for %s", duration)

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if err := f(); err != nil {
				errs <- fmt.Errorf("%s after %s: %w", name, time.Since(start).Round(time.Second), err)
				return
			}
			t.Logf("%s ran for %s", name, time.Since(start).Round(time.Second))
		}()
	}
	run("agent channel", func() error {
		channel := ownsandv1connect.NewAgentServiceClient(client, routeOrigin).Channel(ctx)
		defer func() { _ = channel.CloseResponse() }()
		end := time.Now().Add(duration)
		for seq := uint64(1); ; seq++ {
			if err := heartbeatRoundTrip(channel, seq, 10*time.Second); err != nil {
				return err
			}
			if time.Now().Add(heartbeatInterval).After(end) {
				break
			}
			time.Sleep(heartbeatInterval)
		}
		return channel.CloseRequest()
	})
	run("relay connection", func() error {
		conn := ownsandv1connect.NewRelayAgentServiceClient(client, routeOrigin).Connect(ctx)
		defer func() { _ = conn.CloseResponse() }()
		end := time.Now().Add(duration)
		for id := uint64(1); ; id++ {
			if err := relayRoundTrip(conn, id, 10*time.Second); err != nil {
				return err
			}
			if time.Now().Add(relayKeepaliveInterval).After(end) {
				break
			}
			time.Sleep(relayKeepaliveInterval)
		}
		return conn.CloseRequest()
	})
	run("relay server stream", func() error {
		req := connect.NewRequest(&ownsandv1.ExecRequest{Timeout: durationpb.New(duration)})
		stream, err := ownsandv1connect.NewRelayServiceClient(client, routeOrigin).Exec(ctx, req)
		if err != nil {
			return err
		}
		defer stream.Close()
		for stream.Receive() {
			if stream.Msg().GetEvent().GetExited() != nil {
				return nil
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		return errors.New("the stream ended without the exited event")
	})
	run("unary call", func() error {
		wait := min(duration, writeFileDeadline-10*time.Second)
		req := connect.NewRequest(&ownsandv1.WriteFileRequest{Offset: wait.Milliseconds()})
		_, err := ownsandv1connect.NewRelayServiceClient(client, routeOrigin).WriteFile(ctx, req)
		return err
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestCaddyRouteReloadKeepsStreams reloads Caddy while the streams run, as
// every deployment of any server on the Droplet does: first with a route of
// another service, then with a new upstream for ownsand-api (its own
// deployment). The open streams keep working on the previous upstream, and
// new calls reach the new one.
func TestCaddyRouteReloadKeepsStreams(t *testing.T) {
	bin := caddyBinary(t)
	oldBackend := startRouteBackend(t, "old")
	newBackend := startRouteBackend(t, "new")
	other := startRouteBackend(t, "other")
	caddy := startCaddy(t, bin)
	caddy.writeSite("ownsand-api", caddyRoute("ownsand-api", routeHostname, oldBackend.addr, true))
	caddy.run()

	client := caddy.httpClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	channel := ownsandv1connect.NewAgentServiceClient(client, routeOrigin).Channel(ctx)
	defer func() { _ = channel.CloseResponse() }()
	conn := ownsandv1connect.NewRelayAgentServiceClient(client, routeOrigin).Connect(ctx)
	defer func() { _ = conn.CloseResponse() }()
	execReq := connect.NewRequest(&ownsandv1.ExecRequest{Timeout: durationpb.New(20 * time.Second)})
	exec, err := ownsandv1connect.NewRelayServiceClient(client, routeOrigin).Exec(ctx, execReq)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	if !exec.Receive() {
		t.Fatalf("first exec event: %v", exec.Err())
	}

	seq := uint64(0)
	roundTrips := func(stage string) {
		t.Helper()
		for range 3 {
			seq++
			if err := heartbeatRoundTrip(channel, seq, 5*time.Second); err != nil {
				t.Fatalf("%s: %v", stage, err)
			}
			if err := relayRoundTrip(conn, seq, 5*time.Second); err != nil {
				t.Fatalf("%s: %v", stage, err)
			}
			time.Sleep(time.Second)
		}
	}
	roundTrips("before the reloads")

	caddy.writeSite("accounts-api", caddyRoute("accounts-api", "accounts.api.delino.io", other.addr, false))
	caddy.reload()
	roundTrips("after the reload for another service")

	before := newBackend.requests.Load()
	caddy.writeSite("ownsand-api", caddyRoute("ownsand-api", routeHostname, newBackend.addr, true))
	caddy.reload()
	roundTrips("after the reload with a new ownsand-api upstream")

	// The exec stream that started before both reloads still ends with its
	// exited event.
	for exec.Receive() {
		if exec.Msg().GetEvent().GetExited() != nil {
			break
		}
	}
	if err := exec.Err(); err != nil {
		t.Fatalf("exec stream across the reloads: %v", err)
	}

	// A new call reaches the new upstream.
	fresh := ownsandv1connect.NewAgentServiceClient(client, routeOrigin).Channel(ctx)
	if err := heartbeatRoundTrip(fresh, 1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	_ = fresh.CloseRequest()
	_ = fresh.CloseResponse()
	if newBackend.requests.Load() == before {
		t.Fatal("a new agent channel after the reload did not reach the new upstream")
	}
	if err := channel.CloseRequest(); err != nil {
		t.Fatal(err)
	}
	_ = conn.CloseRequest()
}
