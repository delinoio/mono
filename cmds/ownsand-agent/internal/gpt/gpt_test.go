// SPDX-License-Identifier: FSL-1.1-ALv2

package gpt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
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

// readSpy is an io.ReaderAt that serves the sectors in content and zeros for
// every other byte, and records each read. It allocates nothing for the
// declared entry array, so a test can describe a disk of any layout.
type readSpy struct {
	content map[int64][]byte // sector number to sector content
	reads   []spyRead
}

type spyRead struct{ off, n int64 }

func (r *readSpy) ReadAt(p []byte, off int64) (int, error) {
	r.reads = append(r.reads, spyRead{off, int64(len(p))})
	clear(p)
	for i := range p {
		if sec, ok := r.content[(off+int64(i))/SectorSize]; ok {
			p[i] = sec[(off+int64(i))%SectorSize]
		}
	}
	return len(p), nil
}

// entryReads returns the reads after the header read at sector 1.
func (r *readSpy) entryReads() []spyRead {
	var out []spyRead
	for _, rd := range r.reads {
		if rd.off != SectorSize {
			out = append(out, rd)
		}
	}
	return out
}

// headerOnlyDisk describes a disk with 16384 sectors (8 MiB) whose only
// content is a valid primary header that declares the entry array. This is
// the header of the reported crash, which declared 4096 entries of
// 0xFFFFFFF8 bytes.
func headerOnlyDisk(t *testing.T, entriesLBA uint64, numEntries, entrySize, entriesCRC uint32) (*readSpy, int64) {
	t.Helper()
	const sectors = 1 << 14
	entrySectors := (uint64(numEntries)*uint64(entrySize) + SectorSize - 1) / SectorSize
	var lastUsable uint64
	if entrySectors < sectors-2 {
		lastUsable = sectors - 2 - entrySectors
	}
	h := header{
		Revision: 0x00010000, HeaderSize: headerSize, CurrentLBA: 1, BackupLBA: sectors - 1,
		FirstUsableLBA: 2 + entrySectors, LastUsableLBA: lastUsable, EntriesLBA: entriesLBA,
		NumEntries: numEntries, EntrySize: entrySize, EntriesCRC: entriesCRC,
	}
	copy(h.Signature[:], signature)
	b, err := encodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	return &readSpy{content: map[int64][]byte{1: b}}, sectors * SectorSize
}

