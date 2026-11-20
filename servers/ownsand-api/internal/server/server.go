// SPDX-License-Identifier: FSL-1.1-ALv2

// Package server builds the ownsand-api HTTP handler and server: the Connect
// services, GET /healthz and CORS, served over HTTP/1.1 and HTTP/2 without
// TLS (h2c), because the bidirectional agent channel and agent relay
// connection need HTTP/2 from end to end behind the Caddy reverse proxy.
package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Route registers one Connect service handler.
type Route struct {
	Path    string
	Handler http.Handler
}

// Allowed request headers of a cross-origin Connect call.
var corsAllowedHeaders = strings.Join([]string{
	"Authorization", "Content-Type", "Connect-Protocol-Version", "Connect-Timeout-Ms",
	"Connect-Accept-Encoding", "Connect-Content-Encoding", "Accept-Encoding", "Content-Encoding",
	"Grpc-Timeout", "X-Grpc-Web", "X-User-Agent", "Ownsand-Relay-Grant-Id",
}, ", ")

var corsExposedHeaders = strings.Join([]string{
	"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin", "Content-Encoding", "Connect-Content-Encoding",
}, ", ")

// NewHandler returns the root handler.
func NewHandler(routes []Route, allowedOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	for _, r := range routes {
		mux.Handle(r.Path, r.Handler)
	}
	return withCORS(mux, allowedOrigins)
}

func withCORS(next http.Handler, allowedOrigins []string) http.Handler {
	allowed := map[string]bool{}
	for _, o := range allowedOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if origin != "" && allowed[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				h.Set("Access-Control-Max-Age", "7200")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// A preflight from an origin that is not allowed gets no CORS headers.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HTTP server timeouts. There is no WriteTimeout and no ReadTimeout: the
// agent channel, the agent relay connection and the relay server streams are
// long-lived, and their limits are enforced by the handlers (heartbeats,
// keepalives, the 14-minute relay stream limit and the 5-minute WriteFile
// deadline). ReadHeaderTimeout bounds the request head; IdleTimeout bounds an
// idle keep-alive connection.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// New returns the http.Server that serves HTTP/1.1 and h2c.
func New(handler http.Handler, logger *slog.Logger) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:                      handler,
		Protocols:                    protocols,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            readHeaderTimeout,
		IdleTimeout:                  idleTimeout,
		MaxHeaderBytes:               64 << 10,
		ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}
