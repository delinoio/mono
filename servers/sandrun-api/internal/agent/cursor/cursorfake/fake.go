// SPDX-License-Identifier: FSL-1.1-ALv2

// Package cursorfake is an in-memory fake of the Cursor self-hosted pool
// API for sandrun-api tests, served by httptest: pending requests of one pool,
// the list with a stream cursor, the SSE stream (`created` events and
// heartbeats), claims, releases, fails and worker status. It models only
// what the Cursor documentation states; a test drives the rest (worker
// connections, stream drops).
package cursorfake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Request is one pending request of the fake pool.
type Request struct {
	ID          string `json:"id"`
	CreatedAtMs int64  `json:"createdAtMs"`
}

// Page is one scripted pending-list response.
type Page struct {
	Requests      []Request `json:"requests"`
	NextPageToken string    `json:"nextPageToken,omitempty"`
	StreamCursor  string    `json:"streamCursor"`
}

// ListCall records the token and arrival time of a pending-list call.
type ListCall struct {
	PageToken string
	At        time.Time
}

type blockedClaim struct {
	id       string
	resume   <-chan struct{}
	canceled chan struct{}
}

// Fake is the fake Cursor API.
type Fake struct {
	Server *httptest.Server
	Key    string
	Pool   string

	mu sync.Mutex
	// pending are the unclaimed requests in the queue, in order.
	pending []Request
	// claims maps a request ID to the worker ID of its live claim.
	claims map[string]string
	// connected are the worker IDs whose worker is connected.
	connected map[string]bool
	// Failed and Released record the fail messages and releases by request.
	failed        map[string]string
	failAttempts  map[string]int
	released      map[string]int
	claimsN       map[string]int
	streams       map[int]chan string
	nextID        int
	pages         []Page
	listCalls     []ListCall
	claimCalls    []string
	blockedClaim  *blockedClaim
	streamCursors []string
	auth          []string
	// failClaimCalls makes release and fail answer 503.
	failClaimCalls bool
}

// New starts a fake for a pool with one service account key.
func New(t testing.TB, key, pool string) *Fake {
	f := &Fake{Key: key, Pool: pool, claims: map[string]string{}, connected: map[string]bool{}, failed: map[string]string{},
		failAttempts: map[string]int{}, released: map[string]int{}, claimsN: map[string]int{}, streams: map[int]chan string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.DropStreams()
		f.Server.Close()
	})
	return f
}

func (f *Fake) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	f.mu.Lock()
	f.auth = append(f.auth, h)
	f.mu.Unlock()
	return h == "Basic "+base64.StdEncoding.EncodeToString([]byte(f.Key+":")) || h == "Bearer "+f.Key
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		return
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v0/private-workers/pending-requests":
		if r.URL.Query().Get("pool") != f.Pool {
			writeJSON(w, http.StatusOK, map[string]any{"requests": []Request{}, "streamCursor": "c0"})
			return
		}
		f.mu.Lock()
		f.listCalls = append(f.listCalls, ListCall{PageToken: r.URL.Query().Get("pageToken"), At: time.Now()})
		var page Page
		if len(f.pages) > 0 {
			page, f.pages = f.pages[0], f.pages[1:]
		} else {
			page = Page{Requests: append([]Request{}, f.pending...), StreamCursor: fmt.Sprintf("cursor-%d", time.Now().UnixNano())}
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, page)
	case r.Method == http.MethodGet && path == "/v0/private-workers/pending-requests/stream":
		f.stream(w, r)
	case r.Method == http.MethodPost && path == "/v0/private-workers/claim":
		var body struct {
			ID       string `json:"id"`
			WorkerID string `json:"workerId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" || body.WorkerID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		f.mu.Lock()
		f.claimCalls = append(f.claimCalls, body.ID)
		block := f.blockedClaim
		if block != nil && block.id == body.ID {
			f.blockedClaim = nil
			f.mu.Unlock()
			select {
			case <-block.resume:
			case <-r.Context().Done():
				close(block.canceled)
				return
			}
			f.mu.Lock()
		}
		defer f.mu.Unlock()
		if _, ok := f.claims[body.ID]; ok {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already claimed"})
			return
		}
		idx := -1
		for i, p := range f.pending {
			if p.ID == body.ID {
				idx = i
			}
		}
		if idx < 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such request"})
			return
		}
		f.pending = append(f.pending[:idx], f.pending[idx+1:]...)
		f.claims[body.ID] = body.WorkerID
		f.claimsN[body.ID]++
		f.broadcastLocked("claimed", map[string]string{"id": body.ID})
		writeJSON(w, http.StatusOK, map[string]string{"id": body.ID, "workerId": body.WorkerID})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v0/private-workers/claims/"):
		rest := strings.TrimPrefix(path, "/v0/private-workers/claims/")
		id, action, _ := strings.Cut(rest, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if action == "fail" {
			f.failAttempts[id]++
		}
		if f.failClaimCalls {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		worker, ok := f.claims[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no live claim"})
			return
		}
		switch action {
		case "release":
			delete(f.claims, id)
			f.released[id]++
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "workerId": worker})
		case "fail":
			var body struct {
				Message string `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" || len(body.Message) > 500 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
				return
			}
			if f.connected[worker] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Worker connected: the worker serves the turn"})
				return
			}
			delete(f.claims, id)
			f.failed[id] = body.Message
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "workerId": worker})
		default:
			http.NotFound(w, r)
		}
	case r.Method == http.MethodGet && path == "/v0/private-workers":
		ids := strings.Split(r.URL.Query().Get("workerIds"), ",")
		f.mu.Lock()
		var workers []map[string]any
		var missing []string
		for _, id := range ids {
			if f.connected[id] {
				workers = append(workers, map[string]any{"workerId": id, "connectedAtMs": time.Now().UnixMilli(), "isInUse": true})
			} else {
				missing = append(missing, id)
			}
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"workers": workers, "notFoundWorkerIds": missing, "observedAtMs": time.Now().UnixMilli()})
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) stream(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	f.mu.Lock()
	f.streamCursors = append(f.streamCursors, cursor)
	f.mu.Unlock()
	if cursor == "" || r.URL.Query().Get("pool") != f.Pool {
		writeJSON(w, http.StatusGone, map[string]string{"code": "cursor_expired"})
		return
	}
	ch := make(chan string, 64)
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.streams[id] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.streams, id)
		f.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fmt.Fprint(w, ": connected\n\n")
	if fl != nil {
		fl.Flush()
	}
	hb := time.NewTicker(50 * time.Millisecond)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, msg)
		case <-hb.C:
			fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n")
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

