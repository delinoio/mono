// SPDX-License-Identifier: FSL-1.1-ALv2

package devin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/vendorhttp"
)

func TestWatchHTTPResponseHeaderTimeout(t *testing.T) {
	if got := defaultWatchHTTP.Transport.(*http.Transport).ResponseHeaderTimeout; got != 30*time.Second || defaultWatchHTTP.Timeout != 0 {
		t.Fatalf("default watch timeouts: headers %s, overall %s", got, defaultWatchHTTP.Timeout)
	}
	for _, protoMajor := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", protoMajor), func(t *testing.T) {
			const headerTimeout = 150 * time.Millisecond
			canceled := make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != protoMajor {
					t.Errorf("watch used %s, want HTTP/%d", r.Proto, protoMajor)
				}
				if r.URL.Path == "/withhold" {
					<-r.Context().Done()
					close(canceled)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				time.Sleep(2 * headerTimeout)
				fmt.Fprint(w, "data: after the header budget\n\n")
			}))
			if protoMajor == 2 {
				srv.EnableHTTP2 = true
				srv.StartTLS()
			} else {
				srv.Start()
			}
			t.Cleanup(srv.Close)
			client := newWatchHTTPClient(srv.Client().Transport.(*http.Transport), headerTimeout)
			t.Cleanup(client.CloseIdleConnections)
			c := vendorhttp.Client{Vendor: "DEVIN", BaseURL: srv.URL, HTTP: client}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			received := ""
			read := func(event vendorhttp.Event) error {
				received = string(event.Data)
				return nil
			}
			err := c.Stream(ctx, "watch_queue", "/withhold", nil, nil, read)
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() || ctx.Err() != nil {
				t.Fatalf("withheld headers: got %v, want a transport timeout while the parent remains active", err)
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("header timeout did not cancel the HTTP request")
			}
			if err := c.Stream(ctx, "watch_queue", "/stream", nil, nil, read); err != nil || received != "after the header budget" {
				t.Fatalf("stream after the header budget: event %q, error %v", received, err)
			}
		})
	}
}
