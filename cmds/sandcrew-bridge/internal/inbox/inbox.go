// SPDX-License-Identifier: Apache-2.0

// Package inbox reads the input files that the control plane writes into
// the bridge inbox directory. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#inbox.
package inbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/delinoio/mono/go/uuidv7"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// MaxFileSize limits one inbox file.
const MaxFileSize = 1 << 20

// PollInterval is the interval at which the bridge looks for new files
// when file system notifications are not available.
const PollInterval = 250 * time.Millisecond

// FallbackPollInterval is the interval at which the bridge looks for new
// files while file system notifications work. It finds a file whose
// notification was lost, for example after a full notification queue.
const FallbackPollInterval = 2 * time.Second

const fileSuffix = ".json"

// Item is one inbox file.
type Item struct {
	// InputID is the file name without ".json".
	InputID string
	Path    string
	// Input is the parsed input, or nil when Err is set.
	Input *sandcrewv1.BridgeInput
	// Err is set when the file is too large or does not parse, or when
	// its input_id differs from the file name.
	Err error
}

// Inbox is the inbox directory.
type Inbox struct {
	dir string
}

// New returns the inbox at dir and creates the directory.
func New(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("inbox: create: %w", err)
	}
	return &Inbox{dir: dir}, nil
}

// Dir returns the inbox directory.
func (i *Inbox) Dir() string { return i.dir }

// Notifier reports possible new files in the inbox directory.
type Notifier struct {
	watcher *fsnotify.Watcher
	c       chan struct{}
	done    chan struct{}
}

// Notify starts file system notifications for the inbox directory. The
// caller closes the notifier. An error means that the platform or the file
// system gives no notifications; the caller then polls with PollInterval.
func (i *Inbox) Notify() (*Notifier, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("inbox: notifications: %w", err)
	}
	if err := watcher.Add(i.dir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("inbox: notifications: %w", err)
	}
	n := &Notifier{watcher: watcher, c: make(chan struct{}, 1), done: make(chan struct{})}
	go n.run()
	return n, nil
}

// C receives a value after a file was created or written in the inbox.
// Values are merged: one value can stand for several files.
func (n *Notifier) C() <-chan struct{} { return n.c }

// Close stops the notifications.
func (n *Notifier) Close() error {
	err := n.watcher.Close()
	<-n.done
	return err
}

func (n *Notifier) run() {
	defer close(n.done)
	for {
		select {
		case event, ok := <-n.watcher.Events:
			if !ok {
				return
			}
			// WriteDevSessionFile renames the finished file into the
			// inbox, which is a create event. Removals are the bridge's
			// own deletions.
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				n.signal()
			}
		case _, ok := <-n.watcher.Errors:
			if !ok {
				return
			}
			// An error (for example a full queue) can hide a new file, so
			// the bridge reads the directory again.
			n.signal()
		}
	}
}

func (n *Notifier) signal() {
	select {
	case n.c <- struct{}{}:
	default:
	}
}

// List returns the inbox files in input_id order (UUID v7 time order).
// Files whose names are not `<uuid v7>.json` and directories are ignored.
func (i *Inbox) List() ([]Item, error) {
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		return nil, fmt.Errorf("inbox: list: %w", err)
	}
	var items []Item
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, fileSuffix)
		parsed, err := uuidv7.Parse(id)
		// The canonical text form keeps the lexical order equal to the
		// time order.
		if err != nil || parsed.String() != id {
			continue
		}
		items = append(items, Item{InputID: id, Path: filepath.Join(i.dir, name)})
	}
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.InputID, b.InputID) })
	return items, nil
}

// Read parses the file of item.
func Read(item Item) Item {
	f, err := os.Open(item.Path)
	if err != nil {
		item.Err = err
		return item
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		item.Err = err
		return item
	}
	if len(data) > MaxFileSize {
		item.Err = fmt.Errorf("inbox: file is larger than %d bytes", MaxFileSize)
		return item
	}
	var input sandcrewv1.BridgeInput
	// Unknown fields are discarded, so an input case of a later protocol
	// minor version parses without a case and is rejected as UNSUPPORTED.
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &input); err != nil {
		item.Err = fmt.Errorf("inbox: parse: %w", err)
		return item
	}
	if input.GetInputId() != item.InputID {
		item.Err = errors.New("inbox: input_id differs from the file name")
		return item
	}
	item.Input = &input
	return item
}

// Remove deletes the file of item. A missing file is not an error.
func Remove(item Item) error {
	if err := os.Remove(item.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
