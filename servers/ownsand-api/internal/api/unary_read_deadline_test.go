// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// The tests in this file need no database: they serve small Connect handlers
// behind api.UnaryReadDeadline on a server like the production one (HTTP/1.1
// and HTTP/2 without TLS, no ReadTimeout) and drive them with raw requests.

const (
	// bodyDeadline is the injected body read deadline. It is long enough for
	// a slow upload that is within the budget to finish with margin and
	// short enough that the tests stay fast.
	bodyDeadline = 600 * time.Millisecond

	echoProcedure   = "/test.v1.Probe/Echo"
	slowProcedure   = "/test.v1.Probe/Slow"
	streamProcedure = "/test.v1.Probe/Stream"
)

// deadlineServer serves a unary echo, a unary handler that runs three body
// deadlines after it read its request, and a bidirectional echo stream, all
// behind UnaryReadDeadline with the stream exempt.
func deadlineServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(echoProcedure, connect.NewUnaryHandler(echoProcedure,
		func(_ context.Context, req *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
			return connect.NewResponse(&ownsandv1.GetIceServersResponse{}), nil
		}))
	mux.Handle(slowProcedure, connect.NewUnaryHandler(slowProcedure,
		func(ctx context.Context, req *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
			// A handler that outlives the body deadline, like one that waits
			// for an agent: the deadline must not cancel it.
			select {
			case <-time.After(3 * bodyDeadline):
			case <-ctx.Done():
				return nil, connect.NewError(connect.CodeCanceled, fmt.Errorf("the request context ended: %w", context.Cause(ctx)))
			}
			return connect.NewResponse(&ownsandv1.GetIceServersResponse{}), nil
		}))
	mux.Handle(streamProcedure, connect.NewBidiStreamHandler(streamProcedure,
		func(ctx context.Context, stream *connect.BidiStream[ownsandv1.GetIceServersRequest, ownsandv1.GetIceServersResponse]) error {
			for {
				if _, err := stream.Receive(); err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				if err := stream.Send(&ownsandv1.GetIceServersResponse{}); err != nil {
					return err
				}
			}
		}))
	handler := api.UnaryReadDeadline(mux, bodyDeadline, testenv.Discard(), streamProcedure)

	srv := httptest.NewUnstartedServer(handler)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// h2cClient returns an HTTP client that speaks HTTP/2 without TLS.
func h2cClient(t *testing.T) *http.Client {
	t.Helper()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &protocols}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// uploadStep is one piece of a request body and the pause before it.
type uploadStep struct {
	wait time.Duration
	data []byte
}

// outcome is the end of one request: the HTTP status and Connect error code of
// the response, and how long the request took.
type outcome struct {
	status int
	code   string
	took   time.Duration
}

func connectCode(t *testing.T, body io.Reader) string {
	t.Helper()
	data, _ := io.ReadAll(body)
	var e struct {
		Code string `json:"code"`
	}
	if len(data) > 0 && json.Unmarshal(data, &e) != nil {
		t.Fatalf("response body is not a Connect error: %q", data)
	}
	return e.Code
}

