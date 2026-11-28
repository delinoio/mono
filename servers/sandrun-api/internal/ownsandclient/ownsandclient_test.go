// SPDX-License-Identifier: FSL-1.1-ALv2

package ownsandclient_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/metrics"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
)

func TestInstrumentedCountsLogsAndReportsUnauthenticated(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := metrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	fake := ownsandfake.New()
	key := "osk_" + strings.Repeat("a", 43)
	fake.AddOrg("org-1", key)
	inner, _ := fake.New("org-1", key)
	unauth := 0
	c := &ownsandclient.Instrumented{Inner: inner, Metrics: m, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), OrganizationID: "sandrun-org", OnUnauthenticated: func() { unauth++ }}
	ctx := context.Background()
	if _, err := c.ListDevices(ctx); err != nil {
		t.Fatal(err)
	}
	fake.RevokeKey("org-1")
	if _, err := c.ListDevices(ctx); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
	if unauth != 1 {
		t.Fatalf("OnUnauthenticated ran %d times", unauth)
	}
	if !strings.Contains(logs.String(), `"rpc":"ListDevices","code":"ok"`) || !strings.Contains(logs.String(), `"code":"unauthenticated"`) ||
		strings.Contains(logs.String(), key) {
		t.Fatalf("logs: %s", logs.String())
	}
	got := ownsandCallCounts(t, reader)
	if got["ListDevices/ok"] != 1 || got["ListDevices/unauthenticated"] != 1 {
		t.Fatalf("metrics %v", got)
	}
	if ownsandclient.Code(nil) != "ok" || ownsandclient.Code(context.Canceled) != "canceled" {
		t.Fatal("code strings")
	}
}

func ownsandCallCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			if metric.Name != "sandrun_ownsand_calls" {
				continue
			}
			for _, dp := range metric.Data.(metricdata.Sum[int64]).DataPoints {
				rpc, hasRPC := dp.Attributes.Value("rpc")
				code, hasCode := dp.Attributes.Value("code")
				if !hasRPC || !hasCode || dp.Attributes.Len() != 2 {
					t.Fatalf("unexpected OwnSand metric attributes: %v", dp.Attributes.ToSlice())
				}
				got[rpc.AsString()+"/"+code.AsString()] = dp.Value
			}
		}
	}
	return got
}

