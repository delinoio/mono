// SPDX-License-Identifier: FSL-1.1-ALv2

package gpt

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guidBytes encodes a GUID string in the mixed-endian GPT layout: the
// first three fields are little-endian.
func guidBytes(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		t.Fatalf("bad GUID %q", s)
	}
	out := []byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6]}
	return append(out, raw[8:]...)
}

const (
	appleAPFS = "7C3457EF-0000-11AA-AA11-00306543ECAC"
	appleISC  = "69646961-6700-11AA-AA11-00306543ECAC"
)

// newDisk writes a disk image like the one that Virtualization.framework
// installs: an ISC container, the system container and a recovery container
// at the end.
func newDisk(t *testing.T, sectors uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(sectors) * SectorSize); err != nil {
		t.Fatal(err)
	}
	mbr := make([]byte, SectorSize)
	mbr[446+4] = 0xEE
	binary.LittleEndian.PutUint32(mbr[446+8:], 1)
	binary.LittleEndian.PutUint32(mbr[446+12:], uint32(sectors-1))
	mbr[510], mbr[511] = 0x55, 0xAA
	if _, err := f.WriteAt(mbr, 0); err != nil {
		t.Fatal(err)
	}
	tbl := &Table{sectors: sectors, entries: make([]byte, 128*128)}
	copy(tbl.hdr.Signature[:], signature)
	tbl.hdr.Revision, tbl.hdr.HeaderSize = 0x00010000, headerSize
	tbl.hdr.FirstUsableLBA, tbl.hdr.EntriesLBA = 34, 2
	tbl.hdr.NumEntries, tbl.hdr.EntrySize = 128, 128
	add := func(i int, typ string, first, last uint64) {
		e := tbl.entries[i*128:]
		copy(e[0:16], guidBytes(t, typ))
		e[16] = byte(i + 1) // unique partition GUID
		binary.LittleEndian.PutUint64(e[32:40], first)
		binary.LittleEndian.PutUint64(e[40:48], last)
	}
	add(0, appleISC, 40, 1000)
	add(1, appleAPFS, 1001, sectors-2000)
	add(2, AppleAPFSRecovery, sectors-1999, sectors-40)
	if err := tbl.Write(f); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) *Table {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	tbl, err := Read(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	// The backup table must be valid too.
	var backup header
	buf := make([]byte, headerSize)
	if _, err := f.ReadAt(buf, int64(tbl.hdr.BackupLBA)*SectorSize); err != nil {
		t.Fatal(err)
	}
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &backup); err != nil {
		t.Fatal(err)
	}
	if string(backup.Signature[:]) != signature || backup.CurrentLBA != tbl.hdr.BackupLBA || backup.BackupLBA != 1 ||
		backup.EntriesCRC != tbl.hdr.EntriesCRC || backup.LastUsableLBA != tbl.hdr.LastUsableLBA {
		t.Fatalf("backup header %+v, primary %+v", backup, tbl.hdr)
	}
	return tbl
}

func TestRemoveTrailingAndFit(t *testing.T) {
	const sectors = 1 << 16
	path := newDisk(t, sectors)
	if got := read(t, path).Partitions(); len(got) != 3 || got[2].TypeGUID != AppleAPFSRecovery || got[1].TypeGUID != appleAPFS {
		t.Fatalf("partitions %+v", got)
	}
	n, err := RemoveTrailing(path, AppleAPFSRecovery)
	if err != nil || n != 1 {
		t.Fatalf("RemoveTrailing = %d %v", n, err)
	}
	if got := read(t, path).Partitions(); len(got) != 2 {
		t.Fatalf("partitions after removal %+v", got)
	}
	// A second call finds nothing to remove.
	if n, err := RemoveTrailing(path, AppleAPFSRecovery); err != nil || n != 0 {
		t.Fatalf("second RemoveTrailing = %d %v", n, err)
	}

	if err := os.Truncate(path, 2*sectors*SectorSize); err != nil {
		t.Fatal(err)
	}
	if err := Fit(path); err != nil {
		t.Fatal(err)
	}
	tbl := read(t, path)
	if tbl.hdr.BackupLBA != 2*sectors-1 || tbl.hdr.LastUsableLBA != 2*sectors-1-32-1 {
		t.Fatalf("header after Fit %+v", tbl.hdr)
	}
	// The old backup header is cleared and the protective MBR covers the
	// grown disk.
	f, _ := os.Open(path)
	defer f.Close()
	old := make([]byte, 8)
	f.ReadAt(old, (sectors-1)*SectorSize)
	if string(old) == signature {
		t.Fatal("the old backup header is still on the disk")
	}
	mbr := make([]byte, SectorSize)
	f.ReadAt(mbr, 0)
	if got := binary.LittleEndian.Uint32(mbr[446+12:]); got != 2*sectors-1 {
		t.Fatalf("protective MBR size %d", got)
	}
	if err := Fit(path); err != nil {
		t.Fatal(err)
	}
}

