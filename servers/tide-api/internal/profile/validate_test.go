// SPDX-License-Identifier: FSL-1.1-ALv2

package profile

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

const (
	linux   = tidev1.GuestOs_GUEST_OS_LINUX
	macos   = tidev1.GuestOs_GUEST_OS_MACOS
	windows = tidev1.GuestOs_GUEST_OS_WINDOWS
)

func devProfile() *Profile {
	return &Profile{
		Name: "linux-dev", Kind: tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION, WorkspacePath: "/home/dev/work", GuestOS: linux,
		BaseImage: linux, Resources: Resources{VCPUCount: 2, MemoryMiB: 4096, DiskGiB: 20}, NetworkMode: tidev1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, Priority: tidev1.Priority_PRIORITY_NORMAL, MaxQueueWait: 10 * time.Minute,
	}
}

func agentProfile(guest tidev1.GuestOs) *Profile {
	p := devProfile()
	p.Kind, p.WorkspacePath, p.AgentIntegrationID, p.GuestOS, p.BaseImage = tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION, "", "01900000-0000-7000-8000-000000000001", guest, guest
	return p
}

func TestValidateAcceptsValidProfiles(t *testing.T) {
	if err := Validate(devProfile(), 0); err != nil {
		t.Fatal(err)
	}
	gh := devProfile()
	gh.Kind, gh.WorkspacePath, gh.GuestOS, gh.BaseImage, gh.WarmCount = tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, "", windows, windows, 10
	gh.MaxQueueWait = 24 * time.Hour
	if err := Validate(gh, 0); err != nil {
		t.Fatal(err)
	}
	tmpl := devProfile()
	tmpl.BaseImage, tmpl.TemplateID = 0, "01900000-0000-7000-8000-000000000002"
	if err := Validate(tmpl, 0); err != nil {
		t.Fatal(err)
	}
	if gh.GitHubLabel() != "tide-linux-dev" || devProfile().GitHubLabel() != "" {
		t.Fatal("github label")
	}
	if DefaultPriority(tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB) != tidev1.Priority_PRIORITY_HIGH ||
		DefaultPriority(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION) != tidev1.Priority_PRIORITY_NORMAL ||
		DefaultPriority(tidev1.WorkloadKind_WORKLOAD_KIND_AGENT_SESSION) != tidev1.Priority_PRIORITY_NORMAL {
		t.Fatal("default priorities")
	}
}

func TestValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(p *Profile){
		"empty name":            func(p *Profile) { p.Name = "" },
		"upper case name":       func(p *Profile) { p.Name = "Linux" },
		"leading hyphen":        func(p *Profile) { p.Name = "-a" },
		"double hyphen":         func(p *Profile) { p.Name = "a--b" },
		"41 characters":         func(p *Profile) { p.Name = strings.Repeat("a", 41) },
		"no kind":               func(p *Profile) { p.Kind = 0 },
		"no guest os":           func(p *Profile) { p.GuestOS = 0 },
		"integration on dev":    func(p *Profile) { p.AgentIntegrationID = "01900000-0000-7000-8000-000000000001" },
		"no workspace":          func(p *Profile) { p.WorkspacePath = "" },
		"relative workspace":    func(p *Profile) { p.WorkspacePath = "work" },
		"windows workspace":     func(p *Profile) { p.WorkspacePath = `C:\work` },
		"trailing slash":        func(p *Profile) { p.WorkspacePath = "/work/" },
		"dot dot":               func(p *Profile) { p.WorkspacePath = "/work/../etc" },
		"root workspace":        func(p *Profile) { p.WorkspacePath = "/" },
		"control character":     func(p *Profile) { p.WorkspacePath = "/wo\nrk" },
		"two sources":           func(p *Profile) { p.TemplateID = "01900000-0000-7000-8000-000000000002" },
		"no source":             func(p *Profile) { p.BaseImage = 0 },
		"other base image":      func(p *Profile) { p.BaseImage = macos },
		"bad template id":       func(p *Profile) { p.BaseImage, p.TemplateID = 0, "x" },
		"no vcpu":               func(p *Profile) { p.Resources.VCPUCount = 0 },
		"no memory":             func(p *Profile) { p.Resources.MemoryMiB = 0 },
		"no disk":               func(p *Profile) { p.Resources.DiskGiB = -1 },
		"no network mode":       func(p *Profile) { p.NetworkMode = 0 },
		"no idle ttl":           func(p *Profile) { p.IdleTTL = 0 },
		"max ttl over 7 days":   func(p *Profile) { p.MaxTTL = 7*24*time.Hour + time.Second },
		"idle above max":        func(p *Profile) { p.IdleTTL = 25 * time.Hour },
		"no priority":           func(p *Profile) { p.Priority = 0 },
		"no queue wait":         func(p *Profile) { p.MaxQueueWait = 0 },
		"queue wait over a day": func(p *Profile) { p.MaxQueueWait = 24*time.Hour + time.Second },
		"warm count 11":         func(p *Profile) { p.WarmCount = 11 },
		"negative warm count":   func(p *Profile) { p.WarmCount = -1 },
	} {
		p := devProfile()
		mutate(p)
		if err := Validate(p, 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Validate(&Profile{Name: "x", Kind: tidev1.WorkloadKind_WORKLOAD_KIND_GITHUB_JOB, GuestOS: linux, WorkspacePath: "/w"}, 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("workspace on GITHUB_JOB: %v", err)
	}
	ag := agentProfile(linux)
	ag.AgentIntegrationID = ""
	if err := Validate(ag, tidev1.AgentVendor_AGENT_VENDOR_CURSOR); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("agent without integration: %v", err)
	}
}

