// SPDX-License-Identifier: Apache-2.0

package inbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
)

func TestNotifyReportsRenamedFiles(t *testing.T) {
	stateDir := t.TempDir()
	box, err := New(filepath.Join(stateDir, "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	notifier, err := box.Notify()
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	defer func() {
		if err := notifier.Close(); err != nil {
			t.Error(err)
		}
	}()

	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	// WriteDevSessionFile writes a temporary file and renames it into the
	// inbox.
	tmp := filepath.Join(stateDir, "tmp")
	if err := os.WriteFile(tmp, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(box.Dir(), id+".json")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifier.C():
	case <-time.After(5 * time.Second):
		t.Fatal("no notification for the new inbox file")
	}
	items, err := box.List()
	if err != nil || len(items) != 1 || items[0].InputID != id {
		t.Fatalf("List() = %v, %v", items, err)
	}
}
