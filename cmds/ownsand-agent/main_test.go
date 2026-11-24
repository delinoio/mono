// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

func TestCLIEnrollStatusReset(t *testing.T) {
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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
	// An explicit --state-dir reaches the service definition and wins over a
	// conflicting environment variable; without the flag the environment
	// value stays.
	flagDir := filepath.Join(t.TempDir(), "from-flag")
	envDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv(config.EnvStateDir, envDir)
	out.Reset()
	if code := run([]string{"service", "print", "--state-dir", flagDir}, nil, &out, &errb); code != exitOK ||
		!strings.Contains(out.String(), flagDir) || strings.Contains(out.String(), envDir) {
		t.Fatalf("service print --state-dir: %q", out.String())
	}
	out.Reset()
	if code := run([]string{"service", "print"}, nil, &out, &errb); code != exitOK ||
		!strings.Contains(out.String(), envDir) || strings.Contains(out.String(), flagDir) {
		t.Fatalf("service print without --state-dir: %q", out.String())
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
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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

// statusFixture enrolls a device in a scratch state directory and returns the
// store, so a test can place a status.json of its choice.
func statusFixture(t *testing.T) *store.Store {
	t.Helper()
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
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
	return st
}

// An absent status.json is the first-run case: the unknown line and exit 0.
func TestStatusAbsentStatusFile(t *testing.T) {
	statusFixture(t)
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitOK || !strings.Contains(out.String(), "Connection: unknown (the agent has not run)") || errb.Len() != 0 {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}

// A valid status.json keeps the existing report and exit 0.
func TestStatusValidStatusFile(t *testing.T) {
	st := statusFixture(t)
	if err := st.SaveStatus(&agent.Status{Connected: true, AgentVersion: "1.2.3", Runtime: "process"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitOK || !strings.Contains(out.String(), "Connection: connected") || !strings.Contains(out.String(), "Agent: 1.2.3, runtime process") || strings.Contains(out.String(), "has not run") || errb.Len() != 0 {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}

func TestStatusControlPlaneURL(t *testing.T) {
	const enrollmentURL = "https://enrollment.example.test"
	const runtimeURL = "https://effective.example.test"
	const runtimeSnapshot = `{"api_url":"https://effective.example.test","connected":true,"updated_at":"2026-01-01T00:00:00Z"}`
	const connected = "connected (status of 2026-01-01 00:00:00)"
	tests := []struct {
		name          string
		snapshot      string
		enrollmentURL string
		wantURL       string
		wantConn      string
	}{
		{"runtime URL", runtimeSnapshot, enrollmentURL, runtimeURL, connected},
		{"runtime URL without enrollment URL", runtimeSnapshot, "", runtimeURL, connected},
		{"legacy snapshot", `{"connected":true,"updated_at":"2026-01-01T00:00:00Z"}`, enrollmentURL, enrollmentURL, connected},
		{"absent snapshot", "", enrollmentURL, enrollmentURL, "unknown (the agent has not run)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := statusFixture(t)
			t.Setenv("OWNSAND_AGENT_API_URL", "https://shell.example.test")
			if tt.enrollmentURL != "" {
				if _, err := st.UpdateAgent(func(a *store.AgentState) error {
					a.APIURL = tt.enrollmentURL
					a.Removal.NoticeReceived = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.snapshot != "" {
				if err := os.WriteFile(st.Path(store.StatusFile), []byte(tt.snapshot), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out, errb bytes.Buffer
			if code := run([]string{"status", "--no-color"}, nil, &out, &errb); code != exitOK || errb.Len() != 0 {
				t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
			}
			if got := out.String(); strings.Count(got, "Control plane:") != 1 || !strings.Contains(got, "Control plane: "+tt.wantURL+"\n") || !strings.Contains(got, "Connection: "+tt.wantConn+"\n") {
				t.Fatalf("stdout = %q, want control plane %q and connection %q", got, tt.wantURL, tt.wantConn)
			}
			if tt.enrollmentURL != "" && !strings.Contains(out.String(), "Removal: in progress\n") {
				t.Fatalf("stdout = %q, want removal in progress", out.String())
			}
		})
	}
}

// A status.json that cannot be decoded or read is an error: a diagnostic on
// standard error, exit 1 and never the first-run line. The other sections
// still print.
func TestStatusUnreadableStatusFile(t *testing.T) {
	tests := map[string]func(t *testing.T, path string){
		"malformed": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"truncated": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"connected": tr`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			st := statusFixture(t)
			setup(t, st.Path(store.StatusFile))
			var out, errb bytes.Buffer
			code := run([]string{"status", "--no-color"}, nil, &out, &errb)
			if code != exitError {
				t.Fatalf("status exit code = %d, want %d (%q / %q)", code, exitError, out.String(), errb.String())
			}
			if !strings.Contains(errb.String(), "ownsand-agent: read status.json:") {
				t.Fatalf("stderr = %q, want a read status.json diagnostic", errb.String())
			}
			if strings.Contains(out.String(), "has not run") || strings.Contains(out.String(), "Connection:") {
				t.Fatalf("stdout = %q, must not report a connection state", out.String())
			}
			if !strings.Contains(out.String(), "Sandboxes: 0 records") || !strings.Contains(out.String(), "Snapshots: 0") {
				t.Fatalf("stdout = %q, want the other sections", out.String())
			}
		})
	}
}

// makeUnlistable removes every permission from the inventory directory at
// path so that listing it fails. store.Open runs before the inventory reads
// and fails on a path that is not a directory, so a permission change is the
// way to make only the read fail. The test is skipped where the change has no
// effect: on Windows (mode bits do not restrict directory reads) and for a
// user that bypasses permission checks, such as root.
func makeUnlistable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Skipf("cannot remove directory permissions on this system: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
	if _, err := os.ReadDir(path); err == nil {
		t.Skip("directory permissions do not restrict reads on this system or for this user")
	}
}

// Empty inventories are a successful read: both counts show 0 and exit 0.
func TestStatusEmptyInventories(t *testing.T) {
	statusFixture(t)
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitOK || !strings.Contains(out.String(), "Sandboxes: 0 records") || !strings.Contains(out.String(), "Snapshots: 0\n") || errb.Len() != 0 {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}

// Readable inventories keep their counts and exit 0, and only directory
// entries count as snapshots.
func TestStatusInventoryCounts(t *testing.T) {
	st := statusFixture(t)
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(st.Path(store.SandboxesDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(st.Path(store.SnapshotsDir, "snap-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(store.SnapshotsDir, "stray-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitOK || !strings.Contains(out.String(), "Sandboxes: 2 records") || !strings.Contains(out.String(), "Snapshots: 1\n") || errb.Len() != 0 {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}

// A sandbox record that cannot be read makes the count unavailable: the
// partial count of the records read before it is not shown, the diagnostic
// names the sandbox inventory, the snapshot count still prints and the exit
// code is 1.
func TestStatusUnreadableSandboxRecord(t *testing.T) {
	st := statusFixture(t)
	if err := os.WriteFile(st.Path(store.SandboxesDir, "a.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink lists as a file but fails to read. Creating a
	// symlink needs a privilege on Windows, so the case is skipped there
	// when the system refuses.
	if err := os.Symlink(filepath.Join(st.Path(store.SandboxesDir), "missing-target"), st.Path(store.SandboxesDir, "b.json")); err != nil {
		t.Skipf("cannot create a symlink on this system: %v", err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitError {
		t.Fatalf("status exit code = %d, want %d (%q / %q)", code, exitError, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Sandboxes: unavailable (") || strings.Contains(out.String(), "records") || !strings.Contains(out.String(), "Snapshots: 0\n") {
		t.Fatalf("stdout = %q, want an unavailable sandbox count and the snapshot count", out.String())
	}
	if !strings.Contains(errb.String(), "ownsand-agent: read sandbox records:") {
		t.Fatalf("stderr = %q, want a read sandbox records diagnostic", errb.String())
	}
}

// A sandboxes directory that cannot be listed is the same failure.
func TestStatusUnreadableSandboxDirectory(t *testing.T) {
	st := statusFixture(t)
	makeUnlistable(t, st.Path(store.SandboxesDir))
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitError || !strings.Contains(out.String(), "Sandboxes: unavailable (") || !strings.Contains(out.String(), "Snapshots: 0\n") || !strings.Contains(errb.String(), "ownsand-agent: read sandbox records:") {
		t.Fatalf("status: %d %q / %q", code, out.String(), errb.String())
	}
}

// A snapshots directory that cannot be listed makes the snapshot count
// unavailable (never 0): the diagnostic names the snapshot inventory, the
// sandbox count still prints and the exit code is 1.
func TestStatusUnreadableSnapshotDirectory(t *testing.T) {
	st := statusFixture(t)
	if err := os.WriteFile(st.Path(store.SandboxesDir, "a.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeUnlistable(t, st.Path(store.SnapshotsDir))
	var out, errb bytes.Buffer
	code := run([]string{"status", "--no-color"}, nil, &out, &errb)
	if code != exitError {
		t.Fatalf("status exit code = %d, want %d (%q / %q)", code, exitError, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Sandboxes: 1 records") || !strings.Contains(out.String(), "Snapshots: unavailable") || strings.Contains(out.String(), "Snapshots: 0") {
		t.Fatalf("stdout = %q, want the sandbox count and an unavailable snapshot count", out.String())
	}
	if !strings.Contains(errb.String(), "ownsand-agent: read snapshots:") {
		t.Fatalf("stderr = %q, want a read snapshots diagnostic", errb.String())
	}
}

// TestImageBuildResourceRanges runs the real flag parser: a value that does
// not fit the conversion to bytes or to a 32-bit vCPU count is a usage error
// that names its flag, for both guest OS values, and no state is created.
func TestImageBuildResourceRanges(t *testing.T) {
	// Inherited OWNSAND_AGENT_* variables of the caller must not reach run.
	clearAgentEnv(t)
	big := func(v uint64) string { return strconv.FormatUint(v, 10) }
	tests := []struct {
		flag, value string
	}{
		{"--disk-gib", big(1 << 33)},
		{"--disk-gib", big(1 << 34)},
		{"--disk-gib", big(1<<34 + 64)},
		{"--disk-gib", big(math.MaxInt64)},
		{"--cpus", big(math.MaxInt32 + 1)},
		{"--cpus", big(1 << 32)},
		{"--cpus", big(1<<32 + 4)},
		{"--memory-mib", big(1 << 43)},
		{"--memory-mib", big(1 << 44)},
		{"--memory-mib", big(math.MaxInt64)},
	}
	if strconv.IntSize < 64 {
		// An int flag cannot hold the 64-bit values, and the flag parser
		// rejects them before the range check.
		tests = tests[:4]
		tests = append(tests, struct{ flag, value string }{"--memory-mib", big(1 << 44)})
	}
	for _, guest := range []string{"macos", "windows"} {
		for _, tc := range tests {
			stateDir := filepath.Join(t.TempDir(), "state")
			var out, errb bytes.Buffer
			args := []string{"image", "build", "--os", guest, "--state-dir", stateDir, tc.flag, tc.value}
			if code := run(args, nil, &out, &errb); code != exitUsage {
				t.Fatalf("%v: exit %d, stderr %q", args, code, errb.String())
			}
			if !strings.Contains(errb.String(), tc.flag+" "+tc.value) {
				t.Fatalf("%v: stderr %q does not name the flag and value", args, errb.String())
			}
			if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
				t.Fatalf("%v: the state directory exists (%v)", args, err)
			}
		}
	}
}

func TestValidateImageResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    imageBuildFlags
		want int32
	}{
		{"defaults", imageBuildFlags{}, 0},
		{"four", imageBuildFlags{cpus: 4, diskGiB: 8589934591, memoryMiB: 1<<43 - 1}, 4},
		{"largest", imageBuildFlags{cpus: math.MaxInt32}, math.MaxInt32},
	} {
		if got, err := validateImageResources(tc.f); err != nil || got != tc.want {
			t.Fatalf("%s: got %d, %v", tc.name, got, err)
		}
	}
	if _, err := validateImageResources(imageBuildFlags{cpus: -1}); err == nil {
		t.Fatal("negative cpus accepted")
	}
	if strconv.IntSize == 64 {
		for _, cpus := range []int64{1 << 31, 1 << 32, 1<<32 + 4} {
			_, err := validateImageResources(imageBuildFlags{cpus: int(cpus)})
			if err == nil || !strings.Contains(err.Error(), "--cpus") {
				t.Fatalf("cpus %d: %v", cpus, err)
			}
		}
	}
}
