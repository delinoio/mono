// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

type countingUsage struct{ added chan int64 }

func (c countingUsage) Add(_ string, _ ownsandv1.BillingMeter, _ time.Time, q int64) { c.added <- q }

// TestDrainUsageWaitsForRecordingRequest checks that DrainUsage returns only
// after a billed unary RPC recorded its usage and ended, and that an RPC that
// never ends makes it return ErrUsageDrainIncomplete when its context ends.
func TestDrainUsageWaitsForRecordingRequest(t *testing.T) {
	s := &Service{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Now: time.Now}
	usage := countingUsage{added: make(chan int64, 1)}
	s.Usage = usage
	m := &meterInterceptor{s: s}
	const procedure = ownsandv1connect.SandboxServiceListSandboxesProcedure

	release := make(chan struct{})
	entered := make(chan struct{})
	finished := make(chan struct{})
	handler := m.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		close(entered)
		<-release
		// Record the usage the way the handler of an eligible request does.
		s.recordUsage("0190f3a4-0000-7000-8000-000000000001", ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC, 1)
		return nil, nil
	})
	go func() {
		defer close(finished)
		_, _ = handler(context.Background(), fakeRequest{procedure: procedure})
	}()
	<-entered

	// Not drained while the request is running: a short context ends first.
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := s.DrainUsage(short)
	cancel()
	if !errors.Is(err, ErrUsageDrainIncomplete) {
		t.Fatalf("DrainUsage with a running request = %v, want ErrUsageDrainIncomplete", err)
	}

	drained := make(chan error, 1)
	go func() { drained <- s.DrainUsage(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("DrainUsage returned %v before the request ended", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("DrainUsage = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainUsage did not return after the request ended")
	}
	<-finished
	select {
	case q := <-usage.added:
		if q != 1 {
			t.Errorf("recorded quantity = %d", q)
		}
	default:
		t.Error("the request recorded no usage before DrainUsage returned")
	}
	if err := s.DrainUsage(context.Background()); err != nil {
		t.Errorf("DrainUsage with no request = %v", err)
	}
}

// TestDrainUsageIgnoresUnbilledRPC checks that an RPC that records no usage,
// such as the agent channel, does not hold the drain.
func TestDrainUsageIgnoresUnbilledRPC(t *testing.T) {
	s := &Service{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Now: time.Now, Usage: countingUsage{added: make(chan int64, 1)}}
	m := &meterInterceptor{s: s}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	handler := m.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		close(entered)
		<-release
		return nil
	})
	go func() {
		defer close(done)
		_ = handler(context.Background(), fakeStreamConn{procedure: ownsandv1connect.AgentServiceChannelProcedure})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.DrainUsage(ctx); err != nil {
		t.Errorf("DrainUsage with an unbilled stream = %v", err)
	}
	close(release)
	<-done
}

// TestUsageGateWithoutMetering checks that a service without metering never
// blocks the drain.
func TestUsageGateWithoutMetering(t *testing.T) {
	s := &Service{}
	leave := s.enterBilledRPC(ownsandv1connect.SandboxServiceListSandboxesProcedure)
	defer leave()
	if err := s.DrainUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type fakeRequest struct {
	connect.AnyRequest
	procedure string
}

func (r fakeRequest) Spec() connect.Spec { return connect.Spec{Procedure: r.procedure} }

type fakeStreamConn struct {
	connect.StreamingHandlerConn
	procedure string
}

func (c fakeStreamConn) Spec() connect.Spec { return connect.Spec{Procedure: c.procedure} }
