// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVHDGeometry(t *testing.T) {
	for _, tc := range []struct {
		size    int64
		c       uint16
		h, s    uint8
		comment string
	}{
		{10 << 20, 301, 4, 17, "small disk keeps 17 sectors per track"},
		{512 << 20, 1040, 16, 63, "base image size"},
		{2 << 30, 4161, 16, 63, "2 GiB, the geometry Hyper-V reports"},
		{127 << 30, 65278, 16, 255, "large disk uses 255 sectors per track"},
		{200 << 30, 65535, 16, 255, "very large disk is clamped"},
	} {
		c, h, s := vhdGeometry(tc.size)
		if c != tc.c || h != tc.h || s != tc.s {
			t.Errorf("%s: geometry(%d) = %d/%d/%d, want %d/%d/%d", tc.comment, tc.size, c, h, s, tc.c, tc.h, tc.s)
		}
	}
}

func TestVHDFooter(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	id := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	f := newVHDFooter(2<<30, now, id)
	if len(f) != 512 || string(f[:8]) != "conectix" {
		t.Fatalf("footer %x", f[:16])
	}
	be := binary.BigEndian
	if be.Uint32(f[8:]) != 2 || be.Uint32(f[12:]) != 0x00010000 || be.Uint64(f[16:]) != 0xFFFFFFFFFFFFFFFF {
		t.Fatalf("features, version or data offset wrong: %x", f[8:24])
	}
	if got := be.Uint32(f[24:]); got != uint32(now.Sub(vhdEpoch)/time.Second) {
		t.Fatalf("time stamp %d", got)
	}
	if be.Uint64(f[40:]) != 2<<30 || be.Uint64(f[48:]) != 2<<30 {
		t.Fatalf("sizes %x", f[40:56])
	}
	if be.Uint16(f[56:]) != 4161 || f[58] != 16 || f[59] != 63 {
		t.Fatalf("geometry %x", f[56:60])
	}
	if be.Uint32(f[60:]) != 2 {
		t.Fatalf("disk type %d", be.Uint32(f[60:]))
	}
	if !bytes.Equal(f[68:84], id[:]) || f[84] != 0 || !bytes.Equal(f[85:], make([]byte, 427)) {
		t.Fatal("unique ID, saved state or reserved bytes wrong")
	}
	// The checksum is the one's complement of the byte sum without it.
	var sum uint32
	for i, b := range f {
		if i < 64 || i >= 68 {
			sum += uint32(b)
		}
	}
	if be.Uint32(f[64:]) != ^sum {
		t.Fatalf("checksum %#x, want %#x", be.Uint32(f[64:]), ^sum)
	}
	if size, err := parseVHDFooter(f); err != nil || size != 2<<30 {
		t.Fatalf("parse %d %v", size, err)
	}
	bad := bytes.Clone(f)
	bad[100] = 1
	if _, err := parseVHDFooter(bad); err == nil {
		t.Fatal("parse accepted a footer with a wrong checksum")
	}
	if _, err := parseVHDFooter(make([]byte, 512)); err == nil {
		t.Fatal("parse accepted a footer without the cookie")
	}
}

func TestMakeFixedVHD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.vhd")
	raw := bytes.Repeat([]byte("ext4"), (3<<20)/4)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	footerOf := func() []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b[:len(raw)], raw) {
			t.Fatal("the disk data changed")
		}
		return b[len(b)-512:]
	}
	// A smaller requested size keeps the data, rounded up to 1 MiB.
	size, err := makeFixedVHD(path, 1<<20, false)
	if err != nil || size != 3<<20 {
		t.Fatalf("make %d %v", size, err)
	}
	if st, _ := os.Stat(path); st.Size() != 3<<20+512 {
		t.Fatalf("file size %d", st.Size())
	}
	first := footerOf()
	// A snapshot copy loses its footer, grows and gets a new unique ID.
	size, err = makeFixedVHD(path, 8<<20+1, true)
	if err != nil || size != 9<<20 {
		t.Fatalf("regrow %d %v", size, err)
	}
	if st, _ := os.Stat(path); st.Size() != 9<<20+512 {
		t.Fatalf("file size %d", st.Size())
	}
	second := footerOf()
	if n, err := parseVHDFooter(second); err != nil || n != 9<<20 {
		t.Fatalf("footer %d %v", n, err)
	}
	if bytes.Equal(first[68:84], second[68:84]) {
		t.Fatal("the regrown disk kept its unique ID")
	}
	disk, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var oldFooter, zeros [vhdFooterSize]byte
	_, err = disk.ReadAt(oldFooter[:], int64(len(raw)))
	disk.Close()
	if err != nil {
		t.Fatalf("read old footer range: %v", err)
	}
	if oldFooter != zeros {
		t.Fatal("the old footer range is not zero")
	}
	// A raw image without a footer is rejected where a VHD is expected.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := makeFixedVHD(path, 0, true); err == nil {
		t.Fatal("a raw image passed as a VHD")
	}
}

func TestMakeFixedVHDLinuxBaseCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.vhd")
	for _, want := range []int64{2 << 30, 3 << 30} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 2<<30); err != nil {
			t.Fatal(err)
		}
		got, err := makeFixedVHD(path, want, false)
		if err != nil || got != want {
			t.Fatalf("makeFixedVHD = %d, %v, want %d bytes", got, err, want)
		}
		disk, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var footer [vhdFooterSize]byte
		_, err = disk.ReadAt(footer[:], want)
		disk.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := parseVHDFooter(footer[:]); err != nil || got != want {
			t.Fatalf("footer size = %d, %v, want %d bytes", got, err, want)
		}
	}
}