func TestAgentGuestOSes(t *testing.T) {
	for _, tc := range []struct {
		vendor tidev1.AgentVendor
		guest  tidev1.GuestOs
		ok     bool
	}{
		{tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, linux, true},
		{tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, macos, true},
		{tidev1.AgentVendor_AGENT_VENDOR_CLAUDE_CODE, windows, false},
		{tidev1.AgentVendor_AGENT_VENDOR_CURSOR, windows, true},
		{tidev1.AgentVendor_AGENT_VENDOR_DEVIN, macos, true},
		// Interim rule: no guest OS is verified for Codex yet.
		{tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, linux, false},
		{tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, macos, false},
		{tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX, windows, false},
	} {
		err := Validate(agentProfile(tc.guest), tc.vendor)
		if (err == nil) != tc.ok {
			t.Errorf("%v on %v: %v", tc.vendor, tc.guest, err)
		}
	}
}

func TestCodexQueueWait(t *testing.T) {
	old := VerifiedCodexGuestOSes
	VerifiedCodexGuestOSes = []tidev1.GuestOs{linux}
	defer func() { VerifiedCodexGuestOSes = old }()
	p := agentProfile(linux)
	p.MaxQueueWait = 5 * time.Minute
	if err := Validate(p, tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX); err != nil {
		t.Fatal(err)
	}
	p.MaxQueueWait = 5*time.Minute + time.Second
	if err := Validate(p, tidev1.AgentVendor_AGENT_VENDOR_OPENAI_CODEX); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over 5 minutes: %v", err)
	}
	if err := Validate(p, tidev1.AgentVendor_AGENT_VENDOR_CURSOR); err != nil {
		t.Fatalf("other vendors allow more: %v", err)
	}
}

func TestWorkspacePaths(t *testing.T) {
	for _, tc := range []struct {
		guest tidev1.GuestOs
		path  string
		ok    bool
	}{
		{linux, "/home/dev/work", true},
		{macos, "/Users/dev/work", true},
		{windows, `C:\work`, true},
		{windows, `d:\src\app`, true},
		{windows, `C:/work`, false},
		{windows, `C:\`, false},
		{windows, `\\server\share`, false},
		{windows, `C:\a\`, false},
		{windows, `C:\a?b`, false},
		{windows, `C:\a.`, false},
		{windows, "/work", false},
		{linux, `C:\work`, false},
		{linux, "/a//b", false},
		{linux, "/a/./b", false},
		{linux, "/" + strings.Repeat("a", 1024), false},
		{0, "/a", false},
	} {
		if got := ValidWorkspacePath(tc.guest, tc.path); got != tc.ok {
			t.Errorf("%v %q = %v", tc.guest, tc.path, got)
		}
	}
}
