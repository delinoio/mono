// SPDX-License-Identifier: FSL-1.1-ALv2

// Package devinfake is an in-memory fake of the Devin Fleet API for
// tide-api tests, served by httptest: one outpost, its queue entries, the
// paged list, the SSE watch (`MODIFIED` and `DELETED` events), claims as a
// compare-and-swap, releases and operator messages. It models only what the
// Devin documentation states; a test drives the rest (session status,
// stream ends, watch failures).
package devinfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake is the fake Fleet API.
type Fake struct {
	Server      *httptest.Server
	Token       string
	OutpostID   string
	OutpostName string

	mu              sync.Mutex
	entries         map[string]*entry
	order           []string
	streams         map[int]chan string
	nextID          int
	lists           int
	failWatch       int
	queuePages      map[string]map[string]any
	claimAttempts   []string
	watchCursors    []string
	claims          map[string]int
	releaseAttempts map[string]int
	releases        map[string]int
	messages        map[string][]string
}

type entry struct {
	SessionID     string
	CreatedAt     int64
	Kind          string
	Phase         string
	AcceptorID    string
	SessionStatus string
}

func (e *entry) object(outpost string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"session_id": e.SessionID, "outpost_id": outpost, "created_at": e.CreatedAt, "updated_at": time.Now().Unix()},
		"spec":     map[string]any{"kind": e.Kind, "platform": "linux"},
		"status":   map[string]any{"phase": e.Phase, "acceptor_id": e.AcceptorID, "session_status": e.SessionStatus},
	}
}

