// SPDX-License-Identifier: FSL-1.1-ALv2

package profile

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/validate"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
)

// Rules of the Runner profiles table of the contract.
const (
	maxNameLength           = 40
	MaxQueueWait            = 24 * time.Hour
	MaxCodexQueueWait       = 5 * time.Minute
	MaxWarmCount            = 10
	maxWorkspacePath        = 1024
	maxVCPUCount            = 1024
	maxMemoryMiB      int64 = 16 << 20
	maxDiskGiB        int64 = 1 << 20
)

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// VerifiedCodexGuestOSes are the guest OSes on which `codex exec-server`
// was verified on real devices. OpenAI documents no host OS for it, and
// the contract lets SandRun accept only verified guest OSes. Interim rule
// (recorded in the contract): the Codex adapter exists and installs and
// starts the executor on Linux and macOS guests, but it was verified only
// against fakes, so no guest OS is verified yet and an AGENT_SESSION
// profile of an OPENAI_CODEX integration is rejected for every guest OS.
//
// How an acceptance run adds a verified guest OS: run a build of sandrun-api
// whose layers accept the guest OS under test (layers.Options.
// CodexGuestOSes, which the in-process tests also use, or a local change
// of this list), and run the full Codex session lifecycle on a real
// device of that guest OS. When it passes, add the guest OS here (only
// CodexCandidateGuestOSes can pass) and record it with the run in the
// OpenAI Codex section of the contract, in the same change.
var VerifiedCodexGuestOSes = []sandrunv1.GuestOs{}

// CodexCandidateGuestOSes are the guest OSes for which the Codex adapter
// has install and start scripts (POSIX shell). Windows has none: the
// adapter fails the preparation of a Windows guest.
var CodexCandidateGuestOSes = []sandrunv1.GuestOs{sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_MACOS}

// Rules are the validation inputs that a deployment or a test can change.
type Rules struct {
	// CodexGuestOSes are the guest OSes that OPENAI_CODEX profiles may use.
	CodexGuestOSes []sandrunv1.GuestOs
}

// DefaultRules returns the production rules.
func DefaultRules() Rules { return Rules{CodexGuestOSes: VerifiedCodexGuestOSes} }

// AgentGuestOSes returns the guest OSes that an AGENT_SESSION profile of a
// vendor may use (the Agent integrations table of the contract), with the
// production rules.
//
// Cursor and Devin workers run on Linux, macOS and Windows. Devin has no
// worker binary for macOS on x64 and Windows on arm64; a profile cannot
// select those, because the OwnSand guest architecture is the device
// architecture, OwnSand runs macOS guests on Apple silicon only and builds
// Windows images with an amd64 guest helper only. The Devin Prepare step
// still checks the guest architecture and fails clearly.
func AgentGuestOSes(vendor sandrunv1.AgentVendor) []sandrunv1.GuestOs {
	return DefaultRules().AgentGuestOSes(vendor)
}

// AgentGuestOSes returns the guest OSes of a vendor under the rules.
func (r Rules) AgentGuestOSes(vendor sandrunv1.AgentVendor) []sandrunv1.GuestOs {
	all := []sandrunv1.GuestOs{sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_MACOS, sandrunv1.GuestOs_GUEST_OS_WINDOWS}
	switch vendor {
	case sandrunv1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		return []sandrunv1.GuestOs{sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_MACOS}
	case sandrunv1.AgentVendor_AGENT_VENDOR_CURSOR, sandrunv1.AgentVendor_AGENT_VENDOR_DEVIN:
		return all
	case sandrunv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		return r.CodexGuestOSes
	}
	return nil
}

// DefaultPriority is the priority of a new profile without one.
func DefaultPriority(kind sandrunv1.WorkloadKind) sandrunv1.Priority {
	if kind == sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB {
		return sandrunv1.Priority_PRIORITY_HIGH
	}
	return sandrunv1.Priority_PRIORITY_NORMAL
}

// ValidWorkspacePath reports whether path is an absolute guest path in the
// syntax of guest: `/...` for Linux and macOS, `X:\...` for Windows. The
// path has no `.` or `..` segment, no empty segment and no trailing
// separator, because sync keeps its manifest and lock next to the workspace
// (`<workspace_path>.sandrun-sync.json`). Windows path segments also exclude
// reserved device names.
func ValidWorkspacePath(guest sandrunv1.GuestOs, path string) bool {
	if path == "" || len(path) > maxWorkspacePath || !utf8.ValidString(path) {
		return false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	var sep string
	var rest string
	switch guest {
	case sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_MACOS:
		if path[0] != '/' {
			return false
		}
		sep, rest = "/", path[1:]
	case sandrunv1.GuestOs_GUEST_OS_WINDOWS:
		if len(path) < 4 || !isLetter(path[0]) || path[1] != ':' || path[2] != '\\' {
			return false
		}
		sep, rest = `\`, path[3:]
		if strings.ContainsAny(rest, `/:*?"<>|`) {
			return false
		}
	default:
		return false
	}
	if rest == "" {
		return false
	}
	for _, seg := range strings.Split(rest, sep) {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if guest == sandrunv1.GuestOs_GUEST_OS_WINDOWS && (strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") || isWindowsDeviceName(seg)) {
			return false
		}
	}
	return true
}

