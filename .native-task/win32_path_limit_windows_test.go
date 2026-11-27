// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

func TestWriteExpiryWindowsPathLimitQualification(t *testing.T) {
	part := strings.Repeat("d", 90)
	dir := filepath.Join(t.TempDir(), part, part, part)
	if err := os.MkdirAll(extendedPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	path := tempPath(filepath.Join(dir, "out.txt"), "qualification")
	units := len(utf16.Encode([]rune(path)))
	if units <= 260 || strings.HasPrefix(path, `\\?\`) || !filepath.IsAbs(path) {
		t.Fatalf("expected an ordinary absolute staging path longer than 260 UTF-16 code units, got %q", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("Windows guest is unqualified: os.OpenFile on ordinary long staging path: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err == nil {
		syscall.CloseHandle(h)
	}
	if !errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE) {
		t.Fatalf("Windows guest is unqualified: raw CreateFileW on ordinary long staging path returned %v, want ERROR_FILENAME_EXCED_RANGE", err)
	}
	t.Logf("qualified: os.OpenFile accepted an ordinary path of %d UTF-16 code units and raw CreateFileW returned ERROR_FILENAME_EXCED_RANGE", units)
}
