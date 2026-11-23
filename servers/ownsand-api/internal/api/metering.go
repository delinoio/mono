// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

// UsageRecorder records billable usage of an organization. billing.Metering
// implements it.
type UsageRecorder interface {
	Add(orgID string, meter ownsandv1.BillingMeter, at time.Time, quantity int64)
}

// ErrUsageDrainIncomplete is returned by DrainUsage when requests that can
// still record usage did not end before the context ended.
var ErrUsageDrainIncomplete = errors.New("api: requests that record usage did not finish")

// usageGate counts the requests that can still record usage. A request
// enters before its handler runs and leaves after it recorded its usage, so
// waiting for the count to reach zero orders the final usage flush after
// every such request. The zero value is ready to use.
type usageGate struct {
	mu      sync.Mutex
	n       int
	waiters []chan struct{}
}

// enter registers one request and returns the function that ends it.
func (g *usageGate) enter() (leave func()) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.n--
			if g.n == 0 {
				for _, w := range g.waiters {
					close(w)
				}
				g.waiters = nil
			}
		})
	}
}

// wait blocks until no request is registered or ctx ends. It returns the
// number of requests that were still registered when it returned.
func (g *usageGate) wait(ctx context.Context) int {
	g.mu.Lock()
	if g.n == 0 {
		g.mu.Unlock()
		return 0
	}
	w := make(chan struct{})
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()
	select {
	case <-w:
		return 0
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.n
	}
}

// enterUsage registers a request that can record usage; the returned
// function ends it and is safe to call more than once. Without metering it
// registers nothing.
func (s *Service) enterUsage() (leave func()) {
	if s.Usage == nil {
		return func() {}
	}
	return s.usageWriters.enter()
}

// enterBilledRPC registers an RPC of a billed class. An RPC that is never
// billed (such as the long-lived agent channel) records no usage, so it
// does not hold the shutdown drain.
func (s *Service) enterBilledRPC(procedure string) (leave func()) {
	if _, billed := billing.ClassifyRPC(procedure).Meter(); !billed {
		return func() {}
	}
	return s.enterUsage()
}

// DrainUsage waits until every request that can record usage has recorded
// it and ended, or ctx ends. It returns ErrUsageDrainIncomplete, wrapped
// with the number of requests that are still running, when ctx ends first.
// Call it after the HTTP server stopped accepting requests and before the
// final usage flush.
func (s *Service) DrainUsage(ctx context.Context) error {
	if left := s.usageWriters.wait(ctx); left > 0 {
		return fmt.Errorf("%w: %d still running", ErrUsageDrainIncomplete, left)
	}
	return nil
}

// recordUsage records usage when metering is wired.
func (s *Service) recordUsage(orgID string, meter ownsandv1.BillingMeter, quantity int64) {
	if s.Usage != nil {
		s.Usage.Add(orgID, meter, s.now(), quantity)
	}
}

// meterInterceptor counts each billed RPC that ends with an OK status once
// for its organization, in the class of the billing classification. It runs
// inside the logging interceptor, whose RPC fields carry the principal that
// authentication stored and the organization that the handler authorized,
// and outside the authentication interceptor, so a call that fails
// authentication, authorization, validation or any later step returns an
// error and is never counted.
type meterInterceptor struct{ s *Service }

// meter counts a finished RPC.
func (m *meterInterceptor) meter(ctx context.Context, procedure string, err error) {
	if err != nil || m.s.Usage == nil {
		return
	}
	class := billing.ClassifyRPC(procedure)
	meter, billed := class.Meter()
	if !billed {
		if class == billing.RPCClassUnclassified {
			m.s.Logger.ErrorContext(ctx, "billing_rpc_unclassified", slog.String("procedure", procedure))
		}
		return
	}
	info := rpcInfoFrom(ctx)
	if info == nil {
		return
	}
	info.mu.Lock()
	p, orgID := info.principal, info.organizationID
	info.mu.Unlock()
	// Only calls that a Logto access token or an API key authenticated are
	// billed; Enroll is authenticated by its enrollment token instead.
	if !p.IsUser() && !p.IsAPIKey() && procedure != ownsandv1connect.AgentServiceEnrollProcedure {
		return
	}
	if orgID == "" {
		m.s.Logger.ErrorContext(ctx, "billing_rpc_without_organization", slog.String("procedure", procedure))
		return
	}
	m.s.recordUsage(orgID, meter, 1)
}

func (m *meterInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		leave := m.s.enterBilledRPC(req.Spec().Procedure)
		defer leave()
		resp, err := next(ctx, req)
		m.meter(ctx, req.Spec().Procedure, err)
		return resp, err
	}
}

func (m *meterInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (m *meterInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		leave := m.s.enterBilledRPC(conn.Spec().Procedure)
		defer leave()
		err := next(ctx, conn)
		m.meter(ctx, conn.Spec().Procedure, err)
		return err
	}
}

// relayEgress counts the response bytes of one relay RPC toward the caller
// once the handler has matched the request to its grant (startRelayEgress).
type relayEgress struct {
	s   *Service
	org atomic.Pointer[string]
}

type relayEgressKey struct{}

// egressWriter counts the bytes that the Connect handler writes to the
// response body: every message with its envelope, including the end of
// stream message. The TLS and HTTP/2 framing that the HTTP server and the
// reverse proxy add is not visible here and is not counted.
type egressWriter struct {
	http.ResponseWriter
	meter *relayEgress
}

func (w *egressWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		if org := w.meter.org.Load(); org != nil {
			w.meter.s.recordUsage(*org, ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS, int64(n))
		}
	}
	return n, err
}

// Flush keeps the server streams flushable; Connect requires it.
func (w *egressWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController the underlying writer.
func (w *egressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// MeterRelayEgress counts the relay egress toward the caller of each
// RelayService request. Wrap the RelayService handler with it.
func (p *DataPlane) MeterRelayEgress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The egress writer records until the handler returns.
		leave := p.S.enterUsage()
		defer leave()
		meter := &relayEgress{s: p.S}
		ctx := context.WithValue(r.Context(), relayEgressKey{}, meter)
		next.ServeHTTP(&egressWriter{ResponseWriter: w, meter: meter}, r.WithContext(ctx))
	})
}

// startRelayEgress bills the response bytes that the relay writes toward the
// caller from now on to the organization of the relay grant. Exec,
// AttachExec and ReadFile call it after the request matched its grant.
func startRelayEgress(ctx context.Context, orgID string) {
	if meter, ok := ctx.Value(relayEgressKey{}).(*relayEgress); ok {
		meter.org.Store(&orgID)
	}
}

// agentWireBytesKey carries the byte counter of an agent relay connection
// response.
type agentWireBytesKey struct{}

// agentWireWriter counts the bytes written to an agent relay connection
// response, so the relay can measure the bytes of each WriteFile request
// frame on the wire.
type agentWireWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w *agentWireWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n.Add(int64(n))
	return n, err
}

func (w *agentWireWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *agentWireWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// agentWireBytes returns the byte counter of the agent relay connection of
// ctx, or nil without one.
func agentWireBytes(ctx context.Context) func() int64 {
	n, _ := ctx.Value(agentWireBytesKey{}).(*atomic.Int64)
	if n == nil {
		return nil
	}
	return n.Load
}
