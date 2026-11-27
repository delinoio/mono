// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file holds the attachment and mount logic of the macOS guest disk
// without a build tag, so that unit tests exercise it on every host. Only
// the macOS build wires it to the real host tools.

// hdiutilPath is the host tool that attaches and detaches the guest disk.
const hdiutilPath = "/usr/bin/hdiutil"

const diskutilPath = "/usr/sbin/diskutil"

// unmountTimeout bounds Data volume cleanup before the whole disk detaches.
const unmountTimeout = 30 * time.Second

// detachTimeout bounds each detach attempt of the guest disk. The detach runs
// on its own context, because the build context is often canceled already
// when the cleanup runs, and a canceled context would leave the disk attached.
const detachTimeout = 30 * time.Second

const attachmentInfoTimeout = 30 * time.Second

// errNoGUIDPartitionTable reports an attached guest disk without a GUID
// partition table, which the macOS installer always creates.
var errNoGUIDPartitionTable = errors.New("imagebuild: the attached guest disk has no GUID partition table")

// hostRunner runs a host tool and returns its captured standard output, even on failure.
type hostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// runHostTool is the hostRunner that runs the real tool.
func runHostTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// wholeDiskPattern matches the whole-disk device name at the start of a device
// path, for example disk4 in
// /dev/disk4s2.
var wholeDiskPattern = regexp.MustCompile(`/dev/(disk[0-9]+)`)

// attachedWholeDisk returns the whole-disk device (for example disk4) that
// the plist output of `hdiutil attach` names, and whether the output marks it
// with the GUID partition scheme. The parser does not depend on another host
// tool, so a failure to convert the plist cannot hide a successful attach. A
// malformed plist falls back to the first device path in the raw output.
func attachedWholeDisk(out []byte) (whole string, guid bool) {
	plist, _ := readDiskImagePlist(out)
	for _, entity := range plist.array("system-entities") {
		m := wholeDiskPattern.FindStringSubmatch(entity.dict.text("dev-entry"))
		if m == nil {
			continue
		}
		if entity.dict.text("content-hint") == "GUID_partition_scheme" {
			return m[1], true
		}
		if whole == "" {
			whole = m[1]
		}
	}
	if whole == "" {
		if m := wholeDiskPattern.FindSubmatch(out); m != nil {
			whole = string(m[1])
		}
	}
	return whole, false
}

// Recovery requires a complete whole-disk entry. A raw match or a partition
// name is insufficient because the attach command did not succeed.
var wholeDiskEntryPattern = regexp.MustCompile(`^/dev/(disk[0-9]+)$`)

func interruptedAttachDevice(out []byte) string {
	plist, err := readDiskImagePlist(out)
	if err != nil {
		var syntaxErr *xml.SyntaxError
		if !errors.As(err, &syntaxErr) || syntaxErr.Msg != "unexpected EOF" {
			return ""
		}
	}
	var whole string
	for _, entity := range plist.array("system-entities") {
		device := wholeDiskEntryPattern.FindStringSubmatch(entity.dict.text("dev-entry"))
		if device == nil {
			continue
		}
		if whole != "" && whole != device[1] {
			return ""
		}
		whole = device[1]
	}
	return whole
}

func verifyDiskAttachment(run hostRunner, disk, whole string, logger *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), attachmentInfoTimeout)
	defer cancel()

	out, err := run(ctx, hdiutilPath, "info", "-plist")
	if err != nil {
		logger.Warn("verify the guest disk attachment failed", "device", whole, "error", err)
		return false
	}
	plist, err := readDiskImagePlist(out)
	if err != nil {
		logger.Warn("read the attached disk inventory failed", "device", whole, "error", err)
		return false
	}

	dev := "/dev/" + whole
	var path string
	found := false
	for _, image := range plist.array("images") {
		for _, entity := range image.dict.array("system-entities") {
			if entity.dict.text("dev-entry") != dev {
				continue
			}
			if found {
				return false
			}
			found = true
			path = image.dict.text("image-path")
		}
	}
	if !filepath.IsAbs(path) {
		return false
	}
	staged, err := filepath.Abs(disk)
	if err != nil {
		return false
	}
	if path == staged {
		return true
	}
	// hdiutil can resolve a symlink in the staging directory, such as /var.
	resolved, err := filepath.EvalSymlinks(staged)
	return err == nil && resolved == path
}

// withAttachedDisk attaches the raw disk image without mounting it, calls use
// with the whole-disk device (for example disk4) and detaches that device
// before it returns. If the attach fails after it reports a device, cleanup
// requires a verified association with the staged image. A cleanup failure
// is reported only when no earlier error exists.
func withAttachedDisk(ctx context.Context, run hostRunner, disk string, logger *slog.Logger, use func(whole string) error) (err error) {
	out, err := run(ctx, hdiutilPath, "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", "-plist", disk)
	if err != nil {
		if whole := interruptedAttachDevice(out); whole != "" {
			verify := func() bool { return verifyDiskAttachment(run, disk, whole, logger) }
			if detachErr := detachDisk(run, whole, logger, verify); detachErr != nil {
				logger.Warn("clean up the interrupted guest disk attachment failed", "device", whole, "error", detachErr)
			}
		}
		return fmt.Errorf("imagebuild: attach the guest disk: %w", err)
	}
	whole, guid := attachedWholeDisk(out)
	if whole == "" {
		return errors.New("imagebuild: the attach output of the guest disk names no device")
	}
	defer func() {
		if derr := detachDisk(run, whole, logger, nil); derr != nil && err == nil {
			err = derr
		}
	}()
	if !guid {
		return errNoGUIDPartitionTable
	}
	return use(whole)
}

// withMountedDataVolume unmounts the volume after use, even if the build
// context was canceled. The deadline lets whole-disk cleanup proceed if
// diskutil stalls.
func withMountedDataVolume(ctx context.Context, run hostRunner, data, mnt string, use func() error) (err error) {
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if _, err := run(ctx, diskutilPath, "mount", "-mountPoint", mnt, data); err != nil {
		return fmt.Errorf("imagebuild: mount the guest Data volume: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), unmountTimeout)
		defer cancel()
		if _, unmountErr := run(cleanupCtx, diskutilPath, "unmount", data); unmountErr != nil && err == nil {
			if ctxErr := cleanupCtx.Err(); ctxErr != nil && !errors.Is(unmountErr, ctxErr) {
				unmountErr = fmt.Errorf("%w: %w", ctxErr, unmountErr)
			}
			err = fmt.Errorf("imagebuild: unmount the guest Data volume: %w", unmountErr)
		}
		os.Remove(mnt)
	}()
	return use()
}

// detachDisk detaches the whole-disk device, and forces the detach when the
// first attempt fails. Each attempt runs on a fresh bounded context that the
// build context cannot cancel. After an interrupted attach, verify must
// confirm that the device still belongs to the image before each attempt.
func detachDisk(run hostRunner, whole string, logger *slog.Logger, verify func() bool) error {
	if verify != nil && !verify() {
		return nil
	}
	dev := "/dev/" + whole
	attempt := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), detachTimeout)
		defer cancel()
		_, err := run(ctx, hdiutilPath, args...)
		return err
	}
	derr := attempt("detach", dev)
	if derr == nil {
		return nil
	}
	if verify != nil && !verify() {
		return derr
	}
	logger.Warn("detach the guest disk failed; forcing", "device", dev, "error", derr)
	return attempt("detach", "-force", dev)
}