// diskEntries reads the entry array that the header at sector headerLBA
// points to, straight from the bytes of the disk, and returns it with the
// checksum that the header records. It parses the header fields at their
// fixed offsets, independent of the package's own decoding.
func diskEntries(t *testing.T, f *os.File, headerLBA uint64) (entries []byte, headerCRC uint32) {
	t.Helper()
	hdr := make([]byte, SectorSize)
	if _, err := f.ReadAt(hdr, int64(headerLBA)*SectorSize); err != nil {
		t.Fatal(err)
	}
	if string(hdr[:8]) != signature {
		t.Fatalf("no GPT header at sector %d", headerLBA)
	}
	lba := binary.LittleEndian.Uint64(hdr[72:80])
	n := binary.LittleEndian.Uint32(hdr[80:84])
	size := binary.LittleEndian.Uint32(hdr[84:88])
	entries = make([]byte, int(n)*int(size))
	if _, err := f.ReadAt(entries, int64(lba)*SectorSize); err != nil {
		t.Fatal(err)
	}
	return entries, binary.LittleEndian.Uint32(hdr[88:92])
}

// TestFitKeepsBothEntryArrays grows the disk by amounts around the length of
// the entry array (32 sectors) and checks, from the bytes of the disk, that
// both entry arrays match the checksum of their header and keep every used
// partition. A growth of at most the array length puts the old backup header
// inside the new backup entry array.
func TestFitKeepsBothEntryArrays(t *testing.T) {
	const sectors = 1 << 16
	for _, grow := range []uint64{1, 10, 31, 32, 33, 34, 1000, sectors} {
		t.Run(fmt.Sprintf("grow%d", grow), func(t *testing.T) {
			path := newDisk(t, sectors)
			want := read(t, path).Partitions()
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			oldEntries, _ := diskEntries(t, f, 1)
			if err := f.Truncate(int64(sectors+grow) * SectorSize); err != nil {
				t.Fatal(err)
			}
			if err := Fit(path); err != nil {
				t.Fatal(err)
			}
			last := sectors + grow - 1
			for name, lba := range map[string]uint64{"primary": 1, "backup": last} {
				entries, crc := diskEntries(t, f, lba)
				if got := crc32.ChecksumIEEE(entries); got != crc {
					t.Errorf("%s entry array checksum %08x, header %08x", name, got, crc)
				}
				if !bytes.Equal(entries, oldEntries) {
					t.Errorf("%s entry array lost or changed an entry", name)
				}
			}
			tbl := read(t, path)
			if got := tbl.Partitions(); len(got) != len(want) {
				t.Fatalf("partitions after Fit %+v, want %+v", got, want)
			}
			for i, p := range tbl.Partitions() {
				if p != want[i] {
					t.Errorf("partition %d is %+v, want %+v", i, p, want[i])
				}
			}
			// The old backup header is cleared when its sector is outside
			// the new layout. Otherwise the sector holds new table data,
			// which the checks above compare.
			if grow > 32 {
				old := make([]byte, SectorSize)
				if _, err := f.ReadAt(old, (sectors-1)*SectorSize); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(old, make([]byte, SectorSize)) {
					t.Error("the old backup header was not cleared")
				}
			}
		})
	}
}

func TestVerifyDetectsDamagedEntryArrays(t *testing.T) {
	const sectors = 1 << 14
	for _, c := range []struct {
		name string
		lba  uint64
	}{{"primary", 2}, {"backup", sectors - 1 - 32}} {
		t.Run(c.name, func(t *testing.T) {
			path := newDisk(t, sectors)
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			tbl := read(t, path)
			if err := tbl.verify(f); err != nil {
				t.Fatalf("verify of an intact table: %v", err)
			}
			if _, err := f.WriteAt(make([]byte, SectorSize), int64(c.lba)*SectorSize); err != nil {
				t.Fatal(err)
			}
			if err := tbl.verify(f); err == nil || !strings.Contains(err.Error(), c.name) {
				t.Fatalf("verify of a damaged %s entry array = %v", c.name, err)
			}
		})
	}
}

func TestReadRejectsCorruptTables(t *testing.T) {
	path := newDisk(t, 1<<14)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Flip one byte of the first entry.
	f.WriteAt([]byte{0xff}, 2*SectorSize+40)
	if _, err := Read(f, 1<<14*SectorSize); err == nil {
		t.Fatal("a table with a wrong entry checksum was accepted")
	}
	if _, err := Read(f, 1000); err == nil {
		t.Fatal("a disk size that is not a whole number of sectors was accepted")
	}
}