func (f *Fake) broadcastLocked(event string, v any) {
	b, _ := json.Marshal(v)
	f.nextID++
	msg := fmt.Sprintf("event: %s\nid: ev-%d\ndata: %s\n\n", event, f.nextID, b)
	for _, ch := range f.streams {
		select {
		case ch <- msg:
		default:
		}
	}
}

// AddRequest queues a pending request. With announce, open streams get a
// `created` event; without, only a list shows it (a dropped event).
func (f *Fake) AddRequest(id string, announce bool) Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := Request{ID: id, CreatedAtMs: time.Now().UnixMilli()}
	f.pending = append(f.pending, r)
	if announce {
		f.broadcastLocked("created", r)
	}
	return r
}

// HoldClaim blocks the next claim of id until resume closes or the caller
// cancels the HTTP request. The returned channel closes on cancellation.
func (f *Fake) HoldClaim(id string, resume <-chan struct{}) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	canceled := make(chan struct{})
	f.blockedClaim = &blockedClaim{id: id, resume: resume, canceled: canceled}
	return canceled
}

// Announce sends a `created` event for a request again (a duplicate).
func (f *Fake) Announce(id string, createdAtMs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastLocked("created", Request{ID: id, CreatedAtMs: createdAtMs})
}

// DropStreams ends every open stream, as a network failure does.
func (f *Fake) DropStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, ch := range f.streams {
		close(ch)
		delete(f.streams, id)
	}
}

// Streams returns the number of open streams.
func (f *Fake) Streams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// Lists returns the number of list calls of the pool.
func (f *Fake) Lists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listCalls)
}

// ScriptPages serves the given pages in order before returning to the
// normal pending list.
func (f *Fake) ScriptPages(pages ...Page) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages = slices.Clone(pages)
}

// WatchCalls returns pending-list calls, attempted claim IDs and stream
// cursors in the order they arrived at each endpoint.
func (f *Fake) WatchCalls() (lists []ListCall, claims, streams []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.listCalls), slices.Clone(f.claimCalls), slices.Clone(f.streamCursors)
}

// Claim returns the worker ID of the live claim of a request.
func (f *Fake) Claim(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.claims[id]
	return w, ok
}

// ClaimCount returns how often a request was claimed.
func (f *Fake) ClaimCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimsN[id]
}

// Failed returns the fail message of a request.
func (f *Fake) Failed(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.failed[id]
	return m, ok
}

// FailAttempts returns how often the fail endpoint was called for a request.
func (f *Fake) FailAttempts(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failAttempts[id]
}

// Released returns how often the claim of a request was released.
func (f *Fake) Released(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released[id]
}

// Connect marks a worker as connected, as a started `agent worker` is.
func (f *Fake) Connect(workerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[workerID] = true
}

// Disconnect marks a worker as gone and frees its claim, as Cursor does
// when the worker exits after its idle release timeout.
func (f *Fake) Disconnect(workerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.connected, workerID)
	for id, w := range f.claims {
		if w == workerID {
			delete(f.claims, id)
		}
	}
}

// FailClaimCalls makes release and fail answer 503 while on is true.
func (f *Fake) FailClaimCalls(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failClaimCalls = on
}

// AuthHeaders returns every Authorization header that the fake received.
func (f *Fake) AuthHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}
