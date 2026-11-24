// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Paths of the guest helper in a macOS guest.
const (
	macOSGuestBinary = "/usr/local/libexec/ownsand-guest"
	macOSGuestLabel  = "io.delino.ownsand-guest"
	macOSGuestPlist  = "/Library/LaunchDaemons/" + macOSGuestLabel + ".plist"
	macOSGuestLog    = "/var/log/ownsand-guest.log"
)

// macOSGuestDaemon is the launchd daemon of the guest helper. It runs as
// root from the first boot on, before any user exists, and launchd restarts
// it when it exits.
const macOSGuestDaemon = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + macOSGuestLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + macOSGuestBinary + `</string>
    <string>--grow-disk</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>HOME</key>
    <string>/var/root</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>` + macOSGuestLog + `</string>
</dict>
</plist>
`

// macOSProvisionCommands run in the guest after its first boot: a DHCP
// network service for the guest's network interface (the Setup Assistant,
// which normally creates it, never runs), no sleep, no Spotlight indexing
// and no automatic software updates in sandboxes.
var macOSProvisionCommands = [][]string{
	{"/usr/sbin/networksetup", "-detectnewhardware"},
	{"/bin/sh", "-c", "/usr/sbin/networksetup -listnetworkserviceorder | /usr/bin/grep -q 'Device: en'"},
	{"/usr/bin/pmset", "-a", "sleep", "0", "displaysleep", "0", "disksleep", "0"},
	{"/usr/bin/mdutil", "-a", "-i", "off"},
	{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticCheckEnabled", "-bool", "false"},
	{"/usr/bin/defaults", "write", "/Library/Preferences/com.apple.SoftwareUpdate", "AutomaticDownload", "-bool", "false"},
	{"/bin/sync"},
}

// runTool runs a host tool and returns its standard output.
var runTool hostRunner = runHostTool

// plistJSON runs a tool that prints a property list and decodes it as JSON.
func plistJSON(ctx context.Context, v any, name string, args ...string) error {
	out, err := runTool(ctx, name, args...)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(out)
	js, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("plutil: %w", err)
	}
	return json.Unmarshal(js, v)
}

type apfsList struct {
	Containers []struct {
		ContainerReference string `json:"ContainerReference"`
		PhysicalStores     []struct {
			DeviceIdentifier string `json:"DeviceIdentifier"`
		} `json:"PhysicalStores"`
		Volumes []struct {
			DeviceIdentifier string   `json:"DeviceIdentifier"`
			Name             string   `json:"Name"`
			Roles            []string `json:"Roles"`
		} `json:"Volumes"`
	} `json:"Containers"`
}

// dataVolume returns the device of the Data volume of the macOS installation
// on the whole disk device (for example disk8).
func dataVolume(ctx context.Context, wholeDisk string) (string, error) {
	var list apfsList
	if err := plistJSON(ctx, &list, diskutilPath, "apfs", "list", "-plist"); err != nil {
		return "", err
	}
	for _, c := range list.Containers {
		onDisk := false
		for _, s := range c.PhysicalStores {
			if strings.HasPrefix(s.DeviceIdentifier, wholeDisk+"s") {
				onDisk = true
			}
		}
		if !onDisk {
			continue
		}
		for _, v := range c.Volumes {
			for _, role := range v.Roles {
				if role == "Data" {
					return v.DeviceIdentifier, nil
				}
			}
		}
	}
	return "", errors.New("imagebuild: the installed disk has no APFS Data volume")
}

// provisionMacOSDisk installs the guest helper as a launchd daemon on the
// Data volume of the installed disk and marks the Setup Assistant as done,
// so the guest boots without interaction.
func provisionMacOSDisk(ctx context.Context, build, disk, guestBinary string, logger *slog.Logger) error {
	return withAttachedDisk(ctx, runTool, disk, logger, func(whole string) error {
		data, err := dataVolume(ctx, whole)
		if err != nil {
			return err
		}
		mnt := filepath.Join(build, "mnt")
		return withMountedDataVolume(ctx, runTool, data, mnt, func() error {
			return installGuestHelper(ctx, mnt, data, guestBinary, logger)
		})
	})
}

// installGuestHelper installs the guest helper on the mounted Data volume.
func installGuestHelper(ctx context.Context, mnt, data, guestBinary string, logger *slog.Logger) error {
	// A mounted image ignores file ownership by default; the guest's launchd
	// loads the daemon only when root owns it.
	if _, err := runTool(ctx, diskutilPath, "enableOwnership", data); err != nil {
		return fmt.Errorf("imagebuild: enable ownership on the guest Data volume: %w", err)
	}
	if err := writeGuestFile(mnt, "/private/var/db/.AppleSetupDone", nil, 0o644); err != nil {
		return err
	}
	bin, err := os.Open(guestBinary)
	if err != nil {
		return err
	}
	defer bin.Close()
	content, err := io.ReadAll(bin)
	if err != nil {
		return err
	}
	if err := writeGuestFile(mnt, macOSGuestBinary, content, 0o755); err != nil {
		return err
	}
	if err := writeGuestFile(mnt, macOSGuestPlist, []byte(macOSGuestDaemon), 0o644); err != nil {
		return err
	}
	logger.Info("guest helper installed on the macOS guest disk")
	return nil
}

// writeGuestFile writes a root-owned file at the guest path below the
// mounted Data volume, creating missing directories owned by root.
func writeGuestFile(mnt, guestPath string, content []byte, mode os.FileMode) error {
	p := filepath.Join(mnt, filepath.FromSlash(guestPath))
	// Create the parent directories one by one so that each new one is
	// owned by root with mode 0755.
	rel, err := filepath.Rel(mnt, filepath.Dir(p))
	if err != nil {
		return err
	}
	cur := mnt
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o755); err == nil {
			if err := os.Lchown(cur, 0, 0); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		return fmt.Errorf("imagebuild: write %s on the guest disk: %w", guestPath, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		return err
	}
	return os.Lchown(p, 0, 0)
}
