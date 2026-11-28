// SPDX-License-Identifier: Apache-2.0

// Package pins holds the agent components that this bridge release installs:
// the pinned runtime archives with their SHA-256 and, for each vendor, the
// ACP adapter npm package with a committed package-lock.json whose integrity
// hashes cover every package. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#pinned-agent-installation.
package pins

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

//go:embed pins.json npm
var files embed.FS

// Target is a release target of the bridge, for example "linux-amd64".
type Target string

// The release targets of the bridge.
const (
	TargetLinuxAMD64   Target = "linux-amd64"
	TargetLinuxARM64   Target = "linux-arm64"
	TargetDarwinARM64  Target = "darwin-arm64"
	TargetWindowsAMD64 Target = "windows-amd64"
	TargetWindowsARM64 Target = "windows-arm64"
)

// Targets lists every release target.
var Targets = []Target{TargetLinuxAMD64, TargetLinuxARM64, TargetDarwinARM64, TargetWindowsAMD64, TargetWindowsARM64}

// TargetFor returns the release target of goos and goarch, or false when the
// bridge has none.
func TargetFor(goos, goarch string) (Target, bool) {
	target := Target(goos + "-" + goarch)
	for _, known := range Targets {
		if known == target {
			return target, true
		}
	}
	return "", false
}

// ArchiveFormat is the format of a runtime archive.
type ArchiveFormat string

// The supported archive formats.
const (
	ArchiveTarGz ArchiveFormat = "tar.gz"
	ArchiveZip   ArchiveFormat = "zip"
)

// RuntimeName names a runtime, for example "node".
type RuntimeName string

// RuntimeNode is Node.js.
const RuntimeNode RuntimeName = "node"

// Pins is the content of pins.json.
type Pins struct {
	SchemaVersion int                               `json:"schemaVersion"`
	Runtimes      map[RuntimeName]Runtime           `json:"runtimes"`
	Vendors       map[string]Vendor                 `json:"vendors"`
	vendorsByEnum map[sandcrewv1.AgentVendor]Vendor `json:"-"`
}

// Runtime is a pinned runtime with one archive per release target.
type Runtime struct {
	// Renovate reads this marker and the version that follows it.
	Renovate string `json:"renovate"`
	Version  string `json:"version"`
	// ChecksumsVersion is the version that the target checksums belong to.
	// It differs from Version only in a Renovate update that has not
	// copied the new checksums yet; tests fail until it equals Version.
	ChecksumsVersion string `json:"checksumsVersion"`
	// ReportedVersion is the output of `<executable> --version` for this
	// version, which the preinstalled check compares.
	ReportedVersion string                    `json:"reportedVersion"`
	Targets         map[Target]RuntimeArchive `json:"targets"`
}

// RuntimeArchive is the official archive of a runtime for one target.
type RuntimeArchive struct {
	URL    string        `json:"url"`
	SHA256 string        `json:"sha256"`
	Format ArchiveFormat `json:"format"`
	// Root is the top-level directory of the archive.
	Root string `json:"root"`
	// Executable is the runtime executable, relative to Root.
	Executable string `json:"executable"`
	// NpmCli is the npm command line script, relative to Root.
	NpmCli string `json:"npmCli"`
	// GlobalModules is the global node_modules directory, relative to Root.
	GlobalModules string `json:"globalModules"`
}

// Vendor is the pinned agent of one vendor.
type Vendor struct {
	Runtime RuntimeName `json:"runtime"`
	// PackageDir is the directory under npm/ with the package.json and the
	// package-lock.json of the adapter.
	PackageDir string   `json:"packageDir"`
	Adapter    Package  `json:"adapter"`
	Agent      Package  `json:"agent"`
	Targets    []Target `json:"targets"`
}

// Package is an npm package with an exact version.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Entry is the script that starts the adapter, relative to the package
	// directory.
	Entry string `json:"entry,omitempty"`
}

// Supports reports whether the vendor runs on target.
func (v Vendor) Supports(target Target) bool {
	for _, t := range v.Targets {
		if t == target {
			return true
		}
	}
	return false
}

// Load parses the embedded pins.
func Load() (*Pins, error) {
	data, err := files.ReadFile("pins.json")
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses and validates pins.json content.
func Parse(data []byte) (*Pins, error) {
	var p Pins
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("pins: parse: %w", err)
	}
	if p.SchemaVersion != 1 {
		return nil, fmt.Errorf("pins: unsupported schema version %d", p.SchemaVersion)
	}
	p.vendorsByEnum = make(map[sandcrewv1.AgentVendor]Vendor)
	for name, vendor := range p.Vendors {
		value, ok := sandcrewv1.AgentVendor_value[name]
		if !ok || value == 0 {
			return nil, fmt.Errorf("pins: unknown vendor %q", name)
		}
		if _, ok := p.Runtimes[vendor.Runtime]; !ok {
			return nil, fmt.Errorf("pins: vendor %s needs unknown runtime %q", name, vendor.Runtime)
		}
		if vendor.Adapter.Name == "" || vendor.Adapter.Version == "" || vendor.Adapter.Entry == "" {
			return nil, fmt.Errorf("pins: vendor %s has an incomplete adapter", name)
		}
		p.vendorsByEnum[sandcrewv1.AgentVendor(value)] = vendor
	}
	return &p, nil
}

// Vendor returns the pins of vendor.
func (p *Pins) Vendor(vendor sandcrewv1.AgentVendor) (Vendor, bool) {
	v, ok := p.vendorsByEnum[vendor]
	return v, ok
}

// NpmFiles returns the package.json and package-lock.json of a vendor
// package directory.
func NpmFiles(packageDir string) (packageJSON, packageLock []byte, err error) {
	dir := path.Join("npm", packageDir)
	if packageJSON, err = fs.ReadFile(files, path.Join(dir, "package.json")); err != nil {
		return nil, nil, err
	}
	if packageLock, err = fs.ReadFile(files, path.Join(dir, "package-lock.json")); err != nil {
		return nil, nil, err
	}
	return packageJSON, packageLock, nil
}
