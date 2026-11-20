// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"log/slog"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// RunLeaseWork runs the lease holder's loops until ctx ends: the control
// event consumer and the offline sweeper. Each loop does nothing while this
// instance does not hold the lease.
func (h *Hub) RunLeaseWork(ctx context.Context) {
	go h.sweep(ctx)
	h.consumeEvents(ctx)
}

// sweep marks online devices offline when no heartbeat arrived for 45
// seconds and they have no current channel here. After a lease handover this
// applies the 45-second rule to devices that do not reconnect.
func (h *Hub) sweep(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if h.Lease == nil || !h.Lease.Held() {
			continue
		}
		ids, err := h.Store.OnlineDevicesWithoutHeartbeat(ctx, time.Now().Add(-HeartbeatTimeout))
		if err != nil {
			h.Logger.Warn("offline_sweep_failed", slog.Any("error", err))
			continue
		}
		for _, id := range ids {
			if _, ok := h.Current(id); ok {
				continue
			}
			if err := h.Store.SetDeviceOffline(ctx, id); err == nil {
				h.Logger.Info("device_offline", slog.String("device_id", id), slog.String("reason", "no heartbeat"))
				h.notifyChange()
			}
		}
	}
}

// consumeEvents applies the control events that any instance writes. The
// lease holder starts from the newest event when it takes the lease, so it
// does not replay events whose effects the previous holder delivered or that
// the reconnection state reports cover.
func (h *Hub) consumeEvents(ctx context.Context) {
	var last int64
	t := time.NewTicker(EventInterval)
	defer t.Stop()
	for {
		if h.Lease == nil || !h.Lease.Held() {
			if h.ready.Swap(false) {
				h.CloseAll("lease lost")
			}
		} else if !h.ready.Load() {
			id, err := h.Store.LatestControlEventID(ctx)
			if err == nil {
				last = id
				h.ready.Store(true)
				h.Logger.Info("agent_channels_accepted", slog.Int64("control_event_id", last))
			}
		} else {
			events, err := h.Store.ControlEventsAfter(ctx, last, 500)
			if err != nil {
				h.Logger.Warn("control_events_failed", slog.Any("error", err))
			}
			for _, e := range events {
				last = e.ID
				h.applyEvent(ctx, e)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Hub) applyEvent(ctx context.Context, e store.ControlEvent) {
	switch e.Kind {
	case store.ControlEventDeviceLimitsChanged:
		if c, ok := h.Current(e.DeviceID); ok {
			h.SendConfiguration(ctx, c)
		}
	case store.ControlEventAgentPolicyChanged:
		for _, c := range h.orgChannels(e.OrganizationID) {
			h.SendConfiguration(ctx, c)
		}
	case store.ControlEventSandboxDeleteRequested:
		if h.IsLive(e.DeviceID) {
			h.SendDelete(e.DeviceID, e.SandboxID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER)
		}
	case store.ControlEventDeviceRemoved:
		if c, ok := h.Current(e.DeviceID); ok {
			h.cleanUpAndClose(c)
		}
	case store.ControlEventOrganizationDeleted:
		for _, c := range h.orgChannels(e.OrganizationID) {
			h.cleanUpAndClose(c)
		}
	}
	if h.Extension != nil {
		h.Extension.ControlEvent(e)
	}
}

func (h *Hub) orgChannels(organizationID string) []*Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Conn
	for _, c := range h.channels {
		if c.OrganizationID == organizationID {
			out = append(out, c)
		}
	}
	return out
}
