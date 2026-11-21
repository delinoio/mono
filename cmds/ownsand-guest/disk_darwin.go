// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// growSystemDisk grows the APFS container of the boot volume of a macOS
// guest to the end of its disk. The agent enlarges the disk image of a
// sandbox to the requested disk size, but a raw image has no file system
// tools on the host, so the guest grows its container itself, like the init
// process of a Linux guest grows ext4. The image build removes the recovery
// container behind the system container, so the system container is the
// last partition of the disk.
func growSystemDisk(logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := bootPhysicalStore(ctx)
	if err != nil {
		logger.Warn("find the boot APFS container failed", "error", err)
		return
	}
	start := time.Now()
	out, err := diskutil(ctx, nil, "apfs", "resizeContainer", store, "0")
	if err == nil {
		logger.Info("system disk grown", "duration_ms", time.Since(start).Milliseconds())
		return
	}
	logger.Debug("resize the APFS container failed; repairing the partition table first", "output", out)
	// A grown disk image keeps its backup GPT header at the old end of the
	// disk; repairDisk moves it to the new end. It asks for confirmation on
	// standard input.
	whole := store
	if i := strings.LastIndex(store, "s"); i > len("disk") {
		whole = store[:i]
	}
	if out, err := diskutil(ctx, strings.NewReader("y\n"), "repairDisk", whole); err != nil {
		logger.Warn("repair the system disk partition table failed", "error", err, "output", out)
		return
	}
	if out, err := diskutil(ctx, nil, "apfs", "resizeContainer", store, "0"); err != nil {
		logger.Warn("grow the system disk failed", "error", err, "output", out)
		return
	}
	logger.Info("system disk grown", "duration_ms", time.Since(start).Milliseconds())
}

func diskutil(ctx context.Context, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/sbin/diskutil", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	out, err := cmd.CombinedOutput()
	// diskutil prints device names and sizes only.
	return strings.TrimSpace(string(out)), err
}

// bootPhysicalStore returns the partition (for example disk0s2) that holds
// the APFS container of the boot volume.
func bootPhysicalStore(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", "/").Output()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(out)
	js, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info struct {
		APFSPhysicalStores []struct {
			APFSPhysicalStore string `json:"APFSPhysicalStore"`
		} `json:"APFSPhysicalStores"`
	}
	if err := json.Unmarshal(js, &info); err != nil {
		return "", err
	}
	if len(info.APFSPhysicalStores) != 1 || info.APFSPhysicalStores[0].APFSPhysicalStore == "" {
		return "", fmt.Errorf("the boot volume has %d APFS physical stores", len(info.APFSPhysicalStores))
	}
	return info.APFSPhysicalStores[0].APFSPhysicalStore, nil
}
