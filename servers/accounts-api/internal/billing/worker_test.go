// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/internal/telemetry"
)

func TestRunWorkerSkipsPreCanceledStartup(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	service := New(Config{Logger: logger})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	service.RunWorker(ctx, time.Second)

	if logs.Len() != 0 {
		t.Errorf("worker logs after canceled startup = %q, want no logs", logs.String())
	}
}

func TestRunOnceRefreshesMetricsWithDueNotifications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scrape, org := workerMetricsFixture(t, f)
	f.s.RunOnce(t.Context())
	wantLinkedOrganizationGauge(t, scrape, StatusAllowed)

	acct := f.account()
	var notices []string
	for range 3 {
		notice, _ := f.queueNotice(acct)
		notices = append(notices, notice)
		f.clock.Advance(time.Second)
	}
	f.s.notificationDeliveryBudget = 500 * time.Millisecond
	for i, status := range []LinkStatus{StatusBlocked, StatusSuspended} {
		// Stop each pass on its second send. The next due notice should
		// still be in the queue when the pass refreshes the gauge.
		sends := 0
		f.s.email = &hookSender{next: f.email, before: func(ctx context.Context, _ email.Message) error {
			sends++
			if sends == 2 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}}
		setWorkerMetricStatus(t, f, org, status)
		passCtx, cancel := context.WithTimeout(t.Context(), workerRunTimeout)
		f.s.RunOnce(passCtx)
		cancel()

		wantLinkedOrganizationGauge(t, scrape, status)
		if st := f.noticeState(notices[i]); st.Sent || st.Attempts != 0 || len(st.Recipients) != 1 ||
			!st.Next.Equal(f.clock.Now().Add(notificationDeferDelay)) {
			t.Fatalf("pass %d interrupted notice: %+v", i+1, st)
		}
		if st := f.noticeState(notices[i+1]); st.Sent || st.Next.After(f.clock.Now()) || len(st.Recipients) != 0 {
			t.Fatalf("pass %d remaining due notice: %+v", i+1, st)
		}
	}

	f.s.email = f.email
	f.clock.Advance(notificationDeferDelay)
	f.s.RunOnce(t.Context())
	for _, notice := range notices {
		if st := f.noticeState(notice); !st.Sent || st.Attempts != 1 || len(st.Recipients) != 2 {
			t.Fatalf("notice after retry: %+v", st)
		}
	}
	var keys []string
	for _, msg := range f.email.Messages() {
		keys = append(keys, msg.IdempotencyKey)
	}
	slices.Sort(keys)
	if len(keys) != 6 || len(slices.Compact(keys)) != 6 {
		t.Fatalf("successful delivery keys = %v, want one per recipient and notice", keys)
	}
}

func TestRunOnceCancellationKeepsNotificationProgressAndStopsLaterTasks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scrape, org := workerMetricsFixture(t, f)
	f.s.RunOnce(t.Context())
	wantLinkedOrganizationGauge(t, scrape, StatusAllowed)
	setWorkerMetricStatus(t, f, org, StatusBlocked)

	notice, _ := f.queueNotice(f.account())
	passCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sends := 0
	f.s.email = &hookSender{next: f.email, before: func(ctx context.Context, _ email.Message) error {
		sends++
		if sends == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	}}
	f.s.RunOnce(passCtx)
	if st := f.noticeState(notice); st.Sent || st.Attempts != 0 || len(st.Recipients) != 1 ||
		!st.Next.Equal(f.clock.Now().Add(notificationDeferDelay)) {
		t.Fatalf("canceled notice: %+v", st)
	}
	wantLinkedOrganizationGauge(t, scrape, StatusAllowed)
}

func workerMetricsFixture(t *testing.T, f *fixture) (http.Handler, string) {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Options{
		ServiceName: "accounts-api",
		Stdout:      io.Discard,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.s.metrics, err = metrics.New(tel.MeterProvider)
	if err != nil {
		t.Fatal(err)
	}
	acct := f.account()
	f.activate(acct)
	f.credit(acct, 1000)
	return tel.MetricsHandler(nil), f.link(acct, "Worker metrics")
}

func setWorkerMetricStatus(t *testing.T, f *fixture, org string, status LinkStatus) {
	t.Helper()
	now := f.s.timestamp()
	_, err := f.db.Pool.Exec(t.Context(), `UPDATE organization_links
		SET billing_status = $2, status_reason = $3, blocked_since = $4, suspend_time = $5
		WHERE organization_id = $1`, org, status, ReasonNonPositiveBalance, now, now.Add(SuspendAfter))
	if err != nil {
		t.Fatal(err)
	}
}

func wantLinkedOrganizationGauge(t *testing.T, scrape http.Handler, active LinkStatus) {
	t.Helper()
	response := httptest.NewRecorder()
	scrape.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := map[LinkStatus]int{StatusAllowed: 0, StatusBlocked: 0, StatusSuspended: 0}
	want[active] = 1
	body := response.Body.String()
	for status, count := range want {
		line := fmt.Sprintf("accounts_linked_organizations{status=%q} %d\n", status, count)
		if !strings.Contains(body, line) {
			t.Errorf("metrics status %d, missing %q in %s", response.Code, line, body)
		}
	}
}
