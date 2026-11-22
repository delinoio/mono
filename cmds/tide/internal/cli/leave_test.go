// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
)

// TestMemberLeaveRefreshesOnce checks that member leave refreshes an
// expiring Logto token once and uses the rotated token for both the
// caller ID and RemoveMember. The fake Logto rotates the refresh token and
// rejects a used one, like Logto with refresh token rotation.
func TestMemberLeaveRefreshesOnce(t *testing.T) {
	f := newFixture(t)
	jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"`+f.srv.UserID+`"}`)) + ".sig"
	f.srv.AccessToken = jwt
	var mu sync.Mutex
	valid, refreshes := "refresh-1", 0
	logto := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		refreshes++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/oidc/token" || r.FormValue("refresh_token") != valid {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		valid = "refresh-2"
		_, _ = w.Write([]byte(`{"access_token":"` + jwt + `","refresh_token":"refresh-2","expires_in":3600}`))
	}))
	t.Cleanup(logto.Close)
	st := credstore.Store{Dir: f.cfg, Program: "tide"}
	if err := st.Save(&credstore.Credentials{
		LogtoEndpoint: logto.URL, AppID: "app", Resource: DefaultLogtoResource,
		RefreshToken: "refresh-1", AccessToken: "expired", AccessTokenExpiry: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	f.ok("", "--org", f.org, "member", "leave")
	if refreshes != 1 {
		t.Fatalf("Logto refreshes = %d, want 1", refreshes)
	}
	creds, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.RefreshToken != "refresh-2" {
		t.Fatalf("stored refresh token = %q", creds.RefreshToken)
	}
}