func TestReadBoundsEntryArrayBeforeReading(t *testing.T) {
	const sectors = 1 << 14
	// The checksum of the all-zero array that the spy serves; an unsupported
	// array is never allocated here, and its checksum is never checked.
	zeros := func(num, size uint32) uint32 {
		if total := uint64(num) * uint64(size); total <= maxEntryArrayBytes {
			return crc32.ChecksumIEEE(make([]byte, total))
		}
		return 0
	}
	cases := []struct {
		name       string
		entriesLBA uint64
		num, size  uint32
		wantErr    string // substring of the error; empty means Read succeeds
		wantRead   bool   // whether the entry array is read
		badCRC     bool   // the header holds a wrong entry array checksum
	}{
		{name: "reported crash header", entriesLBA: 2, num: 4096, size: 0xFFFFFFF8, wantErr: "gpt: unsupported entry array (4096 entries of 4294967288 bytes"},
		{name: "size that overflows 32 bits per entry count", entriesLBA: 2, num: 4096, size: 0x7FFFFFF8, wantErr: "gpt: unsupported entry array"},
		{name: "one entry size above the cap", entriesLBA: 2, num: 4096, size: 136, wantErr: "gpt: unsupported entry array (4096 entries of 136 bytes"},
		{name: "one entry above the cap", entriesLBA: 2, num: 4097, size: 128, wantErr: "gpt: unsupported entry array (4097 entries of 128 bytes)"},
		{name: "exactly the cap is read and parsed", entriesLBA: 2, num: 4096, size: 128, wantRead: true},
		{name: "exactly the cap keeps the checksum check", entriesLBA: 2, num: 4096, size: 128, badCRC: true, wantErr: "entry array checksum does not match", wantRead: true},
		{name: "extended entries within the cap", entriesLBA: 2, num: 128, size: 256, wantRead: true},
		{name: "ends at the disk boundary", entriesLBA: sectors - 64, num: 128, size: 256, wantErr: "overlaps the backup layout"},
		{name: "one sector beyond the disk", entriesLBA: sectors - 63, num: 128, size: 256, wantErr: "lies outside the disk"},
		{name: "starts at the disk end", entriesLBA: sectors, num: 128, size: 128, wantErr: "lies outside the disk"},
		{name: "starts far beyond the disk", entriesLBA: 1 << 40, num: 128, size: 128, wantErr: "lies outside the disk"},
		{name: "offset is representable but beyond the disk", entriesLBA: math.MaxInt64 / SectorSize, num: 128, size: 128, wantErr: "lies outside the disk"},
		{name: "offset overflows int64", entriesLBA: math.MaxInt64/SectorSize + 1, num: 128, size: 128, wantErr: "not a valid disk offset"},
		{name: "offset overflows uint64", entriesLBA: math.MaxUint64, num: 128, size: 128, wantErr: "not a valid disk offset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crc := zeros(c.num, c.size)
			if c.badCRC {
				crc++
			}
			spy, size := headerOnlyDisk(t, c.entriesLBA, c.num, c.size, crc)
			tbl, err := Read(spy, size)
			if c.wantErr == "" {
				if err != nil || tbl == nil {
					t.Fatalf("Read = %v, %v", tbl, err)
				}
				if got := len(tbl.Partitions()); got != 0 {
					t.Fatalf("%d partitions, want 0", got)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.HasPrefix(err.Error(), "gpt:") {
				t.Fatalf("Read error = %v, want a gpt error containing %q", err, c.wantErr)
			}
			reads := spy.entryReads()
			if !c.wantRead {
				if len(reads) != 0 {
					t.Fatalf("a rejected array was read: %v", reads)
				}
				return
			}
			want := spyRead{int64(c.entriesLBA) * SectorSize, int64(c.num) * int64(c.size)}
			if len(reads) != 1 || reads[0] != want {
				t.Fatalf("entry reads = %v, want [%v]", reads, want)
			}
		})
	}
}

// TestFitAndRemoveTrailingRejectOversizedEntryArray checks that the failure
// reaches the file based entry points and that nothing is written.
func TestFitAndRemoveTrailingRejectOversizedEntryArray(t *testing.T) {
	spy, size := headerOnlyDisk(t, 2, 4096, 0xFFFFFFF8, 0)
	path := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(spy.content[1], SectorSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"Fit":            func() error { return Fit(path) },
		"RemoveTrailing": func() error { _, err := RemoveTrailing(path, AppleAPFSRecovery); return err },
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "gpt: unsupported entry array") {
			t.Fatalf("%s error = %v", name, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%s changed the disk image", name)
		}
	}
}

func openSizedDisk(t *testing.T, sectors uint64) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := f.Truncate(int64(sectors) * SectorSize); err != nil {
		t.Fatal(err)
	}
	return f
}

func diskDigest(t *testing.T, f *os.File) [sha256.Size]byte {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		t.Fatal(err)
	}
	return [sha256.Size]byte(digest.Sum(nil))
}

type writeCounter int

func (w *writeCounter) WriteAt(p []byte, _ int64) (int, error) {
	(*w)++
	return len(p), nil
}

