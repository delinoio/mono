// SPDX-License-Identifier: FSL-1.1-ALv2

// Package githubfake is an in-memory fake of the GitHub REST API and of the
// app webhook sender for tide-api tests. It serves, on an httptest server,
// the endpoints that package githubapi calls: installation tokens (it
// verifies the RS256 app JWT), the organization runner endpoints, the OAuth
// code exchange, GET /user/installations and the app webhook deliveries
// with redelivery. Send and SendLost deliver signed webhooks to a target URL
// and record the deliveries like GitHub, so tests can lose a delivery and
// let the redelivery job repeat it.
package githubfake

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
)

// Fake is the fake GitHub.
type Fake struct {
	Server *httptest.Server

	ClientID      string
	ClientSecret  string
	AppID         int64
	WebhookSecret string
	PrivateKey    *rsa.PrivateKey

	mu            sync.Mutex
	now           func() time.Time
	installations map[int64]*Installation
	tokens        map[string]int64 // installation token -> installation ID
	codes         map[string]string
	userTokens    map[string]string // user token -> user
	runners       map[int64]*Runner
	nextRunner    int64
	deliveries    []*Delivery
	nextDelivery  int64
	calls         map[string]int
	// Target receives the webhook deliveries; TargetClient sends them.
	Target       string
	TargetClient *http.Client
	// FailDelete makes DeleteRunner answer this status (0: normal).
	FailDelete int
	// FailJIT makes generate-jitconfig answer this status once.
	FailJIT int
	// JITGate, when set, runs before a generate-jitconfig call is served,
	// without the fake lock, so a test can hold a runner start there.
	JITGate func()
	// DeleteGate runs before the fake serves a runner deletion, without the fake lock.
	DeleteGate func(runnerID int64)
}

// Installation is a fake installation.
type Installation struct {
	githubapi.Installation
	Suspended bool
	// Users can see the installation in GET /user/installations.
	Users  []string
	Groups []githubapi.RunnerGroup
}

// Runner is a registered fake runner.
type Runner struct {
	githubapi.Runner
	Org    string
	Labels []string
	JIT    string
}

// Delivery is a recorded webhook delivery.
type Delivery struct {
	githubapi.Delivery
	body []byte
}

