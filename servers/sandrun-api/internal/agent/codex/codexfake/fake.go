// SPDX-License-Identifier: FSL-1.1-ALv2

// Package codexfake is an httptest fake of the OpenAI Agents API session
// read (GET /v1/agents/sessions/{id}) and a webhook sender that signs
// deliveries like OpenAI (Standard Webhooks). It checks the bearer key and
// the OpenAI-Beta header of every call.
package codexfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/codex"
)

// Session is a fake agent session.
type Session struct {
	ID                 string
	Status             string
	NeedsConnection    bool
	EnvironmentID      string
	RemoteURL          string
	WorkspaceDirectory string
}

// Fake is the fake Agents API.
type Fake struct {
	Server *httptest.Server
	apiKey string

	mu       sync.Mutex
	sessions map[string]*Session
	reads    int
	// fail makes session reads answer this status (0: normal).
	fail int
}

// New starts a fake that accepts apiKey.
func New(t testing.TB, apiKey string) *Fake {
	f := &Fake{apiKey: apiKey, sessions: map[string]*Session{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// AddSession adds a self-hosted session that waits for an executor.
func (f *Fake) AddSession(id, environmentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = &Session{ID: id, Status: "requires_action", NeedsConnection: true, EnvironmentID: environmentID,
		RemoteURL: "https://api.openai.test/v1/agents/api", WorkspaceDirectory: "/tmp/codex-workspace"}
}

// Update changes a session.
func (f *Fake) Update(id string, fn func(*Session)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.sessions[id])
}

// Delete removes a session.
func (f *Fake) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
}

// Fail makes session reads answer status (0 restores them).
func (f *Fake) Fail(status int) {
	f.mu.Lock()
	f.fail = status
	f.mu.Unlock()
}

// Reads returns the number of session reads.
func (f *Fake) Reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.apiKey || r.Header.Get("OpenAI-Beta") != "agents=v1" {
		http.Error(w, `{"error":{"message":"unauthorized","code":"invalid_api_key"}}`, http.StatusUnauthorized)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/agents/sessions/")
	if r.Method != http.MethodGet || !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.reads++
	fail := f.fail
	s := f.sessions[id]
	var cp Session
	if s != nil {
		cp = *s
	}
	f.mu.Unlock()
	if fail != 0 {
		http.Error(w, `{"error":{"message":"fake failure"}}`, fail)
		return
	}
	if s == nil {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	actions := []map[string]string{}
	if cp.NeedsConnection {
		actions = append(actions, map[string]string{"type": "environment_connection", "environment_id": cp.EnvironmentID})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": cp.ID, "object": "agent.session", "status": cp.Status, "required_actions": actions,
		"environment": map[string]any{"type": "self_hosted", "id": cp.EnvironmentID, "remote_url": cp.RemoteURL,
			"workspace_directory": cp.WorkspaceDirectory, "capability_directories": []string{}},
		"created_at": 1750287018, "last_active_at": 1750287018,
	})
}

// Sender signs and posts webhook deliveries to a sandrun-api endpoint.
type Sender struct {
	URL    string
	Secret string
	// Now returns the delivery time; nil selects time.Now.
	Now func() time.Time
	// Client sends the deliveries; nil selects http.DefaultClient.
	Client *http.Client
}

// Delivery is one webhook delivery.
type Delivery struct {
	ID   string
	Body []byte
	// Timestamp overrides the webhook-timestamp (0: Now).
	Timestamp int64
	// Signature overrides the webhook-signature header ("": a valid one).
	Signature string
	// ExtraSignatures are added before the valid signature (rotation).
	ExtraSignatures []string
}

// ActionRequired returns the body of an agent.session.action_required
// event with the environment_connection action.
func ActionRequired(sessionID string) []byte {
	return []byte(fmt.Sprintf(`{"id":"evt_%s","object":"event","created_at":1750287018,"type":"agent.session.action_required","data":{"id":%q,"required_action":{"type":"environment_connection"}}}`,
		sessionID, sessionID))
}

// Event returns the body of another session event.
func Event(typ, sessionID string) []byte {
	return []byte(fmt.Sprintf(`{"id":"evt_x","object":"event","created_at":1750287018,"type":%q,"data":{"id":%q}}`, typ, sessionID))
}

// Send posts a delivery and returns the HTTP status.
func (s *Sender) Send(t testing.TB, d Delivery) int {
	t.Helper()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ts := d.Timestamp
	if ts == 0 {
		ts = now().Unix()
	}
	tsText := strconv.FormatInt(ts, 10)
	sig := d.Signature
	if sig == "" {
		key, err := codex.SecretKey(s.Secret)
		if err != nil {
			t.Fatal(err)
		}
		sig = strings.Join(append(append([]string(nil), d.ExtraSignatures...), "v1,"+codex.Sign(key, d.ID, tsText, d.Body)), " ")
	}
	req, err := http.NewRequest(http.MethodPost, s.URL, bytes.NewReader(d.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", d.ID)
	req.Header.Set("webhook-timestamp", tsText)
	req.Header.Set("webhook-signature", sig)
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