func TestRejectUnwritableLayout(t *testing.T) {
	cases := []struct {
		name       string
		sectors    uint64
		entriesLBA uint64
		first      uint64
		last       uint64
		numEntries uint32
		used       bool
	}{
		{name: "large array consumes the backup header", sectors: 1026, entriesLBA: 2, first: 34, numEntries: 4096, used: true},
		{name: "empty overlapping arrays on 1027 sectors", sectors: 1027, entriesLBA: 2, first: 34, numEntries: 4096},
		{name: "empty overlapping arrays on 1030 sectors", sectors: 1030, entriesLBA: 2, first: 34, numEntries: 4096},
		{name: "large arrays overlap by one sector", sectors: 2050, entriesLBA: 2, first: 1026, numEntries: 4096},
		{name: "no sector between adjacent arrays", sectors: 2051, entriesLBA: 2, first: 1026, numEntries: 4096},
		{name: "large disk with entries in the backup array", sectors: 1 << 21, entriesLBA: 1<<21 - 1024, first: 34, numEntries: 4096, used: true},
		{name: "standard array in the backup array", sectors: 68, entriesLBA: 36, first: 34, numEntries: 128, used: true},
		{name: "first usable sector inside the primary array", sectors: 68, entriesLBA: 2, first: 33, last: 34, numEntries: 128},
		{name: "first usable sector inside the backup array", sectors: 68, entriesLBA: 2, first: 35, last: 34, numEntries: 128},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := openSizedDisk(t, c.sectors)
			h := header{
				Revision: 0x00010000, HeaderSize: headerSize, CurrentLBA: 1, BackupLBA: c.sectors - 1,
				FirstUsableLBA: c.first, LastUsableLBA: c.last, EntriesLBA: c.entriesLBA,
				NumEntries: c.numEntries, EntrySize: 128,
			}
			copy(h.Signature[:], signature)
			entries := make([]byte, int(h.NumEntries*h.EntrySize))
			if c.used {
				copy(entries[:16], guidBytes(t, AppleAPFSRecovery))
				binary.LittleEndian.PutUint64(entries[32:40], c.first)
				binary.LittleEndian.PutUint64(entries[40:48], c.first)
			}
			h.EntriesCRC = crc32.ChecksumIEEE(entries)
			sector, err := encodeHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt(sector, SectorSize); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt(entries, int64(c.entriesLBA)*SectorSize); err != nil {
				t.Fatal(err)
			}

			var writer writeCounter
			table := &Table{hdr: h, entries: entries, sectors: c.sectors}
			if err := table.Write(&writer); err == nil || !strings.HasPrefix(err.Error(), "gpt:") {
				t.Errorf("Write error = %v, want a gpt error", err)
			}
			if writer != 0 {
				t.Errorf("Write called WriteAt %d times, want 0", writer)
			}

			before := diskDigest(t, f)
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{"Read", func() error { _, err := Read(f, int64(c.sectors)*SectorSize); return err }},
				{"Fit", func() error { return Fit(f.Name()) }},
				{"RemoveTrailing", func() error { _, err := RemoveTrailing(f.Name(), AppleAPFSRecovery); return err }},
			} {
				if err := op.run(); err == nil || !strings.HasPrefix(err.Error(), "gpt:") {
					t.Errorf("%s error = %v, want a gpt error", op.name, err)
				}
				if diskDigest(t, f) != before {
					t.Fatalf("%s changed the disk image", op.name)
				}
			}
		})
	}
}

func TestFitAcceptsMaximumEntryArray(t *testing.T) {
	const (
		sectors     = 2 + 2*1024 + 2
		firstUsable = 2 + 1024
	)
	f := openSizedDisk(t, sectors)
	table := &Table{
		hdr: header{
			Revision: 0x00010000, HeaderSize: headerSize, FirstUsableLBA: firstUsable,
			EntriesLBA: 2, NumEntries: 4096, EntrySize: 128,
		},
		entries: make([]byte, 4096*128),
		sectors: sectors,
	}
	copy(table.hdr.Signature[:], signature)
	copy(table.entries[:16], guidBytes(t, appleAPFS))
	binary.LittleEndian.PutUint64(table.entries[32:40], firstUsable)
	binary.LittleEndian.PutUint64(table.entries[40:48], firstUsable)
	if err := table.Write(f); err != nil {
		t.Fatal(err)
	}
	if err := table.verify(f); err != nil {
		t.Fatal(err)
	}
	if before := read(t, f.Name()); before.hdr.LastUsableLBA != firstUsable {
		t.Fatalf("last usable sector = %d, want %d", before.hdr.LastUsableLBA, firstUsable)
	}
	if err := f.Truncate((sectors + 1) * SectorSize); err != nil {
		t.Fatal(err)
	}
	if err := Fit(f.Name()); err != nil {
		t.Fatal(err)
	}
	after := read(t, f.Name())
	if after.hdr.BackupLBA != sectors || after.hdr.LastUsableLBA != firstUsable+1 {
		t.Fatalf("unexpected fitted header %+v", after.hdr)
	}
	if err := after.verify(f); err != nil {
		t.Fatal(err)
	}
	if got, want := after.Partitions(), table.Partitions(); !slices.Equal(got, want) {
		t.Fatalf("fitted partitions = %+v, want %+v", got, want)
	}
}
