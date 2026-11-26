// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
)

func TestLoginPromptWrite(t *testing.T) {
	writeErr := errors.New("stderr write rejected")
	for _, tt := range []struct {
		name       string
		writeErr   error
		recover    bool
		wantExit   int
		wantGrants []string
		wantStderr string
	}{
		{
			name: "stderr always fails", writeErr: writeErr, wantExit: ExitError,
		},
		{
			name: "stderr recovers", writeErr: writeErr, recover: true, wantExit: ExitError,
			wantStderr: "write the sign-in instructions: " + writeErr.Error(),
		},
		{
			name: "stderr is writable", wantExit: ExitOK,
			wantGrants: []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
			wantStderr: "Open https://login.example/verify?code=ABCD in a browser and enter the code ABCD to sign in.\nSigned in.\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var deviceRequests int
			var grants []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/oidc/device/auth":
					deviceRequests++
					io.WriteString(w, `{"device_code":"device","user_code":"ABCD","verification_uri":"https://login.example/verify","verification_uri_complete":"https://login.example/verify?code=ABCD","expires_in":60}`)
				case "/oidc/token":
					grant := r.FormValue("grant_type")
					grants = append(grants, grant)
					if tt.writeErr != nil {
						http.Error(w, `{"error":"access_denied"}`, http.StatusBadRequest)
						return
					}
					switch grant {
					case "urn:ietf:params:oauth:grant-type:device_code":
						io.WriteString(w, `{"refresh_token":"device-refresh"}`)
					case "refresh_token":
						io.WriteString(w, `{"access_token":"api-access","refresh_token":"api-refresh","expires_in":3600}`)
					default:
						http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			store := credstore.Store{Dir: t.TempDir(), Program: "sandrun"}
			existing := []byte(`{"refresh_token":"existing"}`)
			if err := os.WriteFile(store.Path(), existing, 0o600); err != nil {
				t.Fatal(err)
			}
			vars := map[string]string{
				EnvConfigDir: store.Dir, EnvLogtoEndpoint: server.URL, EnvLogtoAppID: "app", EnvNoColor: "1",
			}
			var stdout, stderr bytes.Buffer
			status := Run(t.Context(), Env{
				Args: []string{"login"}, Stdout: &stdout,
				Stderr: &loginPromptStderr{Writer: &stderr, err: tt.writeErr, recover: tt.recover},
				LookupEnv: func(key string) (string, bool) {
					value, ok := vars[key]
					return value, ok
				},
				HTTPClient: server.Client(), LoginPollInterval: time.Millisecond,
			})

			if status != tt.wantExit {
				t.Errorf("login exit = %d, want %d", status, tt.wantExit)
			}
			if stdout.Len() != 0 {
				t.Errorf("login stdout = %q, want empty output", stdout.String())
			}
			mu.Lock()
			if deviceRequests != 1 || !slices.Equal(grants, tt.wantGrants) {
				t.Errorf("device requests = %d, token grants = %v, want 1 and %v", deviceRequests, grants, tt.wantGrants)
			}
			mu.Unlock()
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("login stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
			if tt.writeErr != nil {
				got, err := os.ReadFile(store.Path())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, existing) {
					t.Error("login changed the existing credential file")
				}
				return
			}
			creds, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if creds.AccessToken != "api-access" || creds.RefreshToken != "api-refresh" || creds.SessionID == "" {
				t.Error("login did not store the API tokens and a new session ID")
			}
		})
	}
}

type loginPromptStderr struct {
	io.Writer
	err     error
	recover bool
}

func (w *loginPromptStderr) Write(p []byte) (int, error) {
	if w.err != nil {
		err := w.err
		if w.recover {
			w.err = nil
		}
		return 0, err
	}
	return w.Writer.Write(p)
}
