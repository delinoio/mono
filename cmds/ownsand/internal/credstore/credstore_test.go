// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadDelete(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "nested", "ownsand")}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := &Credentials{LogtoEndpoint: "https://auth.example", AppID: "app", RefreshToken: "r", AccessToken: "a", AccessTokenExpiry: time.Unix(1700000000, 0).UTC()}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || *got != *want {
		t.Fatalf("load: %+v %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(s.Path())
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	// Only the credential file and the lock file remain.
	if len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
	for _, e := range entries {
		if e.Name() != FileName && e.Name() != LockFileName {
			t.Fatalf("unexpected file %q", e.Name())
		}
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal("a second delete must succeed")
	}
}

func TestCorruptFileErrorHidesContent(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := os.WriteFile(s.Path(), []byte("not json secret-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error %v", err)
	}
}

func TestSaveIfSession(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	base := &Credentials{SessionID: "one", RefreshToken: "r1"}
	if err := s.SaveIfSession("one", base); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing file must report ErrNotFound, got %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveIfSession recreated the file: %v", err)
	}
	if err := s.Save(base); err != nil {
		t.Fatal(err)
	}
	// The same session stores its rotation.
	if err := s.SaveIfSession("one", &Credentials{SessionID: "one", RefreshToken: "r2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(); got.RefreshToken != "r2" {
		t.Fatalf("rotation not stored: %+v", got)
	}
	// Another session replaced it.
	if err := s.Save(&Credentials{SessionID: "two", RefreshToken: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIfSession("one", &Credentials{SessionID: "one", RefreshToken: "r3"}); !errors.Is(err, ErrReplaced) {
		t.Fatalf("want ErrReplaced, got %v", err)
	}
	if got, _ := s.Load(); got.SessionID != "two" || got.RefreshToken != "new" {
		t.Fatalf("an older session overwrote the new one: %+v", got)
	}
}

func TestSaveIfCurrent(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.SaveIfCurrent("one", "r1", &Credentials{SessionID: "one", RefreshToken: "r2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing file must report ErrNotFound, got %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveIfCurrent recreated the file: %v", err)
	}
	if err := s.Save(&Credentials{SessionID: "one", RefreshToken: "r1"}); err != nil {
		t.Fatal(err)
	}
	// The generation the caller refreshed stores its rotation.
	if err := s.SaveIfCurrent("one", "r1", &Credentials{SessionID: "one", RefreshToken: "r2"}); err != nil {
		t.Fatal(err)
	}
	// A second result derived from r1 is stale: r2 was stored meanwhile.
	if err := s.SaveIfCurrent("one", "r1", &Credentials{SessionID: "one", RefreshToken: "r2-late"}); !errors.Is(err, ErrRotated) {
		t.Fatalf("want ErrRotated, got %v", err)
	}
	if got, _ := s.Load(); got.RefreshToken != "r2" {
		t.Fatalf("a stale result overwrote the newer rotation: %+v", got)
	}
	// Another session replaced it.
	if err := s.Save(&Credentials{SessionID: "two", RefreshToken: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIfCurrent("one", "r1", &Credentials{SessionID: "one", RefreshToken: "r3"}); !errors.Is(err, ErrReplaced) {
		t.Fatalf("want ErrReplaced, got %v", err)
	}
	if got, _ := s.Load(); got.SessionID != "two" {
		t.Fatalf("an older session overwrote the new one: %+v", got)
	}
}

func TestFileWithoutSessionIDStillLoads(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	legacy := `{"logto_endpoint":"https://auth.example","app_id":"app","resource":"r","refresh_token":"old","access_token":"a"}`
	if err := os.WriteFile(s.Path(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.RefreshToken != "old" || got.SessionID != "" {
		t.Fatalf("load: %+v %v", got, err)
	}
	// A legacy session still stores its rotation, and SessionID stays
	// out of the file while it is empty.
	if err := s.SaveIfSession("", &Credentials{RefreshToken: "rotated"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.Path())
	if strings.Contains(string(b), "session_id") {
		t.Fatalf("empty session id written: %s", b)
	}
	// A newer login replaces a legacy file for the legacy holder.
	if err := s.Save(&Credentials{SessionID: "fresh", RefreshToken: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIfSession("", &Credentials{RefreshToken: "stale"}); !errors.Is(err, ErrReplaced) {
		t.Fatalf("want ErrReplaced, got %v", err)
	}
}

func TestDeleteWithoutDirectoryCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if err := (Store{Dir: dir}).Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete created the directory: %v", err)
	}
}

func TestLockExcludesOtherHolders(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	// A second open file description of the lock file must not get it.
	f, err := os.OpenFile(filepath.Join(s.Dir, LockFileName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, err := tryLockFile(f); err != nil || ok {
		t.Fatalf("a held lock was granted again: %v %v", ok, err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.Delete() }()
	select {
	case err := <-deleted:
		t.Fatalf("Delete ran while the lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file still present: %v", err)
	}
}

// TestConcurrentWritersKeepOneSession races a refresh publication against a
// replacing login and a logout; whichever order wins, an older session must
// never be on disk afterwards.
func TestConcurrentSaveIfSessionNeverOverwritesNewerSession(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := Store{Dir: t.TempDir()}
		if err := s.Save(&Credentials{SessionID: "old", RefreshToken: "r"}); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = s.SaveIfSession("old", &Credentials{SessionID: "old", RefreshToken: "rot"})
		}()
		if err := s.Save(&Credentials{SessionID: "new", RefreshToken: "n"}); err != nil {
			t.Fatal(err)
		}
		<-done
		got, err := s.Load()
		if err != nil || got.SessionID != "new" {
			t.Fatalf("iteration %d: %+v %v", i, got, err)
		}
	}
}
