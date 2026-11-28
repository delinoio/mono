// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/internal/telemetry"
)

const (
	// requestTimeout bounds each test request, including the body read.
	requestTimeout = 5 * time.Second
	// serveStopTimeout bounds the wait for Serve to return after cancel. It is
	// larger than the 5 s drain limit inside Serve so a slow runner still sees
	// Serve's own result instead of a test-side timeout.
	serveStopTimeout = 15 * time.Second
)

// startServer serves handler on a new loopback listener and registers a
// cleanup that cancels the server, waits for Serve to return and reports its
// error. t.Cleanup runs on t.Fatal and early exits too, so a failed request
// can no longer leave the server running. Register it after any cleanup that
// must outlive the server, because cleanups run in last-in-first-out order.
func startServer(t *testing.T, handler http.Handler) net.Listener {
	t.Helper()
	listener := listenLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- metrics.Serve(ctx, listener, handler, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v", err)
			}
		case <-time.After(serveStopTimeout):
			t.Errorf("Serve did not return within %v after cancel", serveStopTimeout)
		}
	})
	return listener
}

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func serveListener(t *testing.T, listener net.Listener, handler http.Handler) (context.CancelFunc, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = metrics.Serve(ctx, listener, handler, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		close(done)
	}()
	await := func() error {
		t.Helper()
		select {
		case <-done:
			return serveErr
		case <-time.After(serveStopTimeout):
			t.Fatalf("Serve did not return within %v", serveStopTimeout)
			return nil
		}
	}
	t.Cleanup(func() {
		cancel()
		await()
	})
	return cancel, await
}

func TestServeAndShutdown(t *testing.T) {
	// The SDK reads resource settings directly from the process environment.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	tel, err := telemetry.New(context.Background(), telemetry.Options{
		ServiceName: "accounts-api",
		Stdout:      io.Discard,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the server cleanup so telemetry shuts down after Serve.
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("telemetry shutdown: %v", err)
		}
	})
	m, err := metrics.New(tel.MeterProvider)
	if err != nil {
		t.Fatal(err)
	}
	m.Webhook("transaction.completed", "applied")
	m.TopUpAttempt("auto", "rejected")
	m.TopUpResult("manual", "completed")
	m.Email("low_balance", "sent")
	m.CreditGrant("subscription_credit")
	m.AutoTopUpDisabled()
	m.UsageReport("ownsand", "charged")
	m.StatusChange("blocked")
	m.LinkedOrganizations("suspended", 3)

	listener := startServer(t, tel.MetricsHandler(nil))
	client := &http.Client{Timeout: requestTimeout}

	resp, err := client.Get("http://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("/metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	for _, want := range []model.Sample{
		{Metric: model.Metric{model.MetricNameLabel: "accounts_paddle_webhooks_total", "event_type": "transaction.completed", "result": "applied"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_top_up_attempts_total", "kind": "auto", "outcome": "rejected"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_top_up_results_total", "kind": "manual", "result": "completed"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_billing_emails_total", "kind": "low_balance", "result": "sent"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_credit_grants_total", "source": "subscription_credit"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_auto_top_up_disabled_total"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_usage_reports_total", "result": "charged", "service": "ownsand"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_billing_status_changes_total", "status": "blocked"}, Value: 1},
		{Metric: model.Metric{model.MetricNameLabel: "accounts_linked_organizations", "status": "suspended"}, Value: 3},
	} {
		family := families[string(want.Metric[model.MetricNameLabel])]
		if family == nil {
			t.Errorf("/metrics lacks %s", want.Metric)
			continue
		}
		samples, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{}, family)
		if err != nil {
			t.Errorf("extract %s samples: %v", family.GetName(), err)
			continue
		}
		index := slices.IndexFunc(samples, func(sample *model.Sample) bool {
			return sample.Metric.Equal(want.Metric)
		})
		if index < 0 {
			t.Errorf("/metrics lacks %s", want.Metric)
		} else if got := samples[index].Value; got != want.Value {
			t.Errorf("%s = %v, want %v", want.Metric, got, want.Value)
		}
	}
	if len(families["go_goroutine_count"].GetMetric()) == 0 {
		t.Error("/metrics lacks go_goroutine_count")
	}
	resp, err = client.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz on the metrics listener = %d, want 404", resp.StatusCode)
	}
}