// requestBody returns an encoded request message.
func requestBody(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&ownsandv1.GetIceServersRequest{OrganizationId: "01900000-0000-7000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// postHTTP1 sends a request over a raw HTTP/1.1 connection that announces
// contentLength bytes and then performs steps. It returns when the response
// head arrives, which is when the server ended the call.
func postHTTP1(t *testing.T, srv *httptest.Server, procedure string, contentLength int, steps []uploadStep) outcome {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\nContent-Type: application/proto\r\nContent-Length: %d\r\n\r\n", procedure, contentLength); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for _, s := range steps {
			select {
			case <-time.After(s.wait):
			case <-done:
				return
			}
			if _, err := conn.Write(s.data); err != nil {
				return
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	defer resp.Body.Close()
	took := time.Since(start)
	return outcome{status: resp.StatusCode, code: connectCode(t, resp.Body), took: took}
}

// postH2C sends a request over HTTP/2 without TLS whose body performs steps
// and then, when hold is set, blocks until the call ended.
func postH2C(t *testing.T, srv *httptest.Server, procedure string, steps []uploadStep, hold bool) outcome {
	t.Helper()
	pr, pw := io.Pipe()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+procedure, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	go func() {
		for _, s := range steps {
			select {
			case <-time.After(s.wait):
			case <-ctx.Done():
				return
			}
			if _, err := pw.Write(s.data); err != nil {
				return
			}
		}
		if !hold {
			_ = pw.Close()
			return
		}
		<-ctx.Done()
	}()
	defer pw.CloseWithError(io.ErrClosedPipe)
	resp, err := h2cClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST %s over h2c: %v", procedure, err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("the request used HTTP/%d.%d, want HTTP/2", resp.ProtoMajor, resp.ProtoMinor)
	}
	took := time.Since(start)
	return outcome{status: resp.StatusCode, code: connectCode(t, resp.Body), took: took}
}

// trickle returns steps that send one byte every interval for the given
// duration: a body that never completes.
func trickle(interval, total time.Duration) []uploadStep {
	var steps []uploadStep
	for elapsed := time.Duration(0); elapsed < total; elapsed += interval {
		steps = append(steps, uploadStep{wait: interval, data: []byte{0x0a}})
	}
	return steps
}

// chunks splits body into n steps with the given pause before each.
func chunks(body []byte, n int, pause time.Duration) []uploadStep {
	var steps []uploadStep
	size := (len(body) + n - 1) / n
	for len(body) > 0 {
		k := min(size, len(body))
		steps = append(steps, uploadStep{wait: pause, data: body[:k]})
		body = body[k:]
	}
	return steps
}

// TestUnaryReadDeadline checks, over HTTP/1.1 and over HTTP/2 without TLS,
// that a unary request whose body does not arrive in time ends at the
// deadline with deadline_exceeded, and that a slow upload that completes in
// time and a handler that outlives the deadline are not affected.
func TestUnaryReadDeadline(t *testing.T) {
	srv := deadlineServer(t)
	body := requestBody(t)

	type protocol struct {
		name string
		// post sends a request to the procedure that announces size bytes and
		// performs steps; an upload that does not complete stalls.
		post func(t *testing.T, procedure string, size int, steps []uploadStep) outcome
	}
	protocols := []protocol{
		{"HTTP/1.1", func(t *testing.T, procedure string, size int, steps []uploadStep) outcome {
			return postHTTP1(t, srv, procedure, size, steps)
		}},
		{"h2c", func(t *testing.T, procedure string, size int, steps []uploadStep) outcome {
			return postH2C(t, srv, procedure, steps, size > len(body))
		}},
	}
	for _, p := range protocols {
		t.Run(p.name+"/stalled body", func(t *testing.T) {
			got := p.post(t, echoProcedure, 1<<30, nil)
			assertDeadline(t, got)
		})
		t.Run(p.name+"/body at one byte per interval", func(t *testing.T) {
			got := p.post(t, echoProcedure, 1<<30, trickle(100*time.Millisecond, 5*bodyDeadline))
			assertDeadline(t, got)
		})
		t.Run(p.name+"/slow body that completes within the deadline", func(t *testing.T) {
			steps := chunks(body, 3, bodyDeadline/6)
			got := p.post(t, echoProcedure, len(body), steps)
			if got.status != http.StatusOK || got.code != "" {
				t.Fatalf("slow complete upload: status %d code %q, want 200", got.status, got.code)
			}
		})
		t.Run(p.name+"/handler that outlives the deadline after a complete body", func(t *testing.T) {
			got := p.post(t, slowProcedure, len(body), chunks(body, 1, 0))
			if got.status != http.StatusOK || got.code != "" {
				t.Fatalf("long handler: status %d code %q, want 200 (took %v)", got.status, got.code, got.took)
			}
			if got.took < 3*bodyDeadline {
				t.Fatalf("the handler answered after %v, before its %v of work", got.took, 3*bodyDeadline)
			}
		})
	}
}

func assertDeadline(t *testing.T, got outcome) {
	t.Helper()
	if got.status != http.StatusGatewayTimeout || got.code != "deadline_exceeded" {
		t.Fatalf("status %d code %q after %v, want 504 deadline_exceeded", got.status, got.code, got.took)
	}
	if got.took < bodyDeadline-100*time.Millisecond || got.took > 3*bodyDeadline {
		t.Fatalf("the call ended after %v, want about the %v body deadline", got.took, bodyDeadline)
	}
}

// TestUnaryReadDeadlineSkipsStreams checks that a stream that the handler
// lists is not bounded: it stays open and usable long after the deadline.
func TestUnaryReadDeadlineSkipsStreams(t *testing.T) {
	srv := deadlineServer(t)
	client := connect.NewClient[ownsandv1.GetIceServersRequest, ownsandv1.GetIceServersResponse](h2cClient(t), srv.URL+streamProcedure)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream := client.CallBidiStream(ctx)
	var once sync.Once
	closeStream := func() {
		once.Do(func() {
			_ = stream.CloseRequest()
			_ = stream.CloseResponse()
		})
	}
	defer closeStream()
	for i := 0; i < 3; i++ {
		time.Sleep(bodyDeadline)
		if err := stream.Send(&ownsandv1.GetIceServersRequest{}); err != nil {
			t.Fatalf("send %d after %d deadlines: %v", i, i+1, err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("receive %d after %d deadlines: %v", i, i+1, err)
		}
	}
	// Control: the same body on a unary procedure is cut off.
	if got := postH2C(t, srv, echoProcedure, nil, true); got.code != "deadline_exceeded" {
		t.Fatalf("control stalled unary body: status %d code %q", got.status, got.code)
	}
}