// isWindowsDeviceName applies the same basename rule as the SandRun Go SDK's
// file sync validation. Windows reserves these names even with an extension.
func isWindowsDeviceName(segment string) bool {
	base, _, _ := strings.Cut(segment, ".")
	switch strings.ToUpper(strings.TrimRight(base, " ")) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "COM¹", "COM²", "COM³",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "LPT¹", "LPT²", "LPT³":
		return true
	}
	return false
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// Validate checks every field of p against the Runner profiles table.
// vendor is the vendor of the profile's agent integration (UNSPECIFIED for
// the other kinds). It does not check the OwnSand template, which needs an
// OwnSand call (Profiles.checkTemplate). It uses the production rules.
func Validate(p *Profile, vendor sandrunv1.AgentVendor) error {
	return DefaultRules().Validate(p, vendor)
}

// Validate checks p under the rules.
func (rules Rules) Validate(p *Profile, vendor sandrunv1.AgentVendor) error {
	if len(p.Name) == 0 || len(p.Name) > maxNameLength || !namePattern.MatchString(p.Name) {
		return api.ErrInvalidf("invalid name: must be lowercase kebab-case, 1 to %d characters", maxNameLength)
	}
	switch p.Kind {
	case sandrunv1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION:
	default:
		return api.ErrInvalidf("invalid workload_kind: must be GITHUB_JOB, AGENT_SESSION or DEV_SESSION")
	}
	switch p.GuestOS {
	case sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_MACOS, sandrunv1.GuestOs_GUEST_OS_WINDOWS:
	default:
		return api.ErrInvalidf("invalid guest_os: must be LINUX, MACOS or WINDOWS")
	}
	if p.Kind == sandrunv1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION {
		if p.AgentIntegrationID == "" {
			return api.ErrInvalidf("invalid agent_integration_id: is required for AGENT_SESSION")
		}
		if !slices.Contains(rules.AgentGuestOSes(vendor), p.GuestOS) {
			if vendor == sandrunv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX {
				if len(rules.CodexGuestOSes) == 0 {
					return api.ErrInvalidf("invalid guest_os: no guest OS is verified for OPENAI_CODEX yet")
				}
				return api.ErrInvalidf("invalid guest_os: %s is not verified for OPENAI_CODEX", p.GuestOS)
			}
			return api.ErrInvalidf("invalid guest_os: %s does not support %s", vendor, p.GuestOS)
		}
	} else if p.AgentIntegrationID != "" {
		return api.ErrInvalidf("invalid agent_integration_id: must be absent for %s", p.Kind)
	}
	if p.Kind == sandrunv1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION {
		if !ValidWorkspacePath(p.GuestOS, p.WorkspacePath) {
			if p.GuestOS == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
				return api.ErrInvalidf("invalid workspace_path: must be an absolute Windows path without reserved device names or a trailing separator")
			}
			return api.ErrInvalidf("invalid workspace_path: must be an absolute %s path without a trailing separator", p.GuestOS)
		}
	} else if p.WorkspacePath != "" {
		return api.ErrInvalidf("invalid workspace_path: must be absent for %s", p.Kind)
	}
	switch {
	case p.BaseImage != sandrunv1.GuestOs_GUEST_OS_UNSPECIFIED && p.TemplateID != "":
		return api.ErrInvalidf("invalid source: set exactly one of base_image and ownsand_template_id")
	case p.BaseImage != sandrunv1.GuestOs_GUEST_OS_UNSPECIFIED:
		if p.BaseImage != p.GuestOS {
			return api.ErrInvalidf("invalid source.base_image: must be the base image of guest_os")
		}
	case p.TemplateID != "":
		if _, err := api.NormalizeID("ownsand_template_id", p.TemplateID); err != nil {
			return err
		}
	default:
		return api.ErrInvalidf("invalid source: base_image or ownsand_template_id is required")
	}
	r := p.Resources
	switch {
	case r.VCPUCount <= 0:
		return api.ErrInvalidf("invalid resources.vcpu_count: must be greater than zero")
	case r.VCPUCount > maxVCPUCount:
		return api.ErrInvalidf("invalid resources.vcpu_count: must be at most %d", maxVCPUCount)
	case r.MemoryMiB <= 0:
		return api.ErrInvalidf("invalid resources.memory_mib: must be greater than zero")
	case r.MemoryMiB > maxMemoryMiB:
		return api.ErrInvalidf("invalid resources.memory_mib: must be at most %d", maxMemoryMiB)
	case r.DiskGiB <= 0:
		return api.ErrInvalidf("invalid resources.disk_gib: must be greater than zero")
	case r.DiskGiB > maxDiskGiB:
		return api.ErrInvalidf("invalid resources.disk_gib: must be at most %d", maxDiskGiB)
	}
	if p.NetworkMode != sandrunv1.NetworkMode_NETWORK_MODE_OUTBOUND {
		return api.ErrInvalidf("invalid network_mode: must be OUTBOUND")
	}
	if err := validate.TTL(durationpb.New(p.IdleTTL), durationpb.New(p.MaxTTL)); err != nil {
		return api.ErrInvalidf("invalid ttl: %v", err)
	}
	switch p.Priority {
	case sandrunv1.Priority_PRIORITY_HIGH, sandrunv1.Priority_PRIORITY_NORMAL, sandrunv1.Priority_PRIORITY_LOW:
	default:
		return api.ErrInvalidf("invalid priority: must be HIGH, NORMAL or LOW")
	}
	maxWait := MaxQueueWait
	if vendor == sandrunv1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX {
		maxWait = MaxCodexQueueWait
	}
	if p.MaxQueueWait <= 0 || p.MaxQueueWait > maxWait {
		return api.ErrInvalidf("invalid max_queue_wait: must be greater than zero and at most %s", maxWait)
	}
	if p.WarmCount < 0 || p.WarmCount > MaxWarmCount {
		return api.ErrInvalidf("invalid warm_count: must be 0 to %d", MaxWarmCount)
	}
	return nil
}
