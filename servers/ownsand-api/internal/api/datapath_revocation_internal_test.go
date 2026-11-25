// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

func revocationTestPlane(now func() time.Time) *DataPlane {
	return NewDataPlane(&Service{Hub: hub.New(), Now: now, Logger: slog.New(slog.DiscardHandler)}, relay.Limits{}, DefaultRelayTiming())
}

func TestDeferredRevocationInitialEnqueueInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := revocationTestPlane(time.Now)
		ctx := t.Context()
		conn := p.S.Hub.Register(ctx, "device", "org")
		defer p.S.Hub.Unregister(conn)
		for range cap(conn.Outbound()) {
			if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{}); err != nil {
				t.Fatal(err)
			}
		}
		r := &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_SessionId{SessionId: "session"}}
		go func() {
			first, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			p.sendRevocation(first, conn.DeviceID, conn.Generation, revocationTargetSession, time.Now().Add(time.Hour), r)
		}()
		synctest.Wait()
		id := r.GetRevocationId()
		checkDelivery := func(want revocationDelivery) {
			t.Helper()
			p.mu.Lock()
			record, ok := p.revocations[id]
			p.mu.Unlock()
			if !ok || record.delivery != want {
				t.Fatalf("pending revocation %s: exists = %t, delivery = %v, want delivery %v", id, ok, record.delivery, want)
			}
		}
		p.SweepExpired(ctx)
		checkDelivery(revocationSending)
		time.Sleep(time.Second)
		synctest.Wait()
		checkDelivery(revocationDeferred)
		<-conn.Outbound()
		p.SweepExpired(ctx)
		p.SweepExpired(ctx)
		var queued []string
		for len(conn.Outbound()) > 0 {
			if r := (<-conn.Outbound()).GetSessionRevocation(); r != nil {
				queued = append(queued, r.GetRevocationId())
			}
		}
		if len(queued) != 1 || queued[0] != id {
			t.Fatalf("queued revocations = %v, want only %s", queued, id)
		}
	})
}

func TestDeferredRevocationTerminalCleanup(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, p *DataPlane, conn *hub.Conn, now *time.Time, expireAt time.Time)
	}{
		{"expiry", func(_ *testing.T, _ *DataPlane, _ *hub.Conn, now *time.Time, expireAt time.Time) { *now = expireAt }},
		{"unregister", func(_ *testing.T, p *DataPlane, conn *hub.Conn, _ *time.Time, _ time.Time) { p.S.Hub.Unregister(conn) }},
		{"replacement", func(t *testing.T, p *DataPlane, conn *hub.Conn, _ *time.Time, _ time.Time) {
			replacement := p.S.Hub.Register(t.Context(), conn.DeviceID, conn.OrganizationID)
			p.SweepExpired(t.Context())
			if len(replacement.Outbound()) != 0 {
				t.Error("the replacement channel got a revocation of the old generation")
			}
			p.S.Hub.Unregister(replacement)
		}},
		{"device removal", func(_ *testing.T, p *DataPlane, conn *hub.Conn, _ *time.Time, _ time.Time) {
			p.HandleEvent(context.Background(), events.Event{Kind: events.KindDeviceRemoved, DeviceID: conn.DeviceID})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			expireAt := now.Add(time.Hour)
			p := revocationTestPlane(func() time.Time { return now })
			conn := p.S.Hub.Register(t.Context(), "device", "org")
			defer p.S.Hub.Unregister(conn)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r := &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: "grant"}}
			p.sendRevocation(ctx, conn.DeviceID, conn.Generation, revocationTargetRelayGrant, expireAt, r)
			if n := p.PendingRevocations(); n != 1 {
				t.Fatalf("pending revocations after the initial enqueue = %d, want 1", n)
			}
			c.end(t, p, conn, &now, expireAt)
			p.SweepExpired(t.Context())
			p.SweepExpired(t.Context())
			if n := p.PendingRevocations(); n != 0 {
				t.Fatalf("pending revocations after terminal cleanup = %d, want 0", n)
			}
			if len(conn.Outbound()) != 0 {
				t.Fatal("a terminal revocation was queued")
			}
		})
	}
}
