//go:build linux

package firecracker

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCloneFileIsSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, _ := os.Create(src)
	_ = f.Truncate(64 << 20)
	_, _ = f.WriteAt([]byte("data"), 10<<20)
	_ = f.Close()
	dst := filepath.Join(dir, "dst")
	if err := cloneFile(src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("copy differs")
	}
	var st syscall.Stat_t
	_ = syscall.Stat(dst, &st)
	if st.Blocks*512 > 8<<20 {
		t.Fatalf("copy is not sparse: %d bytes allocated", st.Blocks*512)
	}
}
