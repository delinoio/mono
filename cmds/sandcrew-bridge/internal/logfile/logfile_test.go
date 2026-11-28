// SPDX-License-Identifier: Apache-2.0

package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatesWithOnePreviousFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.log")
	f, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n"} {
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := os.ReadFile(path)
	previous, _ := os.ReadFile(path + ".1")
	if string(current) != "cccc\ndddd\n" || string(previous) != "aaaa\nbbbb\n" {
		t.Fatalf("current %q, previous %q", current, previous)
	}
	matches, _ := filepath.Glob(path + "*")
	if len(matches) != 2 {
		t.Fatalf("files %v, want bridge.log and bridge.log.1", matches)
	}
	if strings.Contains(strings.Join(matches, ","), ".2") {
		t.Fatal("more than one previous file")
	}
}
