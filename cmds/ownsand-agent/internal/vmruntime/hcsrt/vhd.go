// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// Hyper-V attaches VHD and VHDX files only, and the host has no tool that
// converts the raw ext4 base image. A fixed VHD is the raw disk content
// followed by a 512-byte footer, so the runtime makes one by appending the
// footer of the "Virtual Hard Disk Image Format Specification" (version
// 1.0) to a copy of the raw image.

// vhdFooterSize is the size of the VHD footer.
const vhdFooterSize = 512

// vhdMaxSize is the largest fixed VHD that Hyper-V attaches (2040 GiB).
const vhdMaxSize = 2040 << 30

// vhdAlign is the alignment of the disk size; Hyper-V needs a multiple of
// the 512-byte sector, and 1 MiB keeps the guest partitioning tools happy.
const vhdAlign = 1 << 20

// VHD footer fields.
var vhdCookie = []byte("conectix")

const (
	vhdFeatures     = 0x00000002 // reserved bit, always set
	vhdVersion      = 0x00010000
	vhdFixedOffset  = 0xFFFFFFFFFFFFFFFF
	vhdCreatorApp   = "osnd"
	vhdCreatorVer   = 0x00010000
	vhdCreatorHost  = 0x5769326B // "Wi2k"
	vhdDiskTypeFix  = 2
	vhdChecksumAt   = 64
	vhdSectorSize   = 512
	vhdTimestampAt  = 24
	vhdCurrentAt    = 48
	vhdDiskTypeAt   = 60
	vhdUniqueIDAt   = 68
	vhdGeometryAt   = 56
	vhdOriginalAt   = 40
	vhdCreatorAppAt = 28
)

// vhdEpoch is the origin of the footer time stamp.
var vhdEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// vhdGeometry returns the CHS geometry of a disk of size bytes with the
// algorithm of the VHD specification (appendix "CHS Calculation").
func vhdGeometry(size int64) (cylinders uint16, heads, sectors uint8) {
	total := size / vhdSectorSize
	if total > 65535*16*255 {
		total = 65535 * 16 * 255
	}
	var spt, h, cth int64
	if total >= 65535*16*63 {
		spt, h = 255, 16
		cth = total / spt
	} else {
		spt = 17
		cth = total / spt
		h = (cth + 1023) / 1024
		if h < 4 {
			h = 4
		}
		if cth >= h*1024 || h > 16 {
			spt, h = 31, 16
			cth = total / spt
		}
		if cth >= h*1024 {
			spt, h = 63, 16
			cth = total / spt
		}
	}
	return uint16(cth / h), uint8(h), uint8(spt)
}

// vhdChecksum is the one's complement of the byte sum of the footer
// without its checksum field.
func vhdChecksum(f []byte) uint32 {
	var sum uint32
	for i, b := range f {
		if i >= vhdChecksumAt && i < vhdChecksumAt+4 {
			continue
		}
		sum += uint32(b)
	}
	return ^sum
}

// newVHDFooter returns the footer of a fixed VHD whose data is size bytes.
func newVHDFooter(size int64, now time.Time, uniqueID [16]byte) []byte {
	f := make([]byte, vhdFooterSize)
	copy(f, vhdCookie)
	binary.BigEndian.PutUint32(f[8:], vhdFeatures)
	binary.BigEndian.PutUint32(f[12:], vhdVersion)
	binary.BigEndian.PutUint64(f[16:], vhdFixedOffset)
	binary.BigEndian.PutUint32(f[vhdTimestampAt:], uint32(now.Sub(vhdEpoch)/time.Second))
	copy(f[vhdCreatorAppAt:], vhdCreatorApp)
	binary.BigEndian.PutUint32(f[32:], vhdCreatorVer)
	binary.BigEndian.PutUint32(f[36:], vhdCreatorHost)
	binary.BigEndian.PutUint64(f[vhdOriginalAt:], uint64(size))
	binary.BigEndian.PutUint64(f[vhdCurrentAt:], uint64(size))
	c, h, s := vhdGeometry(size)
	binary.BigEndian.PutUint16(f[vhdGeometryAt:], c)
	f[vhdGeometryAt+2] = h
	f[vhdGeometryAt+3] = s
	binary.BigEndian.PutUint32(f[vhdDiskTypeAt:], vhdDiskTypeFix)
	copy(f[vhdUniqueIDAt:], uniqueID[:])
	binary.BigEndian.PutUint32(f[vhdChecksumAt:], vhdChecksum(f))
	return f
}

// parseVHDFooter checks a fixed-VHD footer and returns its data size.
func parseVHDFooter(f []byte) (int64, error) {
	if len(f) != vhdFooterSize || !bytes.Equal(f[:len(vhdCookie)], vhdCookie) {
		return 0, errors.New("hcsrt: not a VHD footer")
	}
	if got, want := binary.BigEndian.Uint32(f[vhdChecksumAt:]), vhdChecksum(f); got != want {
		return 0, fmt.Errorf("hcsrt: VHD footer checksum %#x, want %#x", got, want)
	}
	if t := binary.BigEndian.Uint32(f[vhdDiskTypeAt:]); t != vhdDiskTypeFix {
		return 0, fmt.Errorf("hcsrt: VHD disk type %d is not fixed", t)
	}
	size := binary.BigEndian.Uint64(f[vhdCurrentAt:])
	if size == 0 || size%vhdSectorSize != 0 || size > vhdMaxSize {
		return 0, fmt.Errorf("hcsrt: VHD size %d is invalid", size)
	}
	return int64(size), nil
}

// makeFixedVHD turns the disk image at path into a fixed VHD of at least
// size bytes: it removes the footer that a VHD copy (a snapshot disk)
// carries, grows the data to size (the guest grows its file system at
// boot; a smaller size keeps the data), rounds it up to vhdAlign and
// appends a new footer with a new unique ID. It returns the data size.
func makeFixedVHD(path string, size int64, hasFooter bool) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	data := st.Size()
	if hasFooter {
		if data < vhdFooterSize {
			return 0, errors.New("hcsrt: VHD is shorter than its footer")
		}
		footer := make([]byte, vhdFooterSize)
		if _, err := f.ReadAt(footer, data-vhdFooterSize); err != nil {
			return 0, err
		}
		n, err := parseVHDFooter(footer)
		if err != nil {
			return 0, err
		}
		if n != data-vhdFooterSize {
			return 0, fmt.Errorf("hcsrt: VHD footer size %d does not match the file size %d", n, data)
		}
		data = n
		if err := f.Truncate(data); err != nil {
			return 0, err
		}
	}
	if size < data {
		size = data
	}
	size = (size + vhdAlign - 1) / vhdAlign * vhdAlign
	if size > vhdMaxSize {
		return 0, fmt.Errorf("hcsrt: disk size %d exceeds the fixed VHD limit", size)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return 0, err
	}
	// Writing the footer at size also extends the data with zeros. The file
	// must not be sparse: Hyper-V refuses sparse virtual disk files.
	if _, err := f.WriteAt(newVHDFooter(size, time.Now(), id), size); err != nil {
		return 0, err
	}
	return size, f.Sync()
}
