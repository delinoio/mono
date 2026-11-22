// SPDX-License-Identifier: FSL-1.1-ALv2

// Package images owns the layout of the locally built base images in the
// state directory:
//
//	images/linux/      vmlinux and rootfs.ext4 (see cmds/ownsand-agent/images/linux)
//	images/macos/      disk.img, aux.img, hardware-model and image.json
//	images/windows/    disk.vhdx and image.json
//
// `ownsand-agent image build` writes a macOS or Windows image into a build
// directory next to its final directory and renames it into place only after
// the image booted and its guest helper answered, so a runtime never sees a
// partial image. A process that ends between the two renames of the install
// leaves the previous image in `<dir>.old`; Recover restores it (see the
// agent contract).
package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Directory names below images/ in the state directory.
const (
	LinuxDir   = "linux"
	MacOSDir   = "macos"
	WindowsDir = "windows"
)

// File names of the macOS base image. A macOS snapshot holds the same
// files (with the disk and the auxiliary storage of the sandbox) plus the
// machine identifier and, for a memory-state snapshot, vm.state.
const (
	MacOSDisk          = "disk.img"
	MacOSAuxStorage    = "aux.img"
	MacOSHardwareModel = "hardware-model"
	MacOSMachineID     = "machine-id"
)

// WindowsDisk is the file name of the Windows base image disk: a dynamic
// VHDX with a GPT layout (EFI system partition, MSR and the Windows
// partition last, so the guest can extend it).
const WindowsDisk = "disk.vhdx"

// MetadataFile describes a built image.
const MetadataFile = "image.json"

// Metadata is the content of image.json. It holds no secrets.
type Metadata struct {
	GuestOS ownsandv1.GuestOs `json:"guest_os"`
	// Source names the IPSW build version or the Windows image name and
	// edition; it never holds a path of the operator.
	Source    string    `json:"source"`
	BuiltAt   time.Time `json:"built_at"`
	DiskBytes int64     `json:"disk_bytes"`
	// MinimumCPUCount and MinimumMemoryMiB are the smallest resources that
	// the guest needs (for macOS, the restore image requirements).
	MinimumCPUCount  int32 `json:"minimum_cpu_count"`
	MinimumMemoryMiB int64 `json:"minimum_memory_mib"`
	// GuestVersion is the guest helper version installed in the image.
	GuestVersion string `json:"guest_version,omitempty"`
}

// Dir returns the image directory of a guest OS below imagesDir.
func Dir(imagesDir string, os ownsandv1.GuestOs) (string, error) {
	switch os {
	case ownsandv1.GuestOs_GUEST_OS_LINUX:
		return filepath.Join(imagesDir, LinuxDir), nil
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		return filepath.Join(imagesDir, MacOSDir), nil
	case ownsandv1.GuestOs_GUEST_OS_WINDOWS:
		return filepath.Join(imagesDir, WindowsDir), nil
	}
	return "", fmt.Errorf("images: no image directory for guest OS %v", os)
}

// ReadMetadata reads image.json of an image directory.
func ReadMetadata(dir string) (Metadata, error) {
	var m Metadata
	b, err := os.ReadFile(filepath.Join(dir, MetadataFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("images: parse %s: %w", MetadataFile, err)
	}
	return m, nil
}

// WriteMetadata writes image.json into an image directory.
func WriteMetadata(dir string, m Metadata) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MetadataFile), append(b, '\n'), 0o600)
}

// BuildDir returns the build directory of an image directory.
func BuildDir(dir string) string { return dir + ".build" }

// OldDir returns the directory that holds the previous generation of an
// image directory while Install exchanges the two.
func OldDir(dir string) string { return dir + ".old" }

// invalidDir returns the directory that Recover moves an invalid image
// directory to before it restores the backup, so nothing is deleted.
func invalidDir(dir string) string { return dir + ".invalid" }

// rename is os.Rename; tests replace it to inject a failure or an
// interruption between the two renames of Install.
var rename = os.Rename

// requiredFiles returns the files, besides image.json, that a complete image
// of the guest OS holds. It is nil for a guest OS with no locally built
// image.
func requiredFiles(os ownsandv1.GuestOs) []string {
	switch os {
	case ownsandv1.GuestOs_GUEST_OS_MACOS:
		return []string{MacOSDisk, MacOSAuxStorage, MacOSHardwareModel}
	case ownsandv1.GuestOs_GUEST_OS_WINDOWS:
		return []string{WindowsDisk}
	}
	return nil
}

