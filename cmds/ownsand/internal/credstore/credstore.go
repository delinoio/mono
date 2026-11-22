// SPDX-License-Identifier: Apache-2.0

// Package credstore stores the Logto tokens of the ownsand CLI in a file in
// the user configuration directory that only the user can read: mode 0600
// (directory 0700) on Linux and macOS, a protected user-only ACL on
// Windows. OS keychain storage is not used; see
// docs/cmds-ownsand-cli-foundation.md.
package credstore

import (
	"crypto/rand"
	"encoding/hex"
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
	// SessionID identifies one sign-in. Every login stores a new one and
	// refreshes keep it, so a refresh can tell that its session was replaced
	// by a newer login. Files written before the field existed have none;
	// they all count as one session.
	SessionID string `json:"session_id,omitempty"`
	// RefreshToken rotates on every refresh; the newest one is stored.
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token,omitempty"`
	// AccessTokenExpiry is the absolute expiry of AccessToken.
	AccessTokenExpiry time.Time `json:"access_token_expiry,omitzero"`
}

// ErrNotFound reports that no credentials are stored.
var ErrNotFound = errors.New("credstore: no stored credentials")

// ErrReplaced reports that the stored credentials belong to another session
// than the caller expected, for example because the user signed in again.
var ErrReplaced = errors.New("credstore: the stored credentials belong to another session")

// NewSessionID returns a random identifier for a new sign-in.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("credstore: session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

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

// Save writes the credentials atomically to a user-only file. It holds the
// store lock, so it does not interleave with a Delete or SaveIfSession of
// another process.
func (s Store) Save(c *Credentials) error {
	unlock, err := s.lockDir()
	if err != nil {
		return err
	}
	defer unlock()
	return s.write(c)
}

// SaveIfSession writes c only when the stored credentials still belong to
// the session sessionID, which the caller read before a slow operation such
// as a token refresh. It returns ErrNotFound when the credentials were
// removed (the user signed out) and ErrReplaced when another session replaced
// them (the user signed in again); in both cases it writes nothing. The
// comparison and the write happen under the store lock.
func (s Store) SaveIfSession(sessionID string, c *Credentials) error {
	unlock, err := s.lockDir()
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := s.Load()
	if err != nil {
		return err
	}
	if cur.SessionID != sessionID {
		return ErrReplaced
	}
	return s.write(c)
}

// lockDir creates the restricted store directory and takes the store lock.
func (s Store) lockDir() (func(), error) {
	if err := s.prepareDir(); err != nil {
		return nil, err
	}
	return s.lock()
}

func (s Store) prepareDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("credstore: create directory: %w", err)
	}
	return restrictDir(s.Dir)
}

// write replaces the credential file atomically. The caller holds the lock.
func (s Store) write(c *Credentials) error {
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
	if err := replaceFile(tmpName, s.Path()); err != nil {
		return fmt.Errorf("credstore: replace: %w", err)
	}
	return nil
}

// Delete removes the stored credentials under the store lock. It succeeds
// when none exist. The lock file stays: removing it would let two processes
// lock different files.
func (s Store) Delete() error {
	if _, err := os.Stat(s.Dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := s.lockDir()
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(s.Path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credstore: delete: %w", err)
	}
	return nil
}
