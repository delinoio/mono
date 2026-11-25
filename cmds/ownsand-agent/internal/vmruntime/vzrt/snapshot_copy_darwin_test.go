// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type cancelStreamedCopy struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

func (c cancelStreamedCopy) Err() error {
	if st, err := os.Stat(c.path); err == nil && st.Size() > 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestVirtualizationStreamedCopyStopsWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	data := bytes.Repeat([]byte("disk image\n"), 1<<17)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := cancelStreamedCopy{Context: base, cancel: cancel, path: dst}
	if err := copyFileStream(ctx, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("streamed copy: %v, want context cancellation", err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() == 0 || st.Size() >= int64(len(data)) {
		t.Fatalf("expected a partial copy for the caller to remove, got %v, %v", st, err)
	}

	base, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := copyFileStream(base, src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uncanceled copy: %d bytes, %v", len(got), err)
	}
}
