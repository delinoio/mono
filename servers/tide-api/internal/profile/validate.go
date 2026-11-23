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
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
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

// VerifiedCodexGuestOSes are the guest OSes on which the OpenAI Codex
// adapter verified `codex exec-server`. OpenAI documents no host OS for it,
// and the contract lets tide accept only verified guest OSes. Interim rule
// (recorded in the contract): no Codex adapter exists yet, so the list is
// empty and an AGENT_SESSION profile of an OPENAI_CODEX integration is
// rejected for every guest OS. The Codex adapter change adds the guest OSes
// that it verified here and in the contract.
var VerifiedCodexGuestOSes = []tidev1.GuestOs{}

// AgentGuestOSes returns the guest OSes that an AGENT_SESSION profile of a
// vendor may use (the Agent integrations table of the contract).
//
// Cursor and Devin workers run on Linux, macOS and Windows. Devin has no
// worker binary for macOS on x64 and Windows on arm64; a profile cannot
// select those, because the OwnSand guest architecture is the device
// architecture, OwnSand runs macOS guests on Apple silicon only and builds
// Windows images with an amd64 guest helper only. The Devin Prepare step
// still checks the guest architecture and fails clearly.
func AgentGuestOSes(vendor tidev1.AgentVendor) []tidev1.GuestOs {
	all := []tidev1.GuestOs{tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS, tidev1.GuestOs_GUEST_OS_WINDOWS}
	switch vendor {
	case tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE:
		return []tidev1.GuestOs{tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS}
	case tidev1.AgentVendor_AGENT_VENDOR_CURSOR, tidev1.AgentVendor_AGENT_VENDOR_DEVIN:
		return all
	case tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX:
		return VerifiedCodexGuestOSes
	}
	return nil
}

// DefaultPriority is the priority of a new profile without one.
func DefaultPriority(kind tidev1.WorkloadKind) tidev1.Priority {
	if kind == tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB {
		return tidev1.Priority_PRIORITY_HIGH
	}
	return tidev1.Priority_PRIORITY_NORMAL
}

// ValidWorkspacePath reports whether path is an absolute guest path in the
// syntax of guest: `/...` for Linux and macOS, `X:\...` for Windows. The
// path has no `.` or `..` segment, no empty segment and no trailing
// separator, because sync keeps its manifest and lock next to the workspace
// (`<workspace_path>.tide-sync.json`).
func ValidWorkspacePath(guest tidev1.GuestOs, path string) bool {
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
	case tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS:
		if path[0] != '/' {
			return false
		}
		sep, rest = "/", path[1:]
	case tidev1.GuestOs_GUEST_OS_WINDOWS:
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
		if guest == tidev1.GuestOs_GUEST_OS_WINDOWS && (strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ")) {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// Validate checks every field of p against the Runner profiles table.
// vendor is the vendor of the profile's agent integration (UNSPECIFIED for
// the other kinds). It does not check the OwnSand template, which needs an
// OwnSand call (Profiles.checkTemplate).
func Validate(p *Profile, vendor tidev1.AgentVendor) error {
	if len(p.Name) == 0 || len(p.Name) > maxNameLength || !namePattern.MatchString(p.Name) {
		return api.ErrInvalidf("invalid name: must be lowercase kebab-case, 1 to %d characters", maxNameLength)
	}
	switch p.Kind {
	case tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION:
	default:
		return api.ErrInvalidf("invalid workload_kind: must be GITHUB_JOB, AGENT_SESSION or DEV_SESSION")
	}
	switch p.GuestOS {
	case tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS, tidev1.GuestOs_GUEST_OS_WINDOWS:
	default:
		return api.ErrInvalidf("invalid guest_os: must be LINUX, MACOS or WINDOWS")
	}
	if p.Kind == tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION {
		if p.AgentIntegrationID == "" {
			return api.ErrInvalidf("invalid agent_integration_id: is required for AGENT_SESSION")
		}
		if !slices.Contains(AgentGuestOSes(vendor), p.GuestOS) {
			if vendor == tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX {
				return api.ErrInvalidf("invalid guest_os: no guest OS is verified for OPENAI_CODEX yet")
			}
			return api.ErrInvalidf("invalid guest_os: %s does not support %s", vendor, p.GuestOS)
		}
	} else if p.AgentIntegrationID != "" {
		return api.ErrInvalidf("invalid agent_integration_id: must be absent for %s", p.Kind)
	}
	if p.Kind == tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION {
		if !ValidWorkspacePath(p.GuestOS, p.WorkspacePath) {
			return api.ErrInvalidf("invalid workspace_path: must be an absolute %s path without a trailing separator", p.GuestOS)
		}
	} else if p.WorkspacePath != "" {
		return api.ErrInvalidf("invalid workspace_path: must be absent for %s", p.Kind)
	}
	switch {
	case p.BaseImage != tidev1.GuestOs_GUEST_OS_UNSPECIFIED && p.TemplateID != "":
		return api.ErrInvalidf("invalid source: set exactly one of base_image and ownsand_template_id")
	case p.BaseImage != tidev1.GuestOs_GUEST_OS_UNSPECIFIED:
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
	if r.VCPUCount <= 0 || r.VCPUCount > maxVCPUCount || r.MemoryMiB <= 0 || r.MemoryMiB > maxMemoryMiB || r.DiskGiB <= 0 || r.DiskGiB > maxDiskGiB {
		return api.ErrInvalidf("invalid resources: vcpu_count, memory_mib and disk_gib must be greater than zero")
	}
	if p.NetworkMode != tidev1.NetworkMode_NETWORK_MODE_OUTBOUND {
		return api.ErrInvalidf("invalid network_mode: must be OUTBOUND")
	}
	if err := validate.TTL(durationpb.New(p.IdleTTL), durationpb.New(p.MaxTTL)); err != nil {
		return api.ErrInvalidf("invalid ttl: %v", err)
	}
	switch p.Priority {
	case tidev1.Priority_PRIORITY_HIGH, tidev1.Priority_PRIORITY_NORMAL, tidev1.Priority_PRIORITY_LOW:
	default:
		return api.ErrInvalidf("invalid priority: must be HIGH, NORMAL or LOW")
	}
	maxWait := MaxQueueWait
	if vendor == tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX {
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
