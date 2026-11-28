// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"debug/macho"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// machOHeader returns a 64-bit little-endian Mach-O header with no load
// commands, which debug/macho accepts.
func machOHeader(cpu macho.Cpu, typ macho.Type) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b[0:], macho.Magic64)
	binary.LittleEndian.PutUint32(b[4:], uint32(cpu))
	binary.LittleEndian.PutUint32(b[12:], uint32(typ))
	return b
}

// peImage returns an MZ+PE image with the given machine and characteristics
// and, when withOptionalHeader is set, a 240-byte PE32+ optional header.
func peImage(machine, characteristics uint16, withOptionalHeader bool) []byte {
	optSize := 0
	if withOptionalHeader {
		optSize = 240
	}
	// The padding keeps debug/pe from hitting EOF on header-only images.
	b := make([]byte, 0x40+4+20+optSize+16)
	b[0], b[1] = 'M', 'Z'
	b[0x3c] = 0x40
	copy(b[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x44:], machine)
	binary.LittleEndian.PutUint16(b[0x54:], uint16(optSize))
	binary.LittleEndian.PutUint16(b[0x56:], characteristics)
	if withOptionalHeader {
		binary.LittleEndian.PutUint16(b[0x58:], 0x20b)
		binary.LittleEndian.PutUint32(b[0x58+108:], 16) // NumberOfRvaAndSizes
	}
	return b
}

// peObject returns a COFF object file image: the file header starts at offset
// 0 (there is no MZ stub and no PE signature) and there is no optional header.
func peObject(machine uint16) []byte {
	b := make([]byte, 128) // debug/pe reads a 96-byte DOS header first
	binary.LittleEndian.PutUint16(b[0:], machine)
	return b
}

func writeFixture(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckDarwinARM64FileType(t *testing.T) {
	const cpuX86_64 = macho.Cpu(0x01000007)
	tests := []struct {
		name    string
		content []byte
		wantErr string // substring; empty means accepted
	}{
		{name: "arm64 executable", content: machOHeader(macho.CpuArm64, macho.TypeExec)},
		{name: "arm64 relocatable object", content: machOHeader(macho.CpuArm64, macho.TypeObj), wantErr: "not an executable (MH_EXECUTE)"},
		{name: "arm64 dylib", content: machOHeader(macho.CpuArm64, macho.TypeDylib), wantErr: "not an executable (MH_EXECUTE)"},
		{name: "x86_64 executable", content: machOHeader(cpuX86_64, macho.TypeExec), wantErr: "macOS guests need arm64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDarwinARM64(writeFixture(t, tc.content))
			assertCheckResult(t, err, tc.wantErr)
		})
	}
}

type fatMachOSlice struct {
	cpu    macho.Cpu
	header []byte
}

func fatMachO(slices ...fatMachOSlice) []byte {
	const headerSize, entrySize = 8, 20
	b := make([]byte, headerSize+entrySize*len(slices))
	binary.BigEndian.PutUint32(b[0:], macho.MagicFat)
	binary.BigEndian.PutUint32(b[4:], uint32(len(slices)))
	for i, slice := range slices {
		entry := b[headerSize+i*entrySize:]
		binary.BigEndian.PutUint32(entry[0:], uint32(slice.cpu))
		binary.BigEndian.PutUint32(entry[4:], binary.LittleEndian.Uint32(slice.header[8:]))
		binary.BigEndian.PutUint32(entry[8:], uint32(len(b)))
		binary.BigEndian.PutUint32(entry[12:], uint32(len(slice.header)))
		b = append(b, slice.header...)
	}
	return b
}

