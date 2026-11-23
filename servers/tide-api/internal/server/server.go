// SPDX-License-Identifier: FSL-1.1-ALv2

// Package server builds the tide-api HTTP handler: the Connect handlers,
// the plain HTTP endpoints of later layers (webhooks), `GET /healthz` and
// CORS for the console origins. The per-RPC log record is written by the API
// interceptors.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mount is one handler and its http.ServeMux pattern: the path prefix of a
// Connect service, or a method and path such as "POST /webhooks/github".
type Mount struct {
	Path    string
	Handler http.Handler
}

// Options configures NewHandler.
type Options struct {
	Logger *slog.Logger
	// CORSOrigins are the browser origins that may call the API.
	CORSOrigins []string
	Mounts      []Mount
}

// corsAllowedHeaders are the request headers that Connect and gRPC-Web
// need.
var corsAllowedHeaders = []string{
	"Authorization", "Content-Type", "Connect-Protocol-Version", "Connect-Timeout-Ms",
	"Connect-Content-Encoding", "Connect-Accept-Encoding", "Grpc-Timeout", "X-Grpc-Web",
	"X-User-Agent",
}

// corsExposedHeaders are the response headers that browser clients read.
var corsExposedHeaders = []string{
	"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin", "Connect-Content-Encoding",
	"Connect-Accept-Encoding",
}

// corsMaxAge is how long a browser may cache a preflight answer.
const corsMaxAge = 2 * time.Hour

// NewHandler returns the root HTTP handler.
func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	for _, m := range opts.Mounts {
		mux.Handle(m.Path, m.Handler)
	}
	return cors(opts.CORSOrigins, mux)
}

// cors answers preflight requests and adds the CORS headers for allowed
// origins. Requests from other origins get no CORS headers, so browsers
// block them.
func cors(origins []string, next http.Handler) http.Handler {
	allowHeaders := strings.Join(corsAllowedHeaders, ", ")
	exposeHeaders := strings.Join(corsExposedHeaders, ", ")
	maxAge := strconv.Itoa(int(corsMaxAge / time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !slices.Contains(origins, origin) {
			if r.Method == http.MethodOptions && origin != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", exposeHeaders)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", allowHeaders)
			h.Set("Access-Control-Max-Age", maxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
