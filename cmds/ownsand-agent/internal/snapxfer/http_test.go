// SPDX-License-Identifier: FSL-1.1-ALv2

package snapxfer

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestSnapshotRedirectOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, original, target string
		allowed                bool
	}{
		{"default HTTP port", "http://storage.invalid/part", "http://STORAGE.invalid:80/part", true},
		{"default HTTPS port", "https://storage.invalid/part", "https://storage.invalid:0443/part", true},
		{"IPv6 spelling", "http://[0:0:0:0:0:0:0:1]:8123/part", "http://[::1]:8123/part", true},
		{"IDNA spelling", "https://bücher.invalid/part", "https://xn--bcher-kva.invalid/part", true},
		{"different scheme", "https://storage.invalid:80/part", "http://storage.invalid:80/part", false},
		{"different hostname", "https://storage.invalid/part", "https://other.storage.invalid/part", false},
		{"different port", "https://storage.invalid/part", "https://storage.invalid:8443/part", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, err := http.NewRequest(http.MethodGet, tc.original, nil)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkRedirect(req, []*http.Request{original}); (err == nil) != tc.allowed {
				t.Errorf("checkRedirect() = %v, allowed = %v", err, tc.allowed)
			}
		})
	}
}

func TestSnapshotPartRedirectPolicy(t *testing.T) {
	const payload = "snapshot part"
	for _, tc := range []struct {
		name, method      string
		initialStatus     int
		leaveOrigin, loop bool
		wantRequests      int32
		wantError         string
	}{
		{name: "download chain", method: http.MethodGet, initialStatus: http.StatusFound, wantRequests: 3},
		{name: "download leaves origin", method: http.MethodGet, initialStatus: http.StatusFound, leaveOrigin: true, wantRequests: 2, wantError: "download request failed"},
		{name: "upload leaves origin", method: http.MethodPut, initialStatus: http.StatusTemporaryRedirect, leaveOrigin: true, wantRequests: 2, wantError: "upload request failed"},
		{name: "upload 301", method: http.MethodPut, initialStatus: http.StatusMovedPermanently, wantRequests: 1, wantError: "upload request failed"},
		{name: "upload 302", method: http.MethodPut, initialStatus: http.StatusFound, wantRequests: 1, wantError: "upload request failed"},
		{name: "upload 303", method: http.MethodPut, initialStatus: http.StatusSeeOther, wantRequests: 1, wantError: "upload request failed"},
		{name: "download loop", method: http.MethodGet, initialStatus: http.StatusTemporaryRedirect, loop: true, wantRequests: 10, wantError: "download request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("redirect contacted a different storage origin")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(destination.Close)
			var requests atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != tc.method {
					t.Errorf("storage received %s, want %s", r.Method, tc.method)
				}
				if r.Referer() != "" {
					t.Error("storage received a Referer header")
				}
				switch r.URL.Path {
				case "/first":
					http.Redirect(w, r, "/hop?sig=next", tc.initialStatus)
				case "/hop":
					target := "/final?sig=last"
					switch {
					case tc.leaveOrigin:
						target = destination.URL + target
					case tc.loop:
						target = "/first?sig=initial"
					}
					http.Redirect(w, r, target, http.StatusPermanentRedirect)
				case "/final":
					_, _ = w.Write([]byte(payload))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(source.Close)
			partFile, err := os.Create(filepath.Join(t.TempDir(), "part"))
			if err != nil {
				t.Fatal(err)
			}
			defer partFile.Close()
			tr := &Transfer{HTTP: NewHTTPClient(5 * time.Second), Logger: quietLogger()}
			start := source.URL + "/first?sig=initial"
			if tc.method == http.MethodPut {
				if _, err := partFile.WriteString(payload); err != nil {
					t.Fatal(err)
				}
				err = tr.put(t.Context(), start, partFile, 0, int64(len(payload)))
			} else {
				digest := sha256.Sum256([]byte(payload))
				err = tr.get(t.Context(), start, partFile, &ownsandv1.SnapshotPart{
					PartNumber: 1,
					SizeBytes:  int64(len(payload)),
					Sha256:     digest[:],
				})
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.wantError {
				t.Fatalf("transfer error = %v, want %q", err, tc.wantError)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("storage received %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}