// New starts a fake with one outpost and one API token.
func New(t testing.TB, token, outpostName string) *Fake {
	f := &Fake{Token: token, OutpostID: "outpost_env-0001", OutpostName: outpostName, entries: map[string]*entry{}, streams: map[int]chan string{},
		claims: map[string]int{}, releaseAttempts: map[string]int{}, releases: map[string]int{}, messages: map[string][]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.EndStreams()
		f.Server.Close()
	})
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}
	path := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && path == "/opbeta/outposts":
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"outpost_id": "outpost_env-other"}, "spec": map[string]any{"name": "other", "platform": "linux"}},
			map[string]any{"metadata": map[string]any{"outpost_id": f.OutpostID}, "spec": map[string]any{"name": f.OutpostName, "platform": "linux"}},
		}, "has_next_page": false})
	case r.Method == http.MethodGet && path == "/opbeta/outposts/devins" && q.Get("watch") == "true":
		f.watch(w, r)
	case r.Method == http.MethodGet && path == "/opbeta/outposts/devins":
		if q.Get("outpost") != f.OutpostID {
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "has_next_page": false, "cursor": "c0"})
			return
		}
		f.mu.Lock()
		f.lists++
		if f.queuePages != nil {
			page, ok := f.queuePages[q.Get("cursor")]
			f.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected queue cursor"})
				return
			}
			writeJSON(w, http.StatusOK, page)
			return
		}
		var items []any
		for _, id := range f.order {
			e := f.entries[id]
			if e == nil {
				continue
			}
			if q.Get("phase") == "" || q.Get("phase") == e.Phase {
				items = append(items, e.object(f.OutpostID))
			}
		}
		f.mu.Unlock()
		// One item per page, so tide follows the cursor.
		page, rest := items, []any(nil)
		start := 0
		if c := q.Get("cursor"); strings.HasPrefix(c, "page-") {
			fmt.Sscanf(c, "page-%d", &start)
		}
		if start < len(items) {
			page, rest = items[start:start+1], items[start+1:]
		} else {
			page = nil
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": page, "has_next_page": len(rest) > 0, "cursor": fmt.Sprintf("page-%d", start+1), "total": len(items)})
	case strings.HasPrefix(path, "/opbeta/outposts/devins/"):
		rest := strings.TrimPrefix(path, "/opbeta/outposts/devins/")
		sid, action, _ := strings.Cut(rest, "/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPost && action == "release" {
			f.releaseAttempts[sid]++
		}
		e, ok := f.entries[sid]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		var body struct {
			AcceptorID string `json:"acceptor_id"`
			Message    string `json:"message"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		switch {
		case r.Method == http.MethodGet && action == "":
			writeJSON(w, http.StatusOK, e.object(f.OutpostID))
		case r.Method == http.MethodPost && action == "claim":
			f.claimAttempts = append(f.claimAttempts, sid)
			if body.AcceptorID == "" || (e.Phase == "claimed" && e.AcceptorID != body.AcceptorID) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "already claimed"})
				return
			}
			e.Phase, e.AcceptorID = "claimed", body.AcceptorID
			f.claims[sid]++
			f.broadcastLocked("MODIFIED", e)
			obj := e.object(f.OutpostID)
			obj["status"].(map[string]any)["connect_token"] = "connect-token-secret"
			obj["status"].(map[string]any)["claim_deadline"] = time.Now().Add(time.Minute).Unix()
			writeJSON(w, http.StatusOK, obj)
		case r.Method == http.MethodPost && action == "release":
			if e.AcceptorID != body.AcceptorID {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "not the acceptor"})
				return
			}
			f.releases[sid]++
			if e.SessionStatus == "terminated" {
				// A terminated session leaves the queue.
				delete(f.entries, sid)
				f.broadcastLocked("DELETED", e)
				writeJSON(w, http.StatusOK, e.object(f.OutpostID))
				return
			}
			// The session returns to the queue at once.
			e.Phase, e.AcceptorID = "pending", ""
			f.broadcastLocked("MODIFIED", e)
			writeJSON(w, http.StatusOK, e.object(f.OutpostID))
		case r.Method == http.MethodPost && action == "operator-message":
			f.messages[sid] = append(f.messages[sid], body.Message)
			writeJSON(w, http.StatusOK, map[string]string{})
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) watch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.failWatch > 0 {
		f.failWatch--
		f.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	ch := make(chan string, 64)
	f.watchCursors = append(f.watchCursors, r.URL.Query().Get("cursor"))
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
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, msg)
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (f *Fake) broadcastLocked(typ string, e *entry) {
	f.nextID++
	b, _ := json.Marshal(map[string]any{"type": typ, "object": e.object(f.OutpostID), "cursor": fmt.Sprintf("ev-%d", f.nextID)})
	msg := fmt.Sprintf("data: %s\n\n", b)
	for _, ch := range f.streams {
		select {
		case ch <- msg:
		default:
		}
	}
}

// AddSession queues a pending session. With announce, open watches get a
// MODIFIED event; without, only a list shows it (a lost event).
func (f *Fake) AddSession(sid string, announce bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := &entry{SessionID: sid, CreatedAt: time.Now().Unix(), Kind: "new", Phase: "pending", SessionStatus: "pending"}
	f.entries[sid] = e
	f.order = append(f.order, sid)
	if announce {
		f.broadcastLocked("MODIFIED", e)
	}
}

// Announce sends the current state of a session again (a duplicate).
func (f *Fake) Announce(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcastLocked("MODIFIED", f.entries[sid])
}

// SetSessionStatus changes the session status, as a worker that connects
// (running) or a session that ends (terminated) does.
func (f *Fake) SetSessionStatus(sid, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[sid]
	e.SessionStatus = status
	f.broadcastLocked("MODIFIED", e)
}

// SetSessionStatusSilently changes the session status without a watch
// event, as when tide misses the event.
func (f *Fake) SetSessionStatusSilently(sid, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[sid].SessionStatus = status
}

// Forget removes a session without a watch event, so that reading its
// queue entry answers 404.
func (f *Fake) Forget(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, sid)
}

// DeleteSession removes a session and sends a DELETED event. A repeated
// call delivers the deletion again.
func (f *Fake) DeleteSession(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, sid)
	f.broadcastLocked("DELETED", &entry{SessionID: sid})
}

// ClaimElsewhere lets another acceptor take a pending session.
func (f *Fake) ClaimElsewhere(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[sid]
	e.Phase, e.AcceptorID = "claimed", "someone-else"
	f.broadcastLocked("MODIFIED", e)
}

// ExpireClaim makes a claim lapse: the session is pending again, as after
// its claim deadline.
func (f *Fake) ExpireClaim(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[sid]
	e.Phase, e.AcceptorID = "pending", ""
	f.broadcastLocked("MODIFIED", e)
}

// EndStreams ends every open watch, as Devin does after 5 minutes.
func (f *Fake) EndStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, ch := range f.streams {
		close(ch)
		delete(f.streams, id)
	}
}

// FailWatches makes the next n watch calls fail with 503.
func (f *Fake) FailWatches(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWatch = n
}

// Streams returns the number of open watches.
func (f *Fake) Streams() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

// Lists returns the number of list calls.
func (f *Fake) Lists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// QueuePage describes a scripted list response for a request cursor.
type QueuePage struct {
	SessionIDs  []string
	Cursor      string
	HasNextPage bool
}

// SetQueuePages serves fixed pages, including after a claim changes the live
// queue. Each key is the cursor that tide sends to request that page.
func (f *Fake) SetQueuePages(t testing.TB, pages map[string]QueuePage) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queuePages = make(map[string]map[string]any, len(pages))
	for cursor, page := range pages {
		items := make([]any, 0, len(page.SessionIDs))
		for _, sid := range page.SessionIDs {
			e := f.entries[sid]
			if e == nil {
				t.Fatalf("queue page names unknown session %q", sid)
			}
			items = append(items, e.object(f.OutpostID))
		}
		f.queuePages[cursor] = map[string]any{"items": items, "cursor": page.Cursor, "has_next_page": page.HasNextPage}
	}
}

// ClaimAttempts returns the session IDs of claim requests in order.
func (f *Fake) ClaimAttempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claimAttempts...)
}

// WatchCursors returns the request cursors of successfully opened watches.
func (f *Fake) WatchCursors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.watchCursors...)
}

// State returns the phase and acceptor ID of a session.
func (f *Fake) State(sid string) (phase, acceptorID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[sid]
	if e == nil {
		return "deleted", ""
	}
	return e.Phase, e.AcceptorID
}

// Claims returns how often a session was claimed.
func (f *Fake) Claims(sid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[sid]
}

// ReleaseAttempts returns the number of release calls, including rejected calls.
func (f *Fake) ReleaseAttempts(sid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releaseAttempts[sid]
}

// Releases returns how often a session's claim was released.
func (f *Fake) Releases(sid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases[sid]
}

// Messages returns the operator messages of a session.
func (f *Fake) Messages(sid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages[sid]...)
}
