// SPDX-License-Identifier: Apache-2.0

// Package credwatch detects changes of a credential file (the Codex
// `auth.json`), which the agent rewrites when it refreshes its token. It
// compares the modification time and the SHA-256 of the content and never
// exposes the content. It checks the file after each file system
// notification for it and at a fixed interval. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#credential-updates.
package credwatch

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Interval is the check interval. The interval check also finds changes
// whose notification was lost or that the file system does not report.
const Interval = 10 * time.Second

// Debounce is the quiet time after the last notification before the
// check. The agent writes the file with a truncation and one or more
// writes; a check between them would see a partial content.
const Debounce = 200 * time.Millisecond

type state struct {
	exists  bool
	modTime time.Time
	size    int64
	digest  [sha256.Size]byte
}

// Watcher watches one file.
type Watcher struct {
	path     string
	interval time.Duration
	debounce time.Duration
	last     state
	revision uint64
}

// New returns a watcher of path and records its current state, so only
// later changes count.
func New(path string, interval time.Duration) (*Watcher, error) {
	if interval <= 0 {
		interval = Interval
	}
	w := &Watcher{path: path, interval: interval, debounce: Debounce}
	current, err := w.read(state{})
	if err != nil {
		return nil, err
	}
	w.last = current
	return w, nil
}

// Check reads the file and reports whether its content changed since the
// last check, with the new revision. A changed modification time with the
// same content is not a change. A deleted file is not a change either; its
// next content is compared with the last content.
func (w *Watcher) Check() (changed bool, revision uint64, err error) {
	current, err := w.read(w.last)
	if err != nil {
		return false, w.revision, err
	}
	if !current.exists {
		return false, w.revision, nil
	}
	if w.last.exists && current.digest == w.last.digest {
		w.last = current
		return false, w.revision, nil
	}
	w.last = current
	w.revision++
	return true, w.revision, nil
}

// Handlers receive the results of Run.
type Handlers struct {
	// Change is called for each content change with the new revision.
	Change func(revision uint64)
	// Error is called once for the first failed check. Later errors are
	// ignored until a check succeeds again.
	Error func(error)
	// PollingOnly, when set, is called when file system notifications are
	// not available. Run then checks only at the interval.
	PollingOnly func(error)
}

// Run checks the file at once, then after notifications and at the
// interval until ctx ends. The first check finds a change since New.
func (w *Watcher) Run(ctx context.Context, handlers Handlers) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	settle := time.NewTimer(w.debounce)
	settle.Stop()
	defer settle.Stop()

	var events <-chan fsnotify.Event
	var notifyErrors <-chan error
	if watcher, err := w.notify(); err != nil {
		if handlers.PollingOnly != nil {
			handlers.PollingOnly(err)
		}
	} else {
		defer watcher.Close()
		events, notifyErrors = watcher.Events, watcher.Errors
	}

	failing := false
	check := func() {
		changed, revision, err := w.Check()
		if err != nil {
			if !failing {
				failing = true
				handlers.Error(err)
			}
			return
		}
		failing = false
		if changed {
			handlers.Change(revision)
		}
	}
	name := filepath.Base(w.path)
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		case <-settle.C:
			check()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Base(event.Name) == name {
				settle.Reset(w.debounce)
			}
		case _, ok := <-notifyErrors:
			if !ok {
				notifyErrors = nil
				continue
			}
			// An error (for example a full queue) can hide a change.
			settle.Reset(w.debounce)
		}
	}
}

// notify watches the directory of the file, so a file that the agent
// replaces with a rename stays watched.
func (w *Watcher) notify() (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(filepath.Dir(w.path)); err != nil {
		watcher.Close()
		return nil, err
	}
	return watcher, nil
}

// read returns the state of the file. It hashes the content only when the
// modification time or the size differs from previous.
func (w *Watcher) read(previous state) (state, error) {
	info, err := os.Stat(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	current := state{exists: true, modTime: info.ModTime(), size: info.Size()}
	if previous.exists && previous.modTime.Equal(current.modTime) && previous.size == current.size {
		current.digest = previous.digest
		return current, nil
	}
	f, err := os.Open(w.path)
	if err != nil {
		return state{}, err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return state{}, err
	}
	copy(current.digest[:], hash.Sum(nil))
	return current, nil
}
