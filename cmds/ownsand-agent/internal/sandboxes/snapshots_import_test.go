// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type cancelAfterImportRecord struct {
	context.Context
	cancel context.CancelFunc
	dir    string
}

func (ctx cancelAfterImportRecord) Err() error {
	// Watch both names so that the test also cancels at this check if the
	// import writes directly to the final record.
	for _, name := range []string{".record.pending", "record.json"} {
		if _, err := os.Stat(filepath.Join(ctx.dir, name)); err == nil {
			ctx.cancel()
			break
		}
	}
	return ctx.Context.Err()
}

func TestCanceledImportRecordStaysUnpublishedAfterRestart(t *testing.T) {
	e := newEnv(t)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := e.st.SnapshotDir(id)
	if err != nil {
		t.Fatal(err)
	}
	entry := &ownsandv1.SnapshotStateEntry{SnapshotId: id}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Leave the record in place to model cleanup that lost the content
	// before it could remove the record.
	got, err := e.m.AddImportedSnapshot(cancelAfterImportRecord{ctx, cancel, dir}, entry)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled import = (%v, %v), want (nil, context.Canceled)", got, err)
	}
	if e.m.HasSnapshot(id) {
		t.Fatal("the canceled import has a manager entry")
	}

	e.restart()
	if _, _, ok := e.m.SnapshotContent(id); ok {
		t.Error("restart reloaded the canceled import as ready")
	}
	for _, snapshot := range e.m.StateReport(0, nil).GetSnapshots() {
		if snapshot.GetSnapshotId() == id {
			t.Error("the first state report contains the canceled import")
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart did not remove the unpublished snapshot directory: %v", err)
	}

	content := filepath.Join(dir, contentDir)
	if err := os.MkdirAll(content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "sample"), []byte("retry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := e.m.AddImportedSnapshot(context.Background(), entry); err != nil || got.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("retry import = (%v, %v), want READY", got, err)
	}

	e.restart()
	path, got, ok := e.m.SnapshotContent(id)
	if !ok || got.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		t.Fatalf("retried import after restart = (%v, %v), want READY", got, ok)
	}
	if data, err := os.ReadFile(filepath.Join(path, "sample")); err != nil || string(data) != "retry" {
		t.Fatalf("retried snapshot content = %q, %v", data, err)
	}
}
