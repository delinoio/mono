// SPDX-License-Identifier: FSL-1.1-ALv2

package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// corsAllowedHeaders are the request headers that Connect and gRPC-Web
// clients send.
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

// cors answers preflight requests and adds the CORS headers for the allowed
// origins (docs/servers-accounts-api-foundation.md, Authentication). A
// request from another origin gets no CORS headers, so browsers block it,
// and its preflight gets 403.
func cors(origins []string, next http.Handler) http.Handler {
	allowHeaders := strings.Join(corsAllowedHeaders, ", ")
	exposeHeaders := strings.Join(corsExposedHeaders, ", ")
	maxAge := strconv.Itoa(int(corsMaxAge / time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if origin == "" || !slices.Contains(origins, origin) {
			if preflight && origin != "" {
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
		if preflight {
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", allowHeaders)
			h.Set("Access-Control-Max-Age", maxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
