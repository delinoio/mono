// SPDX-License-Identifier: Apache-2.0

package pins

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestEmbeddedPinsAreComplete(t *testing.T) {
	p, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, runtime := range p.Runtimes {
		// Renovate changes Version; the checksums must follow in the same
		// change.
		if runtime.ChecksumsVersion != runtime.Version {
			t.Errorf("runtime %s: checksums are for %s, not %s; copy the SHA-256 values of the new release", name, runtime.ChecksumsVersion, runtime.Version)
		}
		if runtime.ReportedVersion != "v"+runtime.Version {
			t.Errorf("runtime %s: reported version %q does not match %q", name, runtime.ReportedVersion, runtime.Version)
		}
		for _, target := range Targets {
			archive, ok := runtime.Targets[target]
			if !ok {
				t.Errorf("runtime %s has no archive for %s", name, target)
				continue
			}
			if !sha256Pattern.MatchString(archive.SHA256) {
				t.Errorf("runtime %s %s: malformed SHA-256", name, target)
			}
			if !strings.HasPrefix(archive.URL, "https://nodejs.org/dist/v"+runtime.Version+"/") || !strings.Contains(archive.URL, archive.Root) {
				t.Errorf("runtime %s %s: URL %s does not match version %s and root %s", name, target, archive.URL, runtime.Version, archive.Root)
			}
			if archive.Format != ArchiveTarGz && archive.Format != ArchiveZip {
				t.Errorf("runtime %s %s: unknown format %q", name, target, archive.Format)
			}
			if archive.Executable == "" || archive.NpmCli == "" || archive.GlobalModules == "" {
				t.Errorf("runtime %s %s: incomplete layout", name, target)
			}
		}
	}
	for _, vendor := range []sandcrewv1.AgentVendor{sandcrewv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, sandcrewv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX} {
		v, ok := p.Vendor(vendor)
		if !ok {
			t.Errorf("no pins for %s", vendor)
			continue
		}
		for _, target := range Targets {
			if !v.Supports(target) {
				t.Errorf("%s does not support %s", vendor, target)
			}
		}
	}
}

// The adapter and agent versions of pins.json equal the committed npm
// package and lockfile, and every locked package has an integrity hash.
func TestNpmLockfilesMatchPins(t *testing.T) {
	p, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, vendor := range p.Vendors {
		packageJSON, packageLock, err := NpmFiles(vendor.PackageDir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var manifest struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		if err := json.Unmarshal(packageJSON, &manifest); err != nil {
			t.Fatalf("%s: package.json: %v", name, err)
		}
		if got := manifest.Dependencies[vendor.Adapter.Name]; got != vendor.Adapter.Version || len(manifest.Dependencies) != 1 {
			t.Errorf("%s: package.json depends on %v, want only %s %s", name, manifest.Dependencies, vendor.Adapter.Name, vendor.Adapter.Version)
		}
		var lock struct {
			LockfileVersion int `json:"lockfileVersion"`
			Packages        map[string]struct {
				Version   string `json:"version"`
				Resolved  string `json:"resolved"`
				Integrity string `json:"integrity"`
				Link      bool   `json:"link"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(packageLock, &lock); err != nil {
			t.Fatalf("%s: package-lock.json: %v", name, err)
		}
		if lock.LockfileVersion != 3 {
			t.Errorf("%s: lockfile version %d, want 3", name, lock.LockfileVersion)
		}
		if got := lock.Packages["node_modules/"+vendor.Adapter.Name].Version; got != vendor.Adapter.Version {
			t.Errorf("%s: lockfile has adapter %s, want %s", name, got, vendor.Adapter.Version)
		}
		if got := lock.Packages["node_modules/"+vendor.Agent.Name].Version; got != vendor.Agent.Version {
			t.Errorf("%s: lockfile has agent %s, want %s", name, got, vendor.Agent.Version)
		}
		for path, pkg := range lock.Packages {
			if path == "" || pkg.Link {
				continue
			}
			if !strings.HasPrefix(pkg.Integrity, "sha512-") {
				t.Errorf("%s: %s has no sha512 integrity", name, path)
			}
			if !strings.HasPrefix(pkg.Resolved, "https://registry.npmjs.org/") {
				t.Errorf("%s: %s resolves outside the npm registry: %s", name, path, pkg.Resolved)
			}
		}
	}
}

func TestTargetFor(t *testing.T) {
	if got, ok := TargetFor("windows", "arm64"); !ok || got != TargetWindowsARM64 {
		t.Fatalf("TargetFor(windows, arm64) = %q, %v", got, ok)
	}
	if _, ok := TargetFor("darwin", "amd64"); ok {
		t.Fatal("darwin-amd64 is not a release target")
	}
}