// New starts a fake. Close it with Server.Close (New registers nothing
// with a test).
func New(now func() time.Time) *Fake {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	if now == nil {
		now = time.Now
	}
	f := &Fake{
		ClientID: "Iv23fakeclient", ClientSecret: "fake-client-secret", AppID: 4242, WebhookSecret: "fake-webhook-secret-0123456789",
		PrivateKey: key, now: now, installations: map[int64]*Installation{}, tokens: map[string]int64{}, codes: map[string]string{},
		userTokens: map[string]string{}, runners: map[int64]*Runner{}, nextRunner: 100, nextDelivery: 9000, calls: map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Config returns the githubapi configuration of the fake.
func (f *Fake) Config() githubapi.Config {
	return githubapi.Config{AppID: f.AppID, ClientID: f.ClientID, ClientSecret: f.ClientSecret, PrivateKey: f.PrivateKey, Slug: "tide-test",
		APIOrigin: f.Server.URL, WebOrigin: f.Server.URL}
}

// AddInstallation adds an installation on an account.
func (f *Fake) AddInstallation(id int64, login, accountType string, users ...string) *Installation {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst := &Installation{
		Installation: githubapi.Installation{ID: id, AppID: f.AppID, Account: githubapi.Account{ID: id * 10, Login: login, Type: accountType}, TargetType: accountType},
		Users:        users,
		Groups:       []githubapi.RunnerGroup{{ID: 1, Name: "Default", Visibility: "all", Default: true}, {ID: 7, Name: "tide", Visibility: "selected"}},
	}
	f.installations[id] = inst
	return inst
}

// Update runs fn with the fake lock held.
func (f *Fake) Update(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// SetSuspended suspends or unsuspends an installation.
func (f *Fake) SetSuspended(id int64, v bool) { f.Update(func() { f.installations[id].Suspended = v }) }

// AddCode registers an OAuth code of user.
func (f *Fake) AddCode(code, user string) { f.Update(func() { f.codes[code] = user }) }

// Calls returns the number of calls of an operation (for example
// "generate_jitconfig").
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// Runners returns copies of the registered runners, by ID.
func (f *Fake) Runners() []Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Runner
	for _, r := range f.runners {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RunnerByName returns a registered runner.
func (f *Fake) RunnerByName(name string) (Runner, bool) {
	for _, r := range f.Runners() {
		if r.Name == name {
			return r, true
		}
	}
	return Runner{}, false
}

// ForgetRunner removes a registration outside the REST API.
func (f *Fake) ForgetRunner(id int64) {
	f.Update(func() { delete(f.runners, id) })
}

// SetBusy marks a runner busy (it took a job), so a removal gets 422.
func (f *Fake) SetBusy(name string, busy bool) {
	f.Update(func() {
		for _, r := range f.runners {
			if r.Name == name {
				r.Busy = busy
			}
		}
	})
}

// MarkFailed records the deliveries of a GUID as failed, as GitHub does
// when the receiver answered too late.
func (f *Fake) MarkFailed(guid string) {
	f.Update(func() {
		for _, d := range f.deliveries {
			if d.GUID == guid {
				d.StatusCode, d.Status = http.StatusGatewayTimeout, "timed out"
			}
		}
	})
}

// Deliveries returns copies of the recorded deliveries, newest first.
func (f *Fake) Deliveries() []githubapi.Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]githubapi.Delivery, 0, len(f.deliveries))
	for i := len(f.deliveries) - 1; i >= 0; i-- {
		out = append(out, f.deliveries[i].Delivery)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) verifyJWT(r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.PrivateKey.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	now := f.now().Unix()
	return claims.Iss == f.ClientID && claims.Exp > now && claims.Exp-claims.Iat <= 600 && claims.Iat <= now
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if strings.HasSuffix(p, "/generate-jitconfig") {
		f.mu.Lock()
		gate := f.JITGate
		f.mu.Unlock()
		if gate != nil {
			gate()
		}
	}
	if r.Method == http.MethodDelete && len(parts) == 5 && parts[0] == "orgs" && parts[2] == "actions" && parts[3] == "runners" {
		f.mu.Lock()
		gate := f.DeleteGate
		f.mu.Unlock()
		if gate != nil {
			id, _ := strconv.ParseInt(parts[4], 10, 64)
			gate(id)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	count := func(op string) { f.calls[op]++ }
	switch {
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens":
		count("create_installation_token")
		if !f.verifyJWT(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		inst := f.installations[id]
		if inst == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		if inst.Suspended {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "This installation has been suspended"})
			return
		}
		token := "ghs_fake_" + randomHex(8)
		f.tokens[token] = id
		writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expires_at": f.now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case len(parts) >= 4 && parts[0] == "orgs" && parts[2] == "actions":
		f.serveOrg(w, r, parts[1], parts[3:])
	case r.Method == http.MethodPost && p == "/login/oauth/access_token":
		count("exchange_oauth_code")
		_ = r.ParseForm()
		user, ok := f.codes[r.PostForm.Get("code")]
		if !ok || r.PostForm.Get("client_id") != f.ClientID || r.PostForm.Get("client_secret") != f.ClientSecret || r.Header.Get("Accept") != "application/json" {
			writeJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
			return
		}
		delete(f.codes, r.PostForm.Get("code"))
		token := "ghu_fake_" + randomHex(8)
		f.userTokens[token] = user
		writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "token_type": "bearer", "scope": ""})
	case r.Method == http.MethodGet && p == "/user/installations":
		count("list_user_installations")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		user, ok := f.userTokens[token]
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		var list []githubapi.Installation
		for _, inst := range f.installations {
			for _, u := range inst.Users {
				if u == user {
					list = append(list, inst.Installation)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "installations": list})
	case r.Method == http.MethodGet && p == "/app/hook/deliveries":
		count("list_hook_deliveries")
		if !f.verifyJWT(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		f.listDeliveries(w, r)
	case r.Method == http.MethodPost && len(parts) == 5 && parts[0] == "app" && parts[1] == "hook" && parts[2] == "deliveries" && parts[4] == "attempts":
		count("redeliver_hook_delivery")
		if !f.verifyJWT(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		id, _ := strconv.ParseInt(parts[3], 10, 64)
		for _, d := range f.deliveries {
			if d.ID == id {
				writeJSON(w, http.StatusAccepted, map[string]any{})
				// Deliver after the answer, without the lock.
				go f.deliver(d.Event, d.GUID, d.body, true)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (f *Fake) installationOf(r *http.Request, org string) (*Installation, int) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := f.tokens[token]
	if !ok {
		return nil, http.StatusUnauthorized
	}
	inst := f.installations[id]
	if inst == nil || inst.Suspended {
		return nil, http.StatusForbidden
	}
	if !strings.EqualFold(inst.Account.Login, org) {
		return nil, http.StatusNotFound
	}
	return inst, 0
}

func (f *Fake) serveOrg(w http.ResponseWriter, r *http.Request, org string, rest []string) {
	inst, status := f.installationOf(r, org)
	op := "org_" + strings.Join(rest, "_")
	switch {
	case r.Method == http.MethodGet && rest[0] == "runner-groups":
		op = "list_runner_groups"
	case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "generate-jitconfig":
		op = "generate_jitconfig"
	case r.Method == http.MethodGet && len(rest) == 1 && rest[0] == "runners":
		op = "list_runners"
	case r.Method == http.MethodDelete && len(rest) == 2 && rest[0] == "runners":
		op = "delete_runner"
	}
	f.calls[op]++
	if inst == nil {
		writeJSON(w, status, map[string]string{"message": "denied"})
		return
	}
	switch op {
	case "list_runner_groups":
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(inst.Groups), "runner_groups": inst.Groups})
	case "generate_jitconfig":
		if f.FailJIT != 0 {
			s := f.FailJIT
			f.FailJIT = 0
			writeJSON(w, s, map[string]string{"message": "failed"})
			return
		}
		var body struct {
			Name          string   `json:"name"`
			RunnerGroupID int64    `json:"runner_group_id"`
			Labels        []string `json:"labels"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name == "" || body.RunnerGroupID == 0 || len(body.Labels) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
			return
		}
		for _, x := range f.runners {
			if x.Name == body.Name {
				writeJSON(w, http.StatusConflict, map[string]string{"message": "Already exists"})
				return
			}
		}
		f.nextRunner++
		run := &Runner{Runner: githubapi.Runner{ID: f.nextRunner, Name: body.Name, RunnerGroupID: body.RunnerGroupID, Status: "offline", Ephemeral: true},
			Org: inst.Account.Login, Labels: append([]string{"self-hosted"}, body.Labels...), JIT: "jit-" + randomHex(16)}
		f.runners[run.ID] = run
		writeJSON(w, http.StatusCreated, map[string]any{"runner": run.Runner, "encoded_jit_config": run.JIT})
	case "list_runners":
		name := r.URL.Query().Get("name")
		var list []githubapi.Runner
		for _, x := range f.runners {
			if strings.EqualFold(x.Org, org) && (name == "" || x.Name == name) {
				list = append(list, x.Runner)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "runners": list})
	case "delete_runner":
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		x := f.runners[id]
		switch {
		case f.FailDelete != 0:
			writeJSON(w, f.FailDelete, map[string]string{"message": "failed"})
		case x == nil:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		case x.Busy:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed", "documentation_url": "x"})
		default:
			delete(f.runners, id)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (f *Fake) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 30
	}
	cursor, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	var list []githubapi.Delivery
	for i := len(f.deliveries) - 1; i >= 0; i-- {
		d := f.deliveries[i].Delivery
		if cursor != 0 && d.ID > cursor {
			continue
		}
		if q.Get("status") == "failure" && d.StatusCode < 400 {
			continue
		}
		list = append(list, d)
	}
	if len(list) > per {
		next := list[per].ID
		list = list[:per]
		u := *r.URL
		q.Set("cursor", strconv.FormatInt(next, 10))
		u.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.Server.URL, u.RequestURI()))
	}
	writeJSON(w, http.StatusOK, list)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Send delivers a signed webhook event to Target and records it. It returns
// the HTTP status of the receiver (0 when it could not be reached).
func (f *Fake) Send(event string, payload any) (string, int) {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	guid := randomHex(16)
	return guid, f.deliver(event, guid, body, false)
}

// SendRaw delivers a body with explicit headers (for signature tests)
// without recording it.
func (f *Fake) SendRaw(headers map[string]string, body []byte) int {
	req, _ := http.NewRequest(http.MethodPost, f.Target, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.TargetClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Headers returns the delivery headers of a signed body.
func (f *Fake) Headers(event, guid string, body []byte) map[string]string {
	return map[string]string{
		"Content-Type": "application/json", "User-Agent": "GitHub-Hookshot/fake", "X-GitHub-Event": event, "X-GitHub-Delivery": guid,
		"X-Hub-Signature-256": githubapi.Sign([]byte(f.WebhookSecret), body), "X-GitHub-Hook-Installation-Target-Type": "integration",
		"X-GitHub-Hook-Installation-Target-ID": strconv.FormatInt(f.AppID, 10),
	}
}

// SendLost records a delivery that failed (the receiver was not reached)
// without sending it, like a delivery during an outage.
func (f *Fake) SendLost(event string, payload any) string {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	guid := randomHex(16)
	f.record(event, guid, body, 502, false)
	return guid
}

func (f *Fake) deliver(event, guid string, body []byte, redelivery bool) int {
	status := f.SendRaw(f.Headers(event, guid, body), body)
	code := status
	if code == 0 {
		code = 502
	}
	f.record(event, guid, body, code, redelivery)
	return status
}

func (f *Fake) record(event, guid string, body []byte, code int, redelivery bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextDelivery++
	status := "OK"
	if code < 200 || code >= 300 {
		status = http.StatusText(code)
	}
	f.deliveries = append(f.deliveries, &Delivery{Delivery: githubapi.Delivery{ID: f.nextDelivery, GUID: guid, DeliveredAt: f.now().UTC(), Redelivery: redelivery,
		Status: status, StatusCode: code, Event: event}, body: body})
}
