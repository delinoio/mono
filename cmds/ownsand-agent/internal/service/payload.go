// SPDX-License-Identifier: FSL-1.1-ALv2

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Payloads holds the Windows service's retained executables and the working
// executable that the agent updates. The service launcher lives outside them.
type Payloads struct {
	Dir   string
	Write func(path string, data []byte, mode fs.FileMode) error
}

type Payload struct {
	Path     string
	Digest   string
	previous string
}

const payloadSelection = "selected"

type payloadSelectionRecord struct {
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
}

// Initialize retains the trusted executable supplied by the operator before
// the installer registers the launcher with the service control manager.
func (p Payloads) Initialize(data []byte) error {
	_, err := p.selectPayload(data, "")
	return err
}

func (p Payloads) retained(digest string) string {
	return filepath.Join(p.Dir, "payload-"+digest+".exe")
}

func (p Payloads) working(digest string) string {
	return filepath.Join(p.Dir, "run-"+digest+".exe")
}

func (p Payloads) selectPayload(data []byte, previous string) (Payload, error) {
	digest := digestBytes(data)
	path := p.working(digest)
	if err := p.Write(p.retained(digest), data, 0o700); err != nil {
		return Payload{}, fmt.Errorf("retain the service payload: %w", err)
	}
	if err := p.Write(path, data, 0o700); err != nil {
		return Payload{}, fmt.Errorf("prepare the service payload: %w", err)
	}
	record, err := json.Marshal(payloadSelectionRecord{Current: digest, Previous: previous})
	if err != nil {
		return Payload{}, err
	}
	if err := p.Write(filepath.Join(p.Dir, payloadSelection), record, 0o600); err != nil {
		return Payload{}, fmt.Errorf("select the service payload: %w", err)
	}
	return Payload{Path: path, Digest: digest, previous: previous}, nil
}

// Recover selects the version for this start. The updater publishes verified
// bytes with its final rename onto Path. If it stops between renames, Path is
// absent and the launcher restores it from an independent retained copy.
func (p Payloads) Recover() (Payload, error) {
	raw, err := os.ReadFile(filepath.Join(p.Dir, payloadSelection))
	if err != nil {
		return Payload{}, fmt.Errorf("read the service payload selection: %w", err)
	}
	var selection payloadSelectionRecord
	if err := json.Unmarshal(raw, &selection); err != nil {
		return Payload{}, fmt.Errorf("decode the service payload selection: %w", err)
	}
	digest := selection.Current
	if !validPayloadDigest(digest) || (selection.Previous != "" && !validPayloadDigest(selection.Previous)) {
		return Payload{}, errors.New("invalid service payload selection")
	}
	path := p.working(digest)
	running := Payload{Path: path, Digest: digest, previous: selection.Previous}
	changed, err := running.published()
	if err == nil && !changed {
		return running, nil
	}
	if err == nil {
		// The previous payload stays available even if the launcher is
		// interrupted while retaining and selecting the new executable.
		data, err := os.ReadFile(path)
		if err != nil {
			return Payload{}, err
		}
		return p.selectPayload(data, digest)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Payload{}, err
	}
	data, err := os.ReadFile(p.retained(digest))
	if err != nil {
		return Payload{}, fmt.Errorf("read the retained service payload: %w", err)
	}
	if digestBytes(data) != digest {
		return Payload{}, errors.New("retained service payload does not match its selection")
	}
	if err := p.Write(path, data, 0o700); err != nil {
		return Payload{}, fmt.Errorf("restore the retained service payload: %w", err)
	}
	return running, nil
}

// Prune keeps the selected executable and its predecessor. Both are kept
// even when startup fails. Files outside the launcher's versioned names stay.
func (p Payloads) Prune(selected Payload) error {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		match := oldPayloadName.FindStringSubmatch(entry.Name())
		if !entry.Type().IsRegular() || match == nil || match[1] == selected.Digest || match[1] == selected.previous {
			continue
		}
		result = errors.Join(result, os.Remove(filepath.Join(p.Dir, entry.Name())))
	}
	return result
}

var oldPayloadName = regexp.MustCompile(`^(?:payload|run)-([a-f0-9]{64})\.exe(?:\.(?:old|new))?$`)

func validPayloadDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest
}

// published reports whether the working executable contains different bytes.
// An abandoned .new or .old file does not count as publication.
func (p Payload) published() (bool, error) {
	f, err := os.Open(p.Path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) != p.Digest, nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
