// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file holds the attach and detach logic of the macOS guest disk without
// a build tag, so that unit tests exercise it on every host. Only the macOS
// build (macos_provision_darwin.go) wires it to the real host tools.

// hdiutilPath is the host tool that attaches and detaches the guest disk.
const hdiutilPath = "/usr/bin/hdiutil"

// detachTimeout bounds each detach attempt of the guest disk. The detach runs
// on its own context, because the build context is often canceled already
// when the cleanup runs, and a canceled context would leave the disk attached.
const detachTimeout = 30 * time.Second

// errNoGUIDPartitionTable reports an attached guest disk without a GUID
// partition table, which the macOS installer always creates.
var errNoGUIDPartitionTable = errors.New("imagebuild: the attached guest disk has no GUID partition table")

// hostRunner runs a host tool and returns its standard output.
type hostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// runHostTool is the hostRunner that runs the real tool.
func runHostTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
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
	entities, _ := parseAttachEntities(out)
	for _, e := range entities {
		m := wholeDiskPattern.FindStringSubmatch(e.devEntry)
		if m == nil {
			continue
		}
		if e.contentHint == "GUID_partition_scheme" {
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

type attachEntity struct {
	devEntry    string
	contentHint string
}

// parseAttachEntities reads the `system-entities` array of an attach plist.
// It returns the entities read so far together with the first XML error.
func parseAttachEntities(out []byte) ([]attachEntity, error) {
	dec := xml.NewDecoder(bytes.NewReader(out))
	dec.Strict = false
	var (
		entities []attachEntity
		depth    int // plist=1, root dict=2, system-entities array=3, entity dict=4
		key      string
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return entities, nil
			}
			return entities, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "key":
				if err := dec.DecodeElement(&key, &t); err != nil {
					return entities, err
				}
			case t.Name.Local == "string":
				var value string
				if err := dec.DecodeElement(&value, &t); err != nil {
					return entities, err
				}
				if depth == 4 && len(entities) > 0 {
					switch e := &entities[len(entities)-1]; key {
					case "dev-entry":
						e.devEntry = value
					case "content-hint":
						e.contentHint = value
					}
				}
			default:
				depth++
				if t.Name.Local == "dict" && depth == 4 {
					entities = append(entities, attachEntity{})
				}
			}
		case xml.EndElement:
			depth--
		}
	}
}

// withAttachedDisk attaches the raw disk image without mounting it, calls use
// with the whole-disk device (for example disk4) and detaches that device
// before it returns, whichever way use or a later step fails. The cleanup
// starts as soon as the attach has succeeded and touches only the device that
// this attach reported. A cleanup failure is reported only when no earlier
// error exists, so the original error is never replaced.
func withAttachedDisk(ctx context.Context, run hostRunner, disk string, logger *slog.Logger, use func(whole string) error) (err error) {
	out, err := run(ctx, hdiutilPath, "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", "-plist", disk)
	if err != nil {
		return fmt.Errorf("imagebuild: attach the guest disk: %w", err)
	}
	whole, guid := attachedWholeDisk(out)
	if whole == "" {
		return errors.New("imagebuild: the attach output of the guest disk names no device")
	}
	defer func() {
		if derr := detachDisk(run, whole, logger); derr != nil && err == nil {
			err = derr
		}
	}()
	if !guid {
		return errNoGUIDPartitionTable
	}
	return use(whole)
}

// detachDisk detaches the whole-disk device, and forces the detach when the
// first attempt fails. Each attempt runs on a fresh bounded context that the
// build context cannot cancel.
func detachDisk(run hostRunner, whole string, logger *slog.Logger) error {
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
	logger.Warn("detach the guest disk failed; forcing", "device", dev, "error", derr)
	return attempt("detach", "-force", dev)
}
