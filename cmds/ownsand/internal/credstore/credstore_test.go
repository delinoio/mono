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
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
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
