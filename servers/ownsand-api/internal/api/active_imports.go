// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"sync"

	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// activeImportKey identifies one import command that the server sent on one
// agent channel for one snapshot.
type activeImportKey struct {
	// conn is the channel generation that received the command. A
	// replacement channel of the same device is a different pointer, so it
	// needs its own assignment.
	conn       *hub.Conn
	snapshotID string
	commandID  string
}

// activeImports tracks the snapshot imports that importThenCreate has
// assigned and still waits for. It is in memory only: an import never
// outlives the channel it runs on, because the wait ends with the channel.
// The zero value is ready to use.
type activeImports struct {
	mu      sync.Mutex
	pending map[activeImportKey]struct{}
}

// register records an import assignment before its command is sent and
// returns the function that clears it. The function is idempotent; the
// caller invokes it on completion, send failure, timeout and channel close.
func (a *activeImports) register(conn *hub.Conn, snapshotID, commandID string) (release func()) {
	key := activeImportKey{conn: conn, snapshotID: snapshotID, commandID: commandID}
	a.mu.Lock()
	if a.pending == nil {
		a.pending = map[activeImportKey]struct{}{}
	}
	a.pending[key] = struct{}{}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.pending, key)
			a.mu.Unlock()
		})
	}
}

// assigned reports whether conn runs an assigned import of snapshotID. A
// closed channel never counts, even before its waiter cleared the entry.
func (a *activeImports) assigned(conn *hub.Conn, snapshotID string) bool {
	if conn.Context().Err() != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.pending {
		if key.conn == conn && key.snapshotID == snapshotID {
			return true
		}
	}
	return false
}