type closeObservedListener struct {
	net.Listener
	closed chan struct{}
}

func (l closeObservedListener) Close() error {
	err := l.Listener.Close()
	close(l.closed)
	return err
}

// shutdownAcceptListener can accept one scrape before waiting for Shutdown to
// close the listener. Shutdown sets its flag before Close releases Accept.
type shutdownAcceptListener struct {
	net.Listener
	acceptEntered chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	err           error
	acceptScrape  bool
}

func (l *shutdownAcceptListener) Accept() (net.Conn, error) {
	if l.acceptScrape {
		l.acceptScrape = false
		return l.Listener.Accept()
	}
	close(l.acceptEntered)
	<-l.closed
	return nil, l.err
}

func (l *shutdownAcceptListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.closed) })
	return err
}

var (
	errPermanentAccept = errors.New("metrics listener permanent accept error")
	errTemporaryAccept = &net.DNSError{Err: "metrics listener temporary accept error", IsTemporary: true}
)

func TestServeAcceptErrorDuringShutdown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		acceptErr error
		wantErr   error
	}{
		{name: "permanent failure", acceptErr: errPermanentAccept, wantErr: errPermanentAccept},
		{name: "listener closed", acceptErr: net.ErrClosed},
		{name: "wrapped listener closure", acceptErr: &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}},
		{name: "HTTP server closed", acceptErr: http.ErrServerClosed},
		{name: "temporary failure", acceptErr: errTemporaryAccept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := &shutdownAcceptListener{
				Listener:      listenLoopback(t),
				acceptEntered: make(chan struct{}),
				closed:        make(chan struct{}),
				err:           tc.acceptErr,
			}
			cancel, await := serveListener(t, listener, http.NotFoundHandler())
			select {
			case <-listener.acceptEntered:
			case <-time.After(requestTimeout):
				t.Fatal("metrics listener did not enter Accept")
			}
			cancel()
			err := await()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Serve error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && strings.Count(err.Error(), tc.wantErr.Error()) != 1 {
				t.Fatalf("Serve error = %q, want one occurrence of %q", err, tc.wantErr)
			}
		})
	}
}

type acceptFuncListener struct {
	net.Listener
	accept func() (net.Conn, error)
}

func (l acceptFuncListener) Accept() (net.Conn, error) { return l.accept() }

func TestServeReturnsPermanentAcceptErrorBeforeShutdown(t *testing.T) {
	listener := acceptFuncListener{
		Listener: listenLoopback(t),
		accept:   func() (net.Conn, error) { return nil, errPermanentAccept },
	}
	_, await := serveListener(t, listener, http.NotFoundHandler())
	if err := await(); err != errPermanentAccept {
		t.Fatalf("Serve error = %v, want the original error %v", err, errPermanentAccept)
	}
}

