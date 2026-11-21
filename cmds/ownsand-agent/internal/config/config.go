// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config holds the agent configuration. Every setting comes from a
// command-line flag or an OWNSAND_AGENT_* environment variable; the
// control plane origin is also recorded in the state directory at
// enrollment.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/delinoio/mono/cmds/internal/loglevel"
)

// Environment variables of the agent.
const (
	EnvStateDir           = "OWNSAND_AGENT_STATE_DIR"
	EnvAPIURL             = "OWNSAND_AGENT_API_URL"
	EnvRuntime            = "OWNSAND_AGENT_RUNTIME"
	EnvGuestBinary        = "OWNSAND_AGENT_GUEST_BINARY"
	EnvReleaseManifestURL = "OWNSAND_AGENT_RELEASE_MANIFEST_URL"
	EnvReleasePublicKeys  = "OWNSAND_AGENT_RELEASE_PUBLIC_KEYS"
	EnvLogLevel           = "OWNSAND_AGENT_LOG_LEVEL"
	EnvFirecrackerBinary  = "OWNSAND_AGENT_FIRECRACKER_BINARY"
	EnvLinuxKernel        = "OWNSAND_AGENT_LINUX_KERNEL"
	EnvLinuxRootfs        = "OWNSAND_AGENT_LINUX_ROOTFS"
)

// EnvNames lists every environment variable of the agent. The service
// definitions carry these variables into the service environment; add a
// new variable to this list together with its constant. The first seven
// names keep the order in which service definitions have always listed
// them.
var EnvNames = []string{
	EnvStateDir,
	EnvAPIURL,
	EnvRuntime,
	EnvGuestBinary,
	EnvReleaseManifestURL,
	EnvReleasePublicKeys,
	EnvLogLevel,
	EnvFirecrackerBinary,
	EnvLinuxKernel,
	EnvLinuxRootfs,
}

// DefaultAPIURL is the production control plane origin.
const DefaultAPIURL = "https://ownsand.api.delino.io"

// RuntimeKind selects the sandbox runtime backend.
type RuntimeKind int

const (
	RuntimeKindUnspecified RuntimeKind = iota
	// RuntimeKindFirecracker runs each sandbox in a Firecracker microVM
	// (Linux hosts).
	RuntimeKindFirecracker
	// RuntimeKindProcess runs the real guest helper as a host process for
	// each sandbox. It is a test runtime: it gives no isolation and must
	// never serve real workloads. End-to-end tests use it where no
	// hypervisor is available.
	RuntimeKindProcess
	// RuntimeKindVirtualization is the macOS runtime
	// (Virtualization.framework); not implemented in this version.
	RuntimeKindVirtualization
	// RuntimeKindHyperV is the Windows runtime (Hyper-V through the Host
	// Compute Service); not implemented in this version.
	RuntimeKindHyperV
)

func (k RuntimeKind) String() string {
	switch k {
	case RuntimeKindFirecracker:
		return "firecracker"
	case RuntimeKindProcess:
		return "process"
	case RuntimeKindVirtualization:
		return "virtualization"
	case RuntimeKindHyperV:
		return "hyperv"
	}
	return "unspecified"
}

// ParseRuntimeKind parses a runtime name.
func ParseRuntimeKind(s string) (RuntimeKind, error) {
	switch s {
	case "firecracker":
		return RuntimeKindFirecracker, nil
	case "process":
		return RuntimeKindProcess, nil
	case "virtualization":
		return RuntimeKindVirtualization, nil
	case "hyperv":
		return RuntimeKindHyperV, nil
	}
	return RuntimeKindUnspecified, fmt.Errorf("config: unknown runtime %q (want firecracker, virtualization, hyperv or process)", s)
}

// DefaultRuntime returns the production runtime of the host OS.
func DefaultRuntime() RuntimeKind {
	switch runtime.GOOS {
	case "linux":
		return RuntimeKindFirecracker
	case "darwin":
		return RuntimeKindVirtualization
	case "windows":
		return RuntimeKindHyperV
	}
	return RuntimeKindUnspecified
}

// Config is the agent configuration.
type Config struct {
	StateDir string
	// APIURLOverride replaces the control plane origin recorded at
	// enrollment, when set.
	APIURLOverride string
	Runtime        RuntimeKind
	// GuestBinary is the guest helper binary of the process runtime.
	GuestBinary        string
	ReleaseManifestURL string
	// ReleasePublicKeys are added to the built-in release keys.
	ReleasePublicKeys []ed25519.PublicKey
	LogLevel          slog.Level
	// Firecracker runtime: the firecracker executable and the Linux base
	// image. Empty image paths mean images/linux/ in the state directory.
	FirecrackerBinary string
	LinuxKernel       string
	LinuxRootfs       string
}

// DefaultStateDir returns the state directory of the host OS.
func DefaultStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ownsand-agent"
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "ownsand-agent")
	}
	return "/var/lib/ownsand-agent"
}

// Lookup reads an environment variable.
type Lookup func(string) (string, bool)

// FromEnv builds the configuration from environment variables and
// defaults.
func FromEnv(lookup Lookup) (Config, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	cfg := Config{
		StateDir:           get(EnvStateDir),
		APIURLOverride:     get(EnvAPIURL),
		GuestBinary:        get(EnvGuestBinary),
		ReleaseManifestURL: get(EnvReleaseManifestURL),
		Runtime:            DefaultRuntime(),
		FirecrackerBinary:  get(EnvFirecrackerBinary),
		LinuxKernel:        get(EnvLinuxKernel),
		LinuxRootfs:        get(EnvLinuxRootfs),
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	if v := get(EnvRuntime); v != "" {
		kind, err := ParseRuntimeKind(v)
		if err != nil {
			return Config{}, err
		}
		cfg.Runtime = kind
	}
	if v := get(EnvLogLevel); v != "" {
		level, err := loglevel.Parse(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid %s %q", EnvLogLevel, v)
		}
		cfg.LogLevel = level
	}
	keys, err := ParsePublicKeys(get(EnvReleasePublicKeys))
	if err != nil {
		return Config{}, err
	}
	cfg.ReleasePublicKeys = keys
	return cfg, nil
}

// ParsePublicKeys parses a comma-separated list of base64 (standard or URL
// alphabet, padded or not) Ed25519 public keys.
func ParsePublicKeys(s string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var raw []byte
		var err error
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if raw, err = enc.DecodeString(part); err == nil {
				break
			}
		}
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("config: %s holds a value that is not a base64 Ed25519 public key", EnvReleasePublicKeys)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}
