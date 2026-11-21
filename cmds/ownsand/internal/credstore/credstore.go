// SPDX-License-Identifier: Apache-2.0

// Package credstore stores the Logto tokens of the ownsand CLI in a file in
// the user configuration directory that only the user can read: mode 0600
// (directory 0700) on Linux and macOS, a protected user-only ACL on
// Windows. OS keychain storage is not used; see
// docs/cmds-ownsand-cli-foundation.md.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileName is the name of the credential file.
const FileName = "credentials.json"

// Credentials are the stored Logto tokens. They are secrets: never print or
// log them.
type Credentials struct {
	// LogtoEndpoint and AppID identify the tenant and the application that
	// issued the tokens, so a changed configuration does not reuse them.
	LogtoEndpoint string `json:"logto_endpoint"`
	AppID         string `json:"app_id"`
	Resource      string `json:"resource"`
	// RefreshToken rotates on every refresh; the newest one is stored.
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token,omitempty"`
	// AccessTokenExpiry is the absolute expiry of AccessToken.
	AccessTokenExpiry time.Time `json:"access_token_expiry,omitzero"`
}

// ErrNotFound reports that no credentials are stored.
var ErrNotFound = errors.New("credstore: no stored credentials")

// Store is a credential file in one directory.
type Store struct {
	Dir string
}

// DefaultDir returns <user config dir>/ownsand.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("credstore: user configuration directory: %w", err)
	}
	return filepath.Join(base, "ownsand"), nil
}

// Path returns the credential file path.
func (s Store) Path() string { return filepath.Join(s.Dir, FileName) }

// Load reads the stored credentials.
func (s Store) Load() (*Credentials, error) {
	b, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("credstore: read: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		// Never include the file content: it holds tokens.
		return nil, errors.New("credstore: the credential file is not valid; run ownsand logout and ownsand login")
	}
	return &c, nil
}

// Save writes the credentials atomically to a user-only file.
func (s Store) Save(c *Credentials) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("credstore: create directory: %w", err)
	}
	if err := restrictDir(s.Dir); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("credstore: encode: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("credstore: create file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// Restrict the file before it holds a secret.
	if err := restrictFile(tmp, tmpName); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credstore: close: %w", err)
	}
	if err := os.Rename(tmpName, s.Path()); err != nil {
		return fmt.Errorf("credstore: replace: %w", err)
	}
	return nil
}

// Delete removes the stored credentials. It succeeds when none exist.
func (s Store) Delete() error {
	err := os.Remove(s.Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credstore: delete: %w", err)
	}
	return nil
}