func TestServeRetriesTemporaryAcceptError(t *testing.T) {
	bound := listenLoopback(t)
	failed := false
	listener := acceptFuncListener{
		Listener: bound,
		accept: func() (net.Conn, error) {
			if !failed {
				failed = true
				return nil, errTemporaryAccept
			}
			return bound.Accept()
		},
	}
	cancel, await := serveListener(t, listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Get("http://" + bound.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	cancel()
	if err := await(); err != nil {
		t.Fatalf("Serve error = %v, want nil after a clean shutdown", err)
	}
}

func TestServeDrainsActiveScrape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expires   bool
		acceptErr error
	}{
		{name: "deadline expires", expires: true},
		{name: "deadline expires with permanent accept failure", expires: true, acceptErr: errPermanentAccept},
		{name: "scrape finishes during drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			listener := listenLoopback(t)
			draining := make(chan struct{})
			var serverListener net.Listener = closeObservedListener{listener, draining}
			if tc.acceptErr != nil {
				serverListener = &shutdownAcceptListener{
					Listener:      serverListener,
					acceptEntered: make(chan struct{}),
					closed:        make(chan struct{}),
					err:           tc.acceptErr,
					acceptScrape:  true,
				}
			}
			started := make(chan struct{})
			release := make(chan struct{})
			releaseHandler := sync.OnceFunc(func() { close(release) })
			const metricsBody = "probe_metric 1\n"
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				_, _ = io.WriteString(w, metricsBody)
			})
			cancel, await := serveListener(t, serverListener, handler)
			t.Cleanup(releaseHandler)

			conn, err := net.DialTimeout("tcp", listener.Addr().String(), requestTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(serveStopTimeout)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(requestTimeout):
				t.Fatal("scrape did not enter the handler")
			}
			if gated, ok := serverListener.(*shutdownAcceptListener); ok {
				select {
				case <-gated.acceptEntered:
				case <-time.After(requestTimeout):
					t.Fatal("metrics listener did not enter its second Accept")
				}
			}

			cancel()
			select {
			case <-draining:
			case <-time.After(requestTimeout):
				t.Fatal("metrics listener did not close at the start of the drain")
			}
			if !tc.expires {
				releaseHandler()
			}
			serveErr := await()

			if tc.expires {
				if !errors.Is(serveErr, context.DeadlineExceeded) {
					t.Errorf("metrics Serve error = %v, want the drain deadline error", serveErr)
				}
				if tc.acceptErr != nil && !errors.Is(serveErr, tc.acceptErr) {
					t.Errorf("metrics Serve error = %v, want the permanent Accept error too", serveErr)
				}
				if err := conn.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
					t.Fatal(err)
				}
				var byteRead [1]byte
				if n, err := conn.Read(byteRead[:]); n != 0 || (!errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET)) {
					t.Fatalf("scrape read after Serve returned = (%d, %v), want EOF or connection reset", n, err)
				}
				return
			}

			if serveErr != nil {
				t.Errorf("metrics Serve error = %v, want nil", serveErr)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || string(body) != metricsBody {
				t.Fatalf("scrape response = (%d, %q, %v), want (200, %q, nil)", resp.StatusCode, body, err, metricsBody)
			}
		})
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// TestServerStopsAfterFailedRequest proves that a request failure that ends
// the test early, as t.Fatal does, still shuts the metrics server down.
func TestServerStopsAfterFailedRequest(t *testing.T) {
	errInjected := errors.New("injected transport failure")
	var listener net.Listener
	// The subtest leaves through the same runtime.Goexit path as t.Fatal, but
	// t.Skip keeps the exit expected so this test can inspect the cleanup result.
	t.Run("early exit", func(t *testing.T) {
		listener = startServer(t, http.NotFoundHandler())
		client := &http.Client{Timeout: requestTimeout, Transport: failingTransport{err: errInjected}}
		resp, err := client.Get("http://" + listener.Addr().String() + "/metrics")
		if err == nil {
			resp.Body.Close()
		}
		if !errors.Is(err, errInjected) {
			t.Fatalf("request error does not wrap the injected failure: %T", err)
		}
		t.Skip("simulated early exit after a failed request")
	})
	if listener == nil {
		t.Fatal("subtest did not start the server")
	}
	// Serve closes the listener once it has shut down. The deadline turns a
	// server that is still running into a failure instead of a hang.
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(serveStopTimeout))
	}
	conn, err := listener.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after the subtest cleanup")
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener Accept failed for a reason other than closure: %T", err)
	}
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var m *metrics.Metrics
	m.Webhook("a", "b")
	m.TopUpAttempt("a", "b")
	m.TopUpResult("a", "b")
	m.Email("a", "b")
	m.CreditGrant("a")
	m.AutoTopUpDisabled()
	m.UsageReport("a", "b")
	m.StatusChange("a")
	m.LinkedOrganizations("a", 1)
}
