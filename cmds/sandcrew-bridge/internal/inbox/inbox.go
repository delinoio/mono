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

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/delinoio/mono/go/uuidv7"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// MaxFileSize limits one inbox file.
const MaxFileSize = 1 << 20

// PollInterval is the interval at which the bridge looks for new files.
const PollInterval = 250 * time.Millisecond

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
