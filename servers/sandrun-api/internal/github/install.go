// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

// RunnerVersion is the pinned version of the GitHub Actions runner
// (github.com/actions/runner) that SandRun installs when a guest has none.
// Renovate updates it through the custom manager in renovate.json; the
// checksums below must be updated in the same change (TestRunnerChecksums
// fails until they are).
//
// renovate: datasource=github-releases depName=actions/runner
const RunnerVersion = "2.338.2"

// runnerChecksumsVersion is the runner version of runnerSHA256.
const runnerChecksumsVersion = "2.338.2"

// runnerSHA256 holds the SHA-256 of the release archives of
// runnerChecksumsVersion, from the release notes of
// https://github.com/actions/runner/releases/tag/v2.338.2, by
// <os>-<arch>.
var runnerSHA256 = map[string]string{
	"linux-x64":   "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40",
	"linux-arm64": "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945",
	"osx-x64":     "109fff505a34014a7e8d1acb850c33d404811102f572b8fb9788e2fff013cc49",
	"osx-arm64":   "25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388",
	"win-x64":     "a1f17aa987fc8158bc7521d284912267a0d01b7c2c4684b33c86783b5e8221cc",
	"win-arm64":   "0790e3f586c2c5388ed8e293e7cef01bd0e8a2c5c68dd0a628f848b61da4b3a7",
}

// Guest paths of the runner. The release notes recommend a short root
// folder on Windows because of path length limits. An admin who installs the
// runner in an OwnSand template uses the same directory, so SandRun finds it and
// skips the installation.
const (
	unixRunnerDir    = "/opt/actions-runner"
	windowsRunnerDir = `C:\actions-runner`
	// jitFileName holds the JIT configuration until the start command reads
	// and deletes it.
	jitFileName = ".sandrun-jitconfig"
	// logFileName receives the runner output; the exec stream carries only
	// the exit result.
	logFileName = "sandrun-runner.log"
)

// RunnerDir returns the runner directory of a guest OS.
func RunnerDir(guest sandrunv1.GuestOs) string {
	if guest == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
		return windowsRunnerDir
	}
	return unixRunnerDir
}

func guestJoin(guest sandrunv1.GuestOs, name string) string {
	if guest == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
		return windowsRunnerDir + `\` + name
	}
	return unixRunnerDir + "/" + name
}

// JITFilePath is the guest file of the JIT configuration.
func JITFilePath(guest sandrunv1.GuestOs) string { return guestJoin(guest, jitFileName) }

// LogFilePath is the guest log file of the runner program.
func LogFilePath(guest sandrunv1.GuestOs) string { return guestJoin(guest, logFileName) }

// unixInstallScript checks for the runner in /opt/actions-runner and
// installs it from the official release archive when it is missing: it
// selects the archive of the machine architecture, downloads it with curl
// (or wget), verifies its SHA-256, extracts it next to the target, runs the
// runner's bin/installdependencies.sh on Linux (the .NET dependencies
// through the distribution's package manager) and moves the result into
// place, so an interrupted installation never leaves a partial runner.
func unixInstallScript(osName string) string {
	deps := ""
	if osName == "linux" {
		deps = `(cd "$dir.partial" && ./bin/installdependencies.sh)` + "\n"
	}
	return `set -eu
dir=` + unixRunnerDir + `
if [ -x "$dir/run.sh" ] && [ -e "$dir/bin/Runner.Listener" ]; then exit 0; fi
case "$(uname -m)" in
  x86_64|amd64) arch=x64; sum=` + runnerSHA256[osName+"-x64"] + ` ;;
  aarch64|arm64) arch=arm64; sum=` + runnerSHA256[osName+"-arm64"] + ` ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 3 ;;
esac
asset="actions-runner-` + osName + `-$arch-` + RunnerVersion + `.tar.gz"
url="https://github.com/actions/runner/releases/download/v` + RunnerVersion + `/$asset"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$tmp/$asset" "$url"; else wget -q -O "$tmp/$asset" "$url"; fi
actual="$( (sha256sum "$tmp/$asset" 2>/dev/null || shasum -a 256 "$tmp/$asset") | cut -d ' ' -f 1)"
if [ "$actual" != "$sum" ]; then echo "checksum mismatch for $asset" >&2; exit 4; fi
rm -rf "$dir.partial"
mkdir -p "$dir.partial"
tar xzf "$tmp/$asset" -C "$dir.partial"
` + deps + `rm -rf "$dir"
mv "$dir.partial" "$dir"
`
}

// windowsInstallScript installs the runner if it is missing and restricts
// access to the runner directory before SandRun can write the JIT configuration.
func windowsInstallScript() string {
	return `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$dir = '` + windowsRunnerDir + `'
