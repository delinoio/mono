// SPDX-License-Identifier: Apache-2.0

// Package credwatch detects changes of a credential file (the Codex
// `auth.json`), which the agent rewrites when it refreshes its token. It
// compares the modification time and the SHA-256 of the content and never
// exposes the content. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#credential-updates.
package credwatch

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"time"
)

// Interval is the check interval.
const Interval = 10 * time.Second

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
	last     state
	revision uint64
}

// New returns a watcher of path and records its current state, so only
// later changes count.
func New(path string, interval time.Duration) (*Watcher, error) {
	if interval <= 0 {
		interval = Interval
	}
	w := &Watcher{path: path, interval: interval}
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

// Run checks the file every interval until ctx ends. It calls onChange for
// each change and onError once for the first error; later errors are
// ignored until a check succeeds again.
func (w *Watcher) Run(ctx context.Context, onChange func(revision uint64), onError func(error)) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		changed, revision, err := w.Check()
		if err != nil {
			if !failing {
				failing = true
				onError(err)
			}
			continue
		}
		failing = false
		if changed {
			onChange(revision)
		}
	}
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
