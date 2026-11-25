// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallAndStartCommandsWindowsACL uses Windows PowerShell and a
// disposable runner directory. The fresh path downloads a local fixture ZIP.
func TestInstallAndStartCommandsWindowsACL(t *testing.T) {
	for _, scenario := range []string{"fresh", "reused"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			install := strings.Replace(windowsInstallScript(), "'"+windowsRunnerDir+"'", `(Join-Path $PSScriptRoot 'runner')`, 1)
			install = strings.Replace(install, "'"+runnerSHA256["win-x64"]+"'", "$fixtureHash", 1)
			script := `$ErrorActionPreference = 'Stop'
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { exit 77 }
$env:TEMP = $PSScriptRoot
$env:PROCESSOR_ARCHITEW6432 = 'AMD64'
$runner = Join-Path $PSScriptRoot 'runner'
$source = Join-Path $PSScriptRoot 'source'
$fixture = Join-Path $PSScriptRoot 'fixture.zip'
if ($env:TIDE_ACL_SCENARIO -eq 'reused') { $source = $runner }
New-Item -ItemType Directory -Path "$source\bin" -Force | Out-Null
Set-Content -LiteralPath "$source\run.cmd" -Value 'fixture script'
Set-Content -LiteralPath "$source\bin\Runner.Listener.exe" -Value 'fixture listener'
$startHash = (Get-FileHash -LiteralPath "$source\run.cmd").Hash
$listenerHash = (Get-FileHash -LiteralPath "$source\bin\Runner.Listener.exe").Hash
if ($env:TIDE_ACL_SCENARIO -eq 'reused') {
  & icacls.exe $runner /grant '*S-1-1-0:(OI)(CI)F' | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'could not seed the Everyone grant' }
} else {
  Compress-Archive -LiteralPath "$source\run.cmd", "$source\bin" -DestinationPath $fixture
  $fixtureHash = (Get-FileHash -LiteralPath $fixture).Hash.ToLowerInvariant()
}
function Invoke-WebRequest {
  param([switch] $UseBasicParsing, [string] $Uri, [string] $OutFile)
  if ($env:TIDE_ACL_SCENARIO -eq 'reused') { throw 'reuse attempted a download' }
  Copy-Item -LiteralPath $fixture -Destination $OutFile
}
` + install + `
if ((Get-FileHash -LiteralPath "$runner\run.cmd").Hash -ne $startHash -or
    (Get-FileHash -LiteralPath "$runner\bin\Runner.Listener.exe").Hash -ne $listenerHash) {
  throw 'runner files changed'
}
$file = Join-Path $runner '.tide-jitconfig'
Set-Content -LiteralPath $file -Value 'non-secret fixture'
$directoryACL = Get-Acl -LiteralPath $runner
$sidType = [Security.Principal.SecurityIdentifier]
if (-not $directoryACL.AreAccessRulesProtected -or $directoryACL.GetOwner($sidType).Value -ne 'S-1-5-32-544') {
  throw 'runner directory ACL inherits from its parent or has an unexpected owner'
}
foreach ($acl in @($directoryACL, (Get-Acl -LiteralPath $file))) {
  $rules = $acl.GetAccessRules($true, $true, $sidType)
  $sids = @($rules | ForEach-Object { $_.IdentityReference.Value } | Sort-Object)
  if (($sids -join ',') -ne 'S-1-5-18,S-1-5-32-544') { throw "unexpected ACL principals: $sids" }
  foreach ($rule in $rules) {
    if ($rule.AccessControlType -ne 'Allow' -or $rule.FileSystemRights -ne 'FullControl') {
      throw "unexpected ACL rule: $rule"
    }
  }
}
Write-Output 'Windows ACL assertions passed'
`
			path := filepath.Join(dir, "test-runner-acl.ps1")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
			cmd.Env = append(os.Environ(), "TIDE_ACL_SCENARIO="+scenario)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 77 {
				t.Skip("Windows ACL test requires an elevated PowerShell process")
			}
			if err != nil || !strings.Contains(string(out), "Windows ACL assertions passed") {
				t.Fatalf("Windows runner ACL test did not reach its final assertion: %v\n%s", err, out)
			}
		})
	}
}
