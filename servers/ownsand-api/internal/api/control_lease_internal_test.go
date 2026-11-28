// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

func TestControlAdmittedDeliveryCannotAffectSuccessor(t *testing.T) {
	t.Parallel()
	for _, kind := range []events.Kind{events.KindDeviceRemoved, events.KindAPIKeyRevoked} {
		name := "device removal"
		if kind == events.KindAPIKeyRevoked {
			name = "API key revocation"
		}
		t.Run(name, func(t *testing.T) {
			h := newCursorHarness(t, 2)
			s := h.c.S
			s.Now = time.Now
			p := NewDataPlane(s, relay.Limits{}, RelayTiming{})
			h.all[0] = events.Event{ID: 1, Kind: kind, OrganizationID: "org", DeviceID: "device", APIKeyID: "key"}
			h.acquire(0, nil)
			pr := auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: "key"}
			seed := func(id string, gen uint64) {
				p.sessions[id] = &session{id: id, deviceID: "device", gen: gen, principal: pr, expireAt: s.now().Add(time.Hour)}
				if _, _, err := p.Grants.Reserve(grants.Request{
					AuthorizationID: id, DeviceID: "device", ChannelGen: gen, Principal: pr,
				}, func() string { return id }); err != nil {
					t.Fatal(err)
				}
			}

			old := s.Hub.Register(t.Context(), "device", "org")
			seed("old", old.Generation)
			calls := 0
			h.c.handleEvent = func(ctx context.Context, e events.Event, channels channelGenerations) error {
				calls++
				if calls != 1 {
					t.Fatal("a superseded batch admitted another delivery")
				}
				h.c.LeaseLost()
				h.acquire(2, nil)
				successor := s.Hub.Register(t.Context(), "device", "org")
				seed("successor", successor.Generation)
				p.revocations["successor"] = revocation{deviceID: "device", gen: successor.Generation}

				if err := h.c.handle(ctx, e, channels); err != nil {
					t.Fatal(err)
				}
				if p.sessions["old"] != nil || p.Grants.Get("old") != nil {
					t.Error("the delivery did not remove the old session and grant")
				}
				if p.sessions["successor"] == nil || p.Grants.Get("successor") == nil {
					t.Error("the delivery removed a successor session or grant")
				}
				if _, exists := p.revocations["successor"]; !exists {
					t.Error("the delivery removed a successor revocation")
				}
				if successor.Value(removedDeviceKey{}) != nil || len(successor.Outbound()) != 0 {
					t.Error("the delivery affected the successor channel")
				}
				return nil
			}

			h.c.processEvents(t.Context())
			if calls != 1 || h.cursor() != 2 {
				t.Fatalf("deliveries = %d, cursor = %d, want 1 and 2", calls, h.cursor())
			}
		})
	}
}

func TestControlRefusesDeliveryWhenOwnershipEndsBeforeLossCallback(t *testing.T) {
	t.Parallel()
	h := newCursorHarness(t, 2)
	owner := &ownershipLease{held: true}
	h.c.S.Lease = owner
	h.acquire(0, nil)
	h.onRead = func(int64) { owner.held = false }

	h.c.processEvents(t.Context())
	if len(h.delivered) != 0 || h.cursor() != 0 {
		t.Fatalf("deliveries = %v, cursor = %d after ownership ended", h.delivered, h.cursor())
	}
}

func TestControlLeaseLossWhileDeliveryWaitsForQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newCursorHarness(t, 2)
		s := h.c.S
		s.Now = time.Now
		s.Hub.QueueFullWait = time.Hour
		h.acquire(0, nil)
		old := s.Hub.Register(t.Context(), "device", "org")
		old.SetReconciled()
		for range cap(old.Outbound()) {
			if err := old.Send(t.Context(), &ownsandv1.AgentEnvelope{}); err != nil {
				t.Fatal(err)
			}
		}
		h.all[0] = events.Event{ID: 1, Kind: events.KindSnapshotDeleted, DeviceID: "device", SnapshotID: "old-snapshot"}
		h.c.handleEvent = h.c.handle
		pollDone := make(chan struct{})
		go func() {
			defer close(pollDone)
			h.c.processEvents(t.Context())
		}()
		synctest.Wait()
		if s.PendingCommands() != 1 {
			t.Fatal("delivery did not reach the full queue")
		}

		h.c.LeaseLost()
		h.acquire(2, nil)
		successor := s.Hub.Register(t.Context(), "device", "org")
		synctest.Wait()
		select {
		case <-pollDone:
		default:
			t.Fatal("superseded delivery still waits for queue capacity")
		}
		if s.PendingCommands() != 0 || len(successor.Outbound()) != 0 || h.cursor() != 2 {
			t.Fatal("superseded delivery left a command or changed the new baseline")
		}
	})
}