func TestCheckDarwinARM64Universal(t *testing.T) {
	amd64 := fatMachOSlice{macho.CpuAmd64, machOHeader(macho.CpuAmd64, macho.TypeExec)}
	arm64 := fatMachOSlice{macho.CpuArm64, machOHeader(macho.CpuArm64, macho.TypeExec)}
	both := fatMachO(amd64, arm64)
	unordered := fatMachO(amd64, arm64)
	copy(unordered[8:28], both[28:48])
	copy(unordered[28:48], both[8:28])
	wrongSubtype := fatMachO(arm64)
	binary.BigEndian.PutUint32(wrongSubtype[12:], 1)
	overlap := fatMachO(amd64, arm64)
	binary.BigEndian.PutUint32(overlap[20:], uint32(len(amd64.header)+1))
	misaligned := fatMachO(arm64)
	binary.BigEndian.PutUint32(misaligned[24:], 3) // Offset 28 is not a multiple of 8.
	fat64 := fatMachO(arm64)
	binary.BigEndian.PutUint32(fat64[0:], 0xcafebabf) // FAT_MAGIC_64

	// Keep the arm64 header intact while truncating the slice's declared range.
	truncated := fatMachO(arm64)
	binary.BigEndian.PutUint32(truncated[20:], uint32(len(arm64.header)+1))

	tests := []struct {
		name    string
		content []byte
		wantErr string
	}{
		{name: "x86_64 and arm64 executables", content: both},
		{name: "out of order architecture entries", content: unordered},
		{name: "no arm64 slice", content: fatMachO(amd64), wantErr: "has no arm64 slice"},
		{name: "arm64 object", content: fatMachO(fatMachOSlice{macho.CpuArm64, machOHeader(macho.CpuArm64, macho.TypeObj)}), wantErr: "not an executable (MH_EXECUTE)"},
		{name: "arm64 dylib", content: fatMachO(fatMachOSlice{macho.CpuArm64, machOHeader(macho.CpuArm64, macho.TypeDylib)}), wantErr: "not an executable (MH_EXECUTE)"},
		{name: "mismatched CPU", content: fatMachO(fatMachOSlice{macho.CpuArm64, amd64.header}), wantErr: "does not match its embedded Mach-O header"},
		{name: "mismatched CPU subtype", content: wrongSubtype, wantErr: "does not match its embedded Mach-O header"},
		{name: "truncated architecture table", content: both[:8], wantErr: "invalid fat_arch header"},
		{name: "truncated declared slice", content: truncated, wantErr: "read end of universal Mach-O slice"},
		{name: "overlapping slices", content: overlap, wantErr: "invalid offset or alignment"},
		{name: "misaligned slice", content: misaligned, wantErr: "invalid offset or alignment"},
		{name: "64-bit wrapper magic", content: fat64, wantErr: "invalid magic number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFixture(t, tc.content)
			err := checkDarwinARM64(path)
			assertCheckResult(t, err, tc.wantErr)
			if err != nil && !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name helper %q", err, path)
			}
		})
	}
}

func TestCheckWindowsAMD64FileType(t *testing.T) {
	const (
		machineAMD64 = 0x8664
		machineARM64 = 0xaa64
		executable   = 0x0002
		dll          = 0x2000
	)
	tests := []struct {
		name    string
		content []byte
		wantErr string // substring; empty means accepted
	}{
		{name: "amd64 executable", content: peImage(machineAMD64, executable, true)},
		{name: "amd64 executable with large-address-aware flag", content: peImage(machineAMD64, executable|0x20, true)},
		{name: "amd64 object without executable flag", content: peImage(machineAMD64, 0, false), wantErr: "not an executable image"},
		{name: "amd64 executable flag without optional header", content: peImage(machineAMD64, executable, false), wantErr: "not an executable image"},
		{name: "amd64 optional header without executable flag", content: peImage(machineAMD64, 0, true), wantErr: "not an executable image"},
		{name: "amd64 object file without MZ header", content: peObject(machineAMD64), wantErr: "not an executable image"},
		{name: "amd64 dll", content: peImage(machineAMD64, executable|dll, true), wantErr: "is a DLL"},
		{name: "arm64 executable", content: peImage(machineARM64, executable, true), wantErr: "not an amd64 executable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWindowsAMD64(writeFixture(t, tc.content))
			assertCheckResult(t, err, tc.wantErr)
		})
	}
}

// TestCheckRealCrossBuiltGuestHelpers feeds the check functions the real
// guest helper builds: go build with GOOS=darwin GOARCH=arm64 and GOOS=windows
// GOARCH=amd64. It runs only when OWNSAND_IMAGEBUILD_REAL_DARWIN_ARM64 and
// OWNSAND_IMAGEBUILD_REAL_WINDOWS_AMD64 name those binaries, because a
// cross-build is too slow for the default test run.
func TestCheckRealCrossBuiltGuestHelpers(t *testing.T) {
	for _, tc := range []struct {
		env   string
		check func(string) error
	}{
		{"OWNSAND_IMAGEBUILD_REAL_DARWIN_ARM64", checkDarwinARM64},
		{"OWNSAND_IMAGEBUILD_REAL_WINDOWS_AMD64", checkWindowsAMD64},
	} {
		path := os.Getenv(tc.env)
		if path == "" {
			continue
		}
		t.Run(tc.env, func(t *testing.T) {
			if err := tc.check(path); err != nil {
				t.Fatalf("a real cross-built helper was rejected: %v", err)
			}
		})
	}
}

func assertCheckResult(t *testing.T, err error, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("a valid executable was rejected: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("accepted; want an error containing %q", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("error = %q; want it to contain %q", err.Error(), wantErr)
	}
}
