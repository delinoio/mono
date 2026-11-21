// SPDX-License-Identifier: Apache-2.0

// Package gpt edits the GUID partition table of a raw disk image file. The
// macOS runtime and image build use it on guest disks without attaching
// them to the host: the image build removes the APFS recovery container
// behind the system container, and a sandbox create moves the backup table
// to the end of a grown disk, so the guest can grow its system container to
// the end of the disk. Editing the file needs no disk tool, and System
// Integrity Protection, which blocks erasing an APFS recovery store through
// diskutil, does not apply to a file.
//
// See https://uefi.org/specs/UEFI/2.10/05_GUID_Partition_Table_Format.html.
package gpt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
)

// SectorSize is the logical block size of the disk images.
const SectorSize = 512

// AppleAPFSRecovery is the partition type of an APFS recovery container.
const AppleAPFSRecovery = "52637672-7900-11AA-AA11-00306543ECAC"

const (
	signature  = "EFI PART"
	headerSize = 92
)

// header is the GPT header (all fields little-endian).
type header struct {
	Signature      [8]byte
	Revision       uint32
	HeaderSize     uint32
	HeaderCRC      uint32
	Reserved       uint32
	CurrentLBA     uint64
	BackupLBA      uint64
	FirstUsableLBA uint64
	LastUsableLBA  uint64
	DiskGUID       [16]byte
	EntriesLBA     uint64
	NumEntries     uint32
	EntrySize      uint32
	EntriesCRC     uint32
}

// Partition is one used entry of the table.
type Partition struct {
	// Index is the position in the entry array (partition N is index N-1).
	Index    int
	TypeGUID string
	FirstLBA uint64
	LastLBA  uint64
}

// Table is a parsed partition table of a disk image.
type Table struct {
	hdr     header
	entries []byte
	sectors uint64
}

// guidString formats a GPT GUID (mixed-endian) as an uppercase string.
func guidString(b []byte) string {
	return strings.ToUpper(fmt.Sprintf("%08x-%04x-%04x-%x-%x",
		binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]), b[8:10], b[10:16]))
}

// Read parses the primary partition table of the disk image f.
func Read(f io.ReaderAt, size int64) (*Table, error) {
	if size%SectorSize != 0 || size < 34*SectorSize*2 {
		return nil, fmt.Errorf("gpt: disk size %d is not a valid GPT disk", size)
	}
	buf := make([]byte, SectorSize)
	if _, err := f.ReadAt(buf, SectorSize); err != nil {
		return nil, fmt.Errorf("gpt: read the header: %w", err)
	}
	var h header
	if err := binary.Read(bytes.NewReader(buf[:headerSize]), binary.LittleEndian, &h); err != nil {
		return nil, err
	}
	if string(h.Signature[:]) != signature || h.HeaderSize < headerSize || h.HeaderSize > SectorSize {
		return nil, errors.New("gpt: the disk has no GUID partition table")
	}
	check := append([]byte(nil), buf[:h.HeaderSize]...)
	binary.LittleEndian.PutUint32(check[16:20], 0)
	if crc32.ChecksumIEEE(check) != h.HeaderCRC {
		return nil, errors.New("gpt: the header checksum does not match")
	}
	if h.EntrySize < 128 || h.EntrySize%8 != 0 || h.NumEntries == 0 || h.NumEntries > 4096 {
		return nil, fmt.Errorf("gpt: unsupported entry array (%d entries of %d bytes)", h.NumEntries, h.EntrySize)
	}
	entries := make([]byte, int(h.NumEntries)*int(h.EntrySize))
	if _, err := f.ReadAt(entries, int64(h.EntriesLBA)*SectorSize); err != nil {
		return nil, fmt.Errorf("gpt: read the entries: %w", err)
	}
	if crc32.ChecksumIEEE(entries) != h.EntriesCRC {
		return nil, errors.New("gpt: the entry array checksum does not match")
	}
	return &Table{hdr: h, entries: entries, sectors: uint64(size / SectorSize)}, nil
}

// Partitions returns the used entries.
func (t *Table) Partitions() []Partition {
	var parts []Partition
	for i := 0; i < int(t.hdr.NumEntries); i++ {
		e := t.entries[i*int(t.hdr.EntrySize):]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		parts = append(parts, Partition{Index: i, TypeGUID: guidString(e[:16]),
			FirstLBA: binary.LittleEndian.Uint64(e[32:40]), LastLBA: binary.LittleEndian.Uint64(e[40:48])})
	}
	return parts
}

// Remove clears the entry of a partition. The data of the partition stays
// on the disk as unallocated space.
func (t *Table) Remove(index int) {
	e := t.entries[index*int(t.hdr.EntrySize) : (index+1)*int(t.hdr.EntrySize)]
	clear(e)
}

