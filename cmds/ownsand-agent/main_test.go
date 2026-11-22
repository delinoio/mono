// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

func TestCLIEnrollStatusReset(t *testing.T) {
	cp := testcp.New(t)
	dir := t.TempDir()
	t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
	// The process runtime makes reset skip the host network cleanup, so that
	// the test never deletes tap devices or the nftables table of the machine
	// that runs it, whatever runtime the parent environment selects.
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := run([]string{"status"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("status before enroll: %d", code)
	}
	out.Reset()
	if code := run([]string{"enroll", "--api-url", cp.URL}, strings.NewReader(testcp.Token+"\n"), &out, &errb); code != exitOK {
		t.Fatalf("enroll: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Enrolled device") || strings.Contains(out.String()+errb.String(), testcp.Token) {
		t.Fatalf("enroll output %q / %q", out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"status", "--no-color"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "Enrollment: enrolled") || !strings.Contains(out.String(), cp.URL) {
		t.Fatalf("status: %d %q", code, out.String())
	}
	if code := run([]string{"reset"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("reset without --yes: %d", code)
	}
	out.Reset()
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK {
		t.Fatalf("reset: %d", code)
	}
	if code := run([]string{"status"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("status after reset: %d", code)
	}
	if code := run([]string{"enroll", "--api-url", cp.URL}, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Fatalf("enroll without token: %d", code)
	}
}

func TestCLIMisc(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errb); code != exitOK || out.Len() == 0 {
		t.Fatal("version")
	}
	out.Reset()
	if code := run([]string{"service", "print"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "run") {
		t.Fatalf("service print: %q", out.String())
	}
	if code := run(nil, nil, &out, &errb); code != exitUsage {
		t.Fatal("no command")
	}
	// Linux base images are not built by the agent, and an unknown guest
	// OS is a usage error; neither touches the state directory.
	for _, os := range []string{"linux", "plan9"} {
		if code := run([]string{"image", "build", "--os", os}, nil, &out, &errb); code != exitUsage {
			t.Fatalf("image build --os %s: %d", os, code)
		}
	}
	t.Setenv("OWNSAND_AGENT_STATE_DIR", t.TempDir())
	if code := run([]string{"run"}, nil, &out, &errb); code != exitNotEnrolled {
		t.Fatalf("run without enrollment: %d", code)
	}
	// Under launchd (hidden marker flag) a missing enrollment exits 0, so
	// that KeepAlive SuccessfulExit=false does not restart the agent; the
	// public exit code of plain `run` stays 3 (checked above).
	if code := run([]string{"run", "--service-manager", "launchd"}, nil, &out, &errb); code != exitOK {
		t.Fatalf("launchd run without enrollment: %d", code)
	}
	if code := run([]string{"run", "--service-manager", "bogus"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("unknown service manager: %d", code)
	}
}

// A configuration that does not parse ends `run` with the usage exit code
// and the error on standard error; the test process is no Windows service,
// so the Windows build does not connect to the service control manager.
func TestRunWithInvalidConfiguration(t *testing.T) {
	t.Setenv("OWNSAND_AGENT_STATE_DIR", t.TempDir())
	t.Setenv("OWNSAND_AGENT_LOG_LEVEL", "bogus")
	var out, errb bytes.Buffer
	if code := run([]string{"run"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("run = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errb.String(), "invalid OWNSAND_AGENT_LOG_LEVEL") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// TestStatusPrintsCapabilities checks the capability lines of `status`:
// every hypervisor check of the last capability report with its result and
// detail, and an unknown state before the first report.
func TestStatusPrintsCapabilities(t *testing.T) {
	var out bytes.Buffer
	printCapabilities(&out, agent.Status{}, colors{})
	if got := out.String(); got != "Capabilities: unknown (no capability report yet)\n" {
		t.Fatalf("before a report: %q", got)
	}
	out.Reset()
	at := time.Date(2026, 11, 22, 14, 25, 28, 0, time.Local)
	printCapabilities(&out, agent.Status{
		GuestOSes:  []string{"GUEST_OS_LINUX", "GUEST_OS_MACOS"},
		BaseImages: []string{"BASE_IMAGE_LINUX"},
		Checks: []agent.StatusCheck{
			{ID: "virtualization-entitlement", Passed: true, Detail: "the agent executable has the entitlement"},
			{ID: "macos-base-image", Detail: "the macOS base image is not built"},
			{ID: "outbound-network", Passed: true},
		},
		CapabilitiesAt: at,
	}, colors{})
	want := "Capabilities: guest OSes GUEST_OS_LINUX, GUEST_OS_MACOS; base images BASE_IMAGE_LINUX (report of 2026-11-22 14:25:28)\n" +
		"  virtualization-entitlement: passed (the agent executable has the entitlement)\n" +
		"  macos-base-image: failed (the macOS base image is not built)\n" +
		"  outbound-network: passed\n"
	if got := out.String(); got != want {
		t.Fatalf("status capabilities:\n%s\nwant:\n%s", got, want)
	}
	out.Reset()
	printCapabilities(&out, agent.Status{CapabilitiesAt: at}, colors{})
	if got := out.String(); !strings.Contains(got, "guest OSes none; base images none") {
		t.Fatalf("empty report: %q", got)
	}
}

// Only a missing identity file or an identity without a device ID means that
// the device is not enrolled (exit code 3, which service managers do not
// restart). An identity file that cannot be read or decoded is an error
// (exit code 1) with the cause in the output, so that the service manager
// restarts the agent and the operator sees why.
func TestIdentityReadFailureIsNotNotEnrolled(t *testing.T) {
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	t.Setenv("NO_COLOR", "1")
	cases := []struct {
		name      string
		setup     func(t *testing.T, path string)
		wantRun   int
		wantLD    int
		wantEnrol bool // the status output says "not enrolled"
	}{
		{
			name:      "missing",
			setup:     func(t *testing.T, path string) {},
			wantRun:   exitNotEnrolled,
			wantLD:    exitOK,
			wantEnrol: true,
		},
		{
			name: "pending identity without a device ID",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"device_id":"","organization_id":"","private_key_seed":"AAAA"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantRun:   exitNotEnrolled,
			wantLD:    exitOK,
			wantEnrol: true,
		},
		{
			name: "corrupt",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"device_id": "dev`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantRun: exitError,
			wantLD:  exitError,
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantRun: exitError,
			wantLD:  exitError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
			tc.setup(t, filepath.Join(dir, store.IdentityFile))
			var out, errb bytes.Buffer
			if code := run([]string{"status"}, nil, &out, &errb); code != tc.wantRun {
				t.Fatalf("status: %d, want %d (%q / %q)", code, tc.wantRun, out.String(), errb.String())
			}
			if got := strings.Contains(out.String(), "not enrolled"); got != tc.wantEnrol {
				t.Fatalf("status prints not enrolled = %v, want %v (%q)", got, tc.wantEnrol, out.String())
			}
			if !tc.wantEnrol && (!strings.HasPrefix(errb.String(), "ownsand-agent: ") || out.Len() != 0) {
				t.Fatalf("status error output %q / %q", out.String(), errb.String())
			}
			out.Reset()
			errb.Reset()
			if code := run([]string{"run"}, nil, &out, &errb); code != tc.wantRun {
				t.Fatalf("run: %d, want %d", code, tc.wantRun)
			}
			if code := run([]string{"run", "--service-manager", "launchd"}, nil, &out, &errb); code != tc.wantLD {
				t.Fatalf("launchd run: %d, want %d", code, tc.wantLD)
			}
		})
	}
}

// A valid identity is enrolled: status reports it and exits 0.
func TestStatusWithValidIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveIdentity(&store.Identity{DeviceID: "dev-1", OrganizationID: "org-1", PrivateKeySeed: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"status", "--no-color"}, nil, &out, &errb); code != exitOK || !strings.Contains(out.String(), "Enrollment: enrolled (device dev-1, organization org-1)") {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}
