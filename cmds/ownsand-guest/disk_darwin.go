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
// sandbox and moves the backup partition table to the new end, but the host
// cannot grow APFS inside the image, so the guest grows its container
// itself, like the init process of a Linux guest grows ext4. The image
// build removes the recovery container behind the system container, so the
// system container is the last partition of the disk.
func growSystemDisk(logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store, err := bootPhysicalStore(ctx)
	if err != nil {
		logger.Warn("find the boot APFS container failed", "error", err)
		return
	}
	start := time.Now()
	if out, err := diskutil(ctx, "apfs", "resizeContainer", store, "0"); err != nil {
		// diskutil also fails when the container already fills the disk.
		logger.Info("the system disk was not grown", "error", err, "output", out)
		return
	}
	logger.Info("system disk grown", "duration_ms", time.Since(start).Milliseconds())
}

func diskutil(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/sbin/diskutil", args...)
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