// entrySectors is the number of sectors of the entry array.
func (t *Table) entrySectors() uint64 {
	n := uint64(t.hdr.NumEntries) * uint64(t.hdr.EntrySize)
	return (n + SectorSize - 1) / SectorSize
}

// Write writes the primary and the backup tables for a disk of the table's
// size: the backup header goes to the last sector and the backup entries
// right before it, and the last usable sector follows them. It also updates
// the protective MBR to the disk size.
func (t *Table) Write(f io.WriterAt) error {
	last := t.sectors - 1
	backupEntries := last - t.entrySectors()
	lastUsable := backupEntries - 1
	for _, p := range t.Partitions() {
		if p.LastLBA > lastUsable {
			return fmt.Errorf("gpt: partition %d ends after the last usable sector", p.Index+1)
		}
	}
	t.hdr.EntriesCRC = crc32.ChecksumIEEE(t.entries)
	t.hdr.LastUsableLBA = lastUsable

	primary := t.hdr
	primary.CurrentLBA, primary.BackupLBA = 1, last
	backup := t.hdr
	backup.CurrentLBA, backup.BackupLBA, backup.EntriesLBA = last, 1, backupEntries

	if _, err := f.WriteAt(t.entries, int64(primary.EntriesLBA)*SectorSize); err != nil {
		return err
	}
	if _, err := f.WriteAt(t.entries, int64(backupEntries)*SectorSize); err != nil {
		return err
	}
	for _, h := range []header{primary, backup} {
		b, err := encodeHeader(h)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(b, int64(h.CurrentLBA)*SectorSize); err != nil {
			return err
		}
	}
	return writeProtectiveMBR(f, t.sectors)
}

func encodeHeader(h header) ([]byte, error) {
	var buf bytes.Buffer
	h.HeaderCRC = 0
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		return nil, err
	}
	b := make([]byte, SectorSize)
	copy(b, buf.Bytes())
	binary.LittleEndian.PutUint32(b[16:20], crc32.ChecksumIEEE(b[:h.HeaderSize]))
	return b, nil
}

// writeProtectiveMBR sets the size of the protective partition (type 0xEE)
// of the MBR to the disk size, capped at the 32-bit maximum.
func writeProtectiveMBR(f io.WriterAt, sectors uint64) error {
	r, ok := f.(io.ReaderAt)
	if !ok {
		return nil
	}
	mbr := make([]byte, SectorSize)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return err
	}
	if mbr[510] != 0x55 || mbr[511] != 0xAA {
		return nil
	}
	for i := 0; i < 4; i++ {
		entry := mbr[446+16*i:]
		if entry[4] != 0xEE {
			continue
		}
		size := min(sectors-1, 0xFFFFFFFF)
		binary.LittleEndian.PutUint32(entry[12:16], uint32(size))
		_, err := f.WriteAt(mbr, 0)
		return err
	}
	return nil
}

// RemoveTrailing removes the partitions of the type typeGUID that follow the
// last partition of another type, so that partition can grow, and returns
// how many it removed.
func RemoveTrailing(path, typeGUID string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	t, err := Read(f, st.Size())
	if err != nil {
		return 0, err
	}
	var lastOther uint64
	for _, p := range t.Partitions() {
		if p.TypeGUID != strings.ToUpper(typeGUID) && p.LastLBA > lastOther {
			lastOther = p.LastLBA
		}
	}
	removed := 0
	for _, p := range t.Partitions() {
		if p.TypeGUID == strings.ToUpper(typeGUID) && p.FirstLBA > lastOther {
			t.Remove(p.Index)
			removed++
		}
	}
	if removed == 0 {
		return 0, nil
	}
	if err := t.Write(f); err != nil {
		return 0, err
	}
	return removed, f.Sync()
}

// Fit rewrites the table of the disk image at path for its current size,
// after the file grew: the backup table moves to the new end and the free
// space becomes usable.
func Fit(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	t, err := Read(f, st.Size())
	if err != nil {
		return err
	}
	if t.hdr.LastUsableLBA == t.sectors-1-t.entrySectors()-1 {
		return nil
	}
	oldBackup := t.hdr.BackupLBA
	if err := t.Write(f); err != nil {
		return err
	}
	// Clear the old backup header, which now lies in the free space, so no
	// tool mistakes it for a table.
	if oldBackup > 1 && oldBackup < t.sectors-1 {
		if _, err := f.WriteAt(make([]byte, SectorSize), int64(oldBackup)*SectorSize); err != nil {
			return err
		}
	}
	return f.Sync()
}