func TestInstrumentedTemplateGuestOSCountsRPCResults(t *testing.T) {
	const (
		orgID      = "ownsand-org"
		templateID = "target-template"
		snapshotID = "source-snapshot"
		nextToken  = "next-page"
		linux      = ownsandv1.GuestOs_GUEST_OS_LINUX
	)
	tests := []struct {
		name          string
		paginate      bool
		hasTemplate   bool
		listError     connect.Code
		snapshotError connect.Code
		wantGuest     ownsandv1.GuestOs
		wantError     connect.Code
		invalidations int
		counts        map[string]int64
	}{
		{
			name: "match on second page", paginate: true, hasTemplate: true, wantGuest: linux,
			counts: map[string]int64{"ListTemplates/ok": 2, "GetSnapshot/ok": 1},
		},
		{
			name: "snapshot unavailable", hasTemplate: true,
			snapshotError: connect.CodeUnavailable, wantError: connect.CodeUnavailable,
			counts: map[string]int64{"ListTemplates/ok": 1, "GetSnapshot/unavailable": 1},
		},
		{
			name: "local miss", paginate: true, wantError: connect.CodeNotFound,
			counts: map[string]int64{"ListTemplates/ok": 2},
		},
		{
			name: "key rejected by list", listError: connect.CodeUnauthenticated,
			wantError: connect.CodeUnauthenticated, invalidations: 1,
			counts: map[string]int64{"ListTemplates/unauthenticated": 1},
		},
		{
			name: "second page unavailable", paginate: true, listError: connect.CodeUnavailable,
			wantError: connect.CodeUnavailable,
			counts:    map[string]int64{"ListTemplates/ok": 1, "ListTemplates/unavailable": 1},
		},
		{
			name: "key rejected by snapshot", hasTemplate: true, snapshotError: connect.CodeUnauthenticated,
			wantError: connect.CodeUnauthenticated, invalidations: 1,
			counts: map[string]int64{"ListTemplates/ok": 1, "GetSnapshot/unauthenticated": 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var snapshots atomic.Int32
			mux := http.NewServeMux()
			listProcedure := ownsandv1connect.SnapshotServiceListTemplatesProcedure
			mux.Handle(listProcedure, connect.NewUnaryHandler(listProcedure,
				func(_ context.Context, req *connect.Request[ownsandv1.ListTemplatesRequest]) (*connect.Response[ownsandv1.ListTemplatesResponse], error) {
					token := req.Msg.GetPageToken()
					if test.paginate && token == "" {
						return connect.NewResponse(&ownsandv1.ListTemplatesResponse{NextPageToken: nextToken}), nil
					}
					if (test.paginate && token != nextToken) || (!test.paginate && token != "") {
						t.Errorf("unexpected page token %q", token)
					}
					if test.listError != 0 {
						return nil, connect.NewError(test.listError, errors.New("list failed"))
					}
					res := &ownsandv1.ListTemplatesResponse{}
					if test.hasTemplate {
						res.Templates = []*ownsandv1.Template{{TemplateId: templateID, SnapshotId: snapshotID}}
					}
					return connect.NewResponse(res), nil
				}))
			getProcedure := ownsandv1connect.SnapshotServiceGetSnapshotProcedure
			mux.Handle(getProcedure, connect.NewUnaryHandler(getProcedure,
				func(_ context.Context, req *connect.Request[ownsandv1.GetSnapshotRequest]) (*connect.Response[ownsandv1.GetSnapshotResponse], error) {
					snapshots.Add(1)
					if req.Msg.GetSnapshotId() != snapshotID {
						t.Errorf("snapshot ID = %q, want %q", req.Msg.GetSnapshotId(), snapshotID)
					}
					if test.snapshotError != 0 {
						return nil, connect.NewError(test.snapshotError, errors.New("snapshot failed"))
					}
					return connect.NewResponse(&ownsandv1.GetSnapshotResponse{Snapshot: &ownsandv1.Snapshot{GuestOs: linux}}), nil
				}))
			server := httptest.NewUnstartedServer(mux)
			server.Config.Protocols = new(http.Protocols)
			server.Config.Protocols.SetUnencryptedHTTP2(true)
			server.Start()
			t.Cleanup(server.Close)

			key := "osk_" + strings.Repeat("a", 43)
			inner, err := (ownsandclient.SDKFactory{BaseURL: server.URL}).New(orgID, key)
			if err != nil {
				t.Fatal(err)
			}
			reader := sdkmetric.NewManualReader()
			provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			m, err := metrics.New(provider)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			invalidations := 0
			client := &ownsandclient.Instrumented{
				Inner: inner, Metrics: m, Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
				OrganizationID: "sandrun-org", OnUnauthenticated: func() { invalidations++ },
			}
			guest, err := client.TemplateGuestOS(t.Context(), templateID)
			if guest != test.wantGuest || (err == nil) != (test.wantError == 0) ||
				(err != nil && connect.CodeOf(err) != test.wantError) {
				t.Fatalf("lookup = (%s, %v), want guest %s and error code %d", guest, err, test.wantGuest, test.wantError)
			}
			if !test.hasTemplate && snapshots.Load() != 0 {
				t.Fatalf("unexpected snapshot requests: %d", snapshots.Load())
			}
			if invalidations != test.invalidations {
				t.Fatalf("invalidations = %d, want %d", invalidations, test.invalidations)
			}
			if got := ownsandCallCounts(t, reader); !maps.Equal(got, test.counts) {
				t.Fatalf("metrics = %v, want %v", got, test.counts)
			}
			log := logs.String()
			if strings.Count(log, "\n") != 1 || !strings.Contains(log, `"msg":"ownsand_call"`) ||
				!strings.Contains(log, `"rpc":"ListTemplates","code":"`+ownsandclient.Code(err)+`"`) ||
				!strings.Contains(log, `"ownsand_template_id":"`+templateID+`"`) || strings.Contains(log, key) {
				t.Fatalf("lookup log: %s", log)
			}
		})
	}
}

func TestSDKDeleteSandboxDeadline(t *testing.T) {
	client := newDeleteSandboxFixture(t, http.NotFoundHandler())
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	if _, err := client.DeleteSandbox(ctx, "sandbox-1"); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("DeleteSandbox error = %v, want deadline_exceeded", err)
	}
}

func TestSDKDeleteSandboxCancellation(t *testing.T) {
	const wait = 5 * time.Second
	testCtx := t.Context()
	arrived := make(chan string)
	canceled := make(chan struct{})
	client := newDeleteSandboxFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- r.URL.Path:
		case <-r.Context().Done():
			return
		case <-testCtx.Done():
			return
		}
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-testCtx.Done():
		}
	}))

	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := client.DeleteSandbox(ctx, "sandbox-1")
		result <- err
	}()

	select {
	case path := <-arrived:
		if want := ownsandv1connect.SandboxServiceDeleteSandboxProcedure; path != want {
			t.Fatalf("request path = %q, want %q", path, want)
		}
	case err := <-result:
		t.Fatalf("DeleteSandbox returned before reaching the h2c fixture: %v", err)
	case <-time.After(wait):
		t.Fatal("timed out waiting for DeleteSandbox to reach the h2c fixture")
	}

	cancel()
	select {
	case err := <-result:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("DeleteSandbox error = %v, want canceled", err)
		}
	case <-time.After(wait):
		t.Fatal("timed out waiting for DeleteSandbox to return after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(wait):
		t.Fatal("the server did not observe request cancellation")
	}
}

func newDeleteSandboxFixture(t *testing.T, handler http.Handler) ownsandclient.Client {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)

	factory := ownsandclient.SDKFactory{BaseURL: server.URL}
	client, err := factory.New("org-1", "osk_"+strings.Repeat("a", 43))
	if err != nil {
		t.Fatal(err)
	}
	return client
}