// Complete reports whether dir holds a complete locally built image: a
// readable image.json of a macOS or Windows image and every file that the
// guest OS needs. A directory that Complete rejects is never an image that a
// runtime can boot.
func Complete(dir string) bool {
	m, err := ReadMetadata(dir)
	if err != nil {
		return false
	}
	files := requiredFiles(m.GuestOS)
	if files == nil {
		return false
	}
	for _, name := range files {
		if st, err := os.Stat(filepath.Join(dir, name)); err != nil || !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// RecoverResult is the outcome of Recover.
type RecoverResult int

const (
	// RecoverNone means that the image directory is complete and nothing
	// changed (a leftover backup stays until the next Install).
	RecoverNone RecoverResult = iota
	// RecoverNoBackup means that the image directory is missing or invalid
	// and no complete backup exists, so there is nothing to restore. The
	// build directory is never a candidate: it may be unfinished.
	RecoverNoBackup
	// RecoverRestored means that Recover renamed the backup back to the
	// image directory.
	RecoverRestored
	// RecoverDeferred means that another process holds the lock of the image
	// destination, for example an Install that is between its two renames.
	// Recover changed nothing; the holder completes or fails and a later
	// call recovers if it is still needed.
	RecoverDeferred
)

func (r RecoverResult) String() string {
	switch r {
	case RecoverNone:
		return "none"
	case RecoverNoBackup:
		return "no-backup"
	case RecoverRestored:
		return "restored"
	case RecoverDeferred:
		return "deferred"
	}
	return fmt.Sprintf("RecoverResult(%d)", int(r))
}

// Recover repairs an image directory after an interrupted Install. Install
// moves the old image to `<dir>.old` and then renames the build directory to
// dir, so a process that ended between the two renames leaves no dir and the
// only complete image in `<dir>.old`. When dir is missing or not a complete
// image and `<dir>.old` is a complete image, Recover renames the backup back
// to dir (an invalid dir moves to `<dir>.invalid` first). A complete dir
// takes precedence over any leftover backup, and Recover never touches the
// build directory.
//
// Recover takes the lock of the image destination without waiting, only when
// it has something to restore, so it never exchanges directories under a
// running Install; it returns RecoverDeferred then. A caller that already
// holds the lock (Install) must not call it.
func Recover(dir string) (RecoverResult, error) {
	if res, needed := recoveryNeeded(dir); !needed {
		return res, nil
	}
	lock, err := TryLock(dir)
	if errors.Is(err, ErrLocked) {
		slog.Debug("image recovery deferred: another process holds the image lock", "dir", dir, "lock_path", LockPath(dir))
		return RecoverDeferred, nil
	}
	if err != nil {
		return RecoverNone, fmt.Errorf("images: recover %s: %w", dir, err)
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			slog.Warn("release the image lock failed", "lock_path", LockPath(dir), "error", rerr)
		}
	}()
	return recoverLocked(dir)
}

// recoveryNeeded reports, without a lock, whether a backup should be
// restored; when it is not needed it also returns the result to report.
func recoveryNeeded(dir string) (RecoverResult, bool) {
	if Complete(dir) {
		return RecoverNone, false
	}
	if !Complete(OldDir(dir)) {
		return RecoverNoBackup, false
	}
	return RecoverNone, true
}

// recoverLocked is Recover for a caller that holds the lock of dir.
func recoverLocked(dir string) (RecoverResult, error) {
	if res, needed := recoveryNeeded(dir); !needed {
		return res, nil
	}
	old := OldDir(dir)
	if _, err := os.Lstat(dir); err == nil {
		// An invalid image directory: keep it for inspection and make room.
		invalid := invalidDir(dir)
		if err := os.RemoveAll(invalid); err != nil {
			return RecoverNone, fmt.Errorf("images: recover %s: %w", dir, err)
		}
		if err := rename(dir, invalid); err != nil {
			return RecoverNone, fmt.Errorf("images: recover %s: move the invalid image aside: %w", dir, err)
		}
		slog.Warn("moved an invalid image directory aside before restoring the backup", "dir", dir, "invalid_dir", invalid)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RecoverNone, fmt.Errorf("images: recover %s: %w", dir, err)
	}
	if err := rename(old, dir); err != nil {
		return RecoverNone, fmt.Errorf("images: recover %s: restore the backup: %w", dir, err)
	}
	slog.Warn("restored the previous image after an interrupted install", "dir", dir, "backup_dir", old)
	return RecoverRestored, nil
}

// Resolve recovers dir (see Recover) and reads its image.json. Readers use
// it where they need the metadata of the base image. A recovery failure is
// logged and does not hide the read result, because the image directory may
// be usable anyway.
func Resolve(dir string) (Metadata, error) {
	if _, err := Recover(dir); err != nil {
		slog.Warn("image recovery failed", "dir", dir, "error", err)
	}
	return ReadMetadata(dir)
}

// Install replaces the image directory dir with the finished build
// directory. The caller holds the lock of the image destination (TryLock),
// which keeps Recover of a reader out of the two renames.
//
// The old image moves aside first and stays until the new image is in place,
// so at every point of the exchange one complete image exists, under dir or
// under `<dir>.old`. Install first calls Recover, so an exchange that an
// earlier process left half done is repaired instead of deleted, and it
// removes a leftover `<dir>.old` only when dir holds a complete image. When
// the second rename fails, the old image moves back.
func Install(dir string) error {
	if _, err := recoverLocked(dir); err != nil {
		return err
	}
	build := BuildDir(dir)
	if _, err := os.Stat(filepath.Join(build, MetadataFile)); err != nil {
		return fmt.Errorf("images: the build directory has no %s: %w", MetadataFile, err)
	}
	old := OldDir(dir)
	// After recoverLocked, a complete backup implies a complete dir, so
	// whatever is left in old is stale and the only complete generation is
	// not at risk.
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	moved := true
	if err := rename(dir, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		moved = false
	}
	if err := rename(build, dir); err != nil {
		if !moved {
			return err
		}
		// Put the old image back so a failed install keeps it.
		if rerr := rename(old, dir); rerr != nil {
			slog.Error("restoring the old image after a failed install failed; the next recovery restores it from the backup",
				"dir", dir, "backup_dir", old, "error", rerr)
			return errors.Join(err, fmt.Errorf("images: restore the old image: %w", rerr))
		}
		return err
	}
	if err := os.RemoveAll(old); err != nil {
		slog.Warn("remove the previous image failed; the next install removes it", "backup_dir", old, "error", err)
	}
	if err := os.RemoveAll(invalidDir(dir)); err != nil {
		slog.Warn("remove the invalid image directory failed", "invalid_dir", invalidDir(dir), "error", err)
	}
	return nil
}