$installed = (Test-Path -LiteralPath "$dir\run.cmd") -and (Test-Path -LiteralPath "$dir\bin\Runner.Listener.exe")
if (-not $installed) {
  $arch = $env:PROCESSOR_ARCHITEW6432
  if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
  switch ($arch) {
    'AMD64' { $a = 'x64'; $sum = '` + runnerSHA256["win-x64"] + `' }
    'ARM64' { $a = 'arm64'; $sum = '` + runnerSHA256["win-arm64"] + `' }
    default { throw "unsupported architecture: $arch" }
  }
  $asset = "actions-runner-win-$a-` + RunnerVersion + `.zip"
  $zip = Join-Path $env:TEMP $asset
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest -UseBasicParsing -Uri "https://github.com/actions/runner/releases/download/v` + RunnerVersion + `/$asset" -OutFile $zip
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant() -ne $sum) { Remove-Item -LiteralPath $zip -Force; throw "checksum mismatch for $asset" }
  $partial = "$dir.partial"
  if (Test-Path -LiteralPath $partial) { Remove-Item -LiteralPath $partial -Recurse -Force }
  New-Item -ItemType Directory -Force -Path $partial | Out-Null
  Add-Type -AssemblyName System.IO.Compression.FileSystem
  [System.IO.Compression.ZipFile]::ExtractToDirectory($zip, $partial)
  Remove-Item -LiteralPath $zip -Force
  if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
  Move-Item -LiteralPath $partial -Destination $dir
}
# Replace the entire DACL, including explicit grants from a template.
# Set a trusted owner so the previous owner cannot restore those grants.
$acl = Get-Acl -LiteralPath $dir
$acl.SetSecurityDescriptorSddlForm('O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)', [System.Security.AccessControl.AccessControlSections]'Owner, Access')
Set-Acl -LiteralPath $dir -AclObject $acl
`
}

// InstallCommand returns the exec command that checks for the runner and
// installs it when it is missing.
func InstallCommand(guest sandrunv1.GuestOs) []string {
	switch guest {
	case sandrunv1.GuestOs_GUEST_OS_WINDOWS:
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", windowsInstallScript()}
	case sandrunv1.GuestOs_GUEST_OS_MACOS:
		return []string{"/bin/sh", "-c", unixInstallScript("osx")}
	}
	return []string{"/bin/sh", "-c", unixInstallScript("linux")}
}

// StartCommand returns the exec command that starts the runner with the JIT
// configuration from its guest file: it reads the file, deletes it, and
// starts run.sh --jitconfig (run.cmd --jitconfig on Windows) with the
// program output appended to the guest log file, so the exec stream carries
// only the exit result. RUNNER_ALLOW_RUNASROOT=1 is set because the guest
// helper may run the exec as root, which run-helper.sh refuses otherwise
// (source only: src/Misc/layoutroot/run-helper.sh.template of the runner).
// The JIT configuration never appears in the command; on Windows, run.cmd
// --jitconfig is shown only by the runner source (run.cmd passes its
// arguments to run-helper.cmd), not by the GitHub docs.
func StartCommand(guest sandrunv1.GuestOs) []string {
	if guest == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
		// cmd.exe runs run.cmd with the redirection, because PowerShell 5.1
		// turns redirected native stderr into errors.
		script := `$ErrorActionPreference = 'Stop'
$f = '` + JITFilePath(guest) + `'
$cfg = (Get-Content -Raw -LiteralPath $f).Trim()
Remove-Item -LiteralPath $f -Force
Set-Location -LiteralPath '` + windowsRunnerDir + `'
$ErrorActionPreference = 'Continue'
& cmd.exe /d /c "run.cmd --jitconfig $cfg >> ` + logFileName + ` 2>&1"
exit $LASTEXITCODE
`
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}
	}
	script := `set -eu
cfg="$(cat "$1")"
rm -f "$1"
cd "$2"
RUNNER_ALLOW_RUNASROOT=1 exec ./run.sh --jitconfig "$cfg" >>"$3" 2>&1`
	return []string{"/bin/sh", "-c", script, "sandrun-runner", JITFilePath(guest), RunnerDir(guest), LogFilePath(guest)}
}
