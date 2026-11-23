// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"strings"
	"testing"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// TestRunnerChecksums fails when Renovate (or a person) changes the pinned
// runner version without the checksums of the new release archives.
func TestRunnerChecksums(t *testing.T) {
	if RunnerVersion != runnerChecksumsVersion {
		t.Fatalf("RunnerVersion %s has no checksums (runnerSHA256 is for %s): copy the sha256 values of the six archives from the release notes of v%s",
			RunnerVersion, runnerChecksumsVersion, RunnerVersion)
	}
	for _, platform := range []string{"linux-x64", "linux-arm64", "osx-x64", "osx-arm64", "win-x64", "win-arm64"} {
		if sum := runnerSHA256[platform]; len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
			t.Errorf("%s: invalid sha256 %q", platform, sum)
		}
	}
}

func TestInstallAndStartCommands(t *testing.T) {
	for _, tc := range []struct {
		guest       tidev1.GuestOs
		program     string
		asset, sums []string
		start       string
	}{
		{tidev1.GuestOs_GUEST_OS_LINUX, "/bin/sh", []string{"actions-runner-linux-$arch-2.338.2.tar.gz", "installdependencies.sh", "/opt/actions-runner"},
			[]string{runnerSHA256["linux-x64"], runnerSHA256["linux-arm64"]}, "./run.sh --jitconfig"},
		{tidev1.GuestOs_GUEST_OS_MACOS, "/bin/sh", []string{"actions-runner-osx-$arch-2.338.2.tar.gz", "/opt/actions-runner"},
			[]string{runnerSHA256["osx-x64"], runnerSHA256["osx-arm64"]}, "./run.sh --jitconfig"},
		{tidev1.GuestOs_GUEST_OS_WINDOWS, "powershell.exe", []string{"actions-runner-win-$a-2.338.2.zip", `C:\actions-runner`, "Get-FileHash", "icacls.exe"},
			[]string{runnerSHA256["win-x64"], runnerSHA256["win-arm64"]}, "run.cmd --jitconfig"},
	} {
		install := InstallCommand(tc.guest)
		joined := strings.Join(install, " ")
		if install[0] != tc.program || !strings.Contains(joined, "https://github.com/actions/runner/releases/download/v2.338.2/") {
			t.Errorf("%v install %v", tc.guest, install)
		}
		for _, s := range append(tc.asset, tc.sums...) {
			if !strings.Contains(joined, s) {
				t.Errorf("%v install lacks %q", tc.guest, s)
			}
		}
		if tc.guest == tidev1.GuestOs_GUEST_OS_MACOS && strings.Contains(joined, "installdependencies") {
			t.Error("macOS runs the Linux dependency installer")
		}
		start := strings.Join(StartCommand(tc.guest), " ")
		if !strings.Contains(start, tc.start) || !strings.Contains(start, JITFilePath(tc.guest)) || !strings.Contains(start, logFileName) {
			t.Errorf("%v start %q", tc.guest, start)
		}
	}
	if JITFilePath(tidev1.GuestOs_GUEST_OS_WINDOWS) != `C:\actions-runner\.tide-jitconfig` || LogFilePath(tidev1.GuestOs_GUEST_OS_LINUX) != "/opt/actions-runner/tide-runner.log" {
		t.Fatal("paths")
	}
}
