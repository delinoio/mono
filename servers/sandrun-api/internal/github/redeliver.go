// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/webhook"
)

// runRedelivery is the lease component that requests redeliveries of
// failed deliveries of the app webhook: GitHub does not redeliver by itself.
// It runs when the instance gets the lease and then every
// RedeliveryInterval.
func (l *Layer) runRedelivery(ctx context.Context) error {
	t := time.NewTicker(l.timing.RedeliveryInterval)
	defer t.Stop()
	for {
		l.RedeliverOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// RedeliverOnce lists the failed deliveries of the last RedeliveryWindow
// (24 hours; GitHub redelivers only deliveries of the past 3 days), groups
// them by GUID (a redelivery keeps the GUID of its delivery) and requests a
// redelivery of the newest delivery of each GUID that no delivery delivered:
// a group with a successful delivery, or whose GUID SandRun recorded (SandRun
// records a delivery ID before it answers 2xx), is skipped. It returns the
// number of requested redeliveries.
func (l *Layer) RedeliverOnce(ctx context.Context) int {
	if l.gh == nil {
		return 0
	}
	deliveries, err := l.gh.FailedDeliveries(ctx, l.svc.NowUTC().Add(-l.timing.RedeliveryWindow))
	if err != nil {
		if ctx.Err() == nil {
			l.svc.Logger.WarnContext(ctx, "github_redelivery_list_failed", slog.Any("error", err))
		}
		return 0
	}
	type group struct {
		newest  int64
		newestT time.Time
		ok      bool
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range deliveries {
		g := groups[d.GUID]
		if g == nil {
			g = &group{}
			groups[d.GUID] = g
			order = append(order, d.GUID)
		}
		if d.Succeeded() {
			g.ok = true
		}
		if g.newest == 0 || d.DeliveredAt.After(g.newestT) || (d.DeliveredAt.Equal(g.newestT) && d.ID > g.newest) {
			g.newest, g.newestT = d.ID, d.DeliveredAt
		}
	}
	n := 0
	for _, guid := range order {
		g := groups[guid]
		if g.ok || guid == "" {
			continue
		}
		seen, err := webhook.Seen(ctx, l.svc.DB.Pool, webhook.SenderGitHub, guid)
		if err != nil {
			l.svc.Logger.WarnContext(ctx, "github_redelivery_check_failed", slog.Any("error", err))
			return n
		}
		if seen {
			continue
		}
		if err := l.gh.Redeliver(ctx, g.newest); err != nil {
			l.svc.Logger.WarnContext(ctx, "github_redelivery_failed", slog.String("delivery_id", guid), slog.Int64("github_delivery_id", g.newest), slog.Any("error", err))
			continue
		}
		n++
		l.svc.Logger.InfoContext(ctx, "github_redelivery_requested", slog.String("delivery_id", guid), slog.Int64("github_delivery_id", g.newest))
	}
	if n > 0 || len(groups) > 0 {
		l.svc.Logger.InfoContext(ctx, "github_redelivery_done", slog.Int("failed_deliveries", len(deliveries)), slog.Int("redeliveries", n))
	}
	return n
}
