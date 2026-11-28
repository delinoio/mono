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

// TestInstallAndStartCommandsWindowsACL uses Windows PowerShell with simulated
// ACL operations and, when elevated, with a disposable NTFS runner directory.
func TestInstallAndStartCommandsWindowsACL(t *testing.T) {
	t.Parallel()
	const policy = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	const extraGrant = policy + "(A;OICI;FA;;;WD)"
	for _, tc := range []struct {
		name    string
		initial string
		failAt  string
		wantOps string
		wantErr string
	}{
		{name: "cached", initial: policy, wantOps: "read"},
		{name: "cached-reordered", initial: "O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)", wantOps: "read"},
		{name: "retry-extra-grant", initial: extraGrant, wantOps: "read,write,read"},
		{name: "retry-wrong-owner", initial: "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "retry-unprotected", initial: "O:BAD:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "retry-wrong-rights", initial: "O:BAD:P(A;OICI;FR;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "retry-inherit-only", initial: "O:BAD:P(A;OICIIO;FA;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "retry-wrong-inheritance", initial: "O:BAD:P(A;OI;FA;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "retry-duplicate-principal", initial: "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;SY)", wantOps: "read,write,read"},
		{name: "retry-deny-rule", initial: "O:BAD:P(D;OICI;FA;;;SY)(A;OICI;FA;;;BA)", wantOps: "read,write,read"},
		{name: "read-failure", initial: extraGrant, failAt: "read", wantOps: "read", wantErr: "injected initial ACL read failure"},
		{name: "repair-failure", initial: extraGrant, failAt: "write", wantOps: "read,write", wantErr: "injected ACL write failure"},
		{name: "read-back-failure", initial: extraGrant, failAt: "read-back", wantOps: "read,write,read", wantErr: "injected ACL read-back failure"},
		{name: "repair-not-applied", initial: extraGrant, failAt: "apply", wantOps: "read,write,read", wantErr: "runner directory ACL verification failed after repair"},
	} {
		t.Run("scripted/"+tc.name, func(t *testing.T) {
			script := `$ErrorActionPreference = 'Stop'
$runner = Join-Path $PSScriptRoot 'runner'
New-Item -ItemType Directory -Path "$runner\bin" -Force | Out-Null
Set-Content -LiteralPath "$runner\run.cmd" -Value 'fixture'
Set-Content -LiteralPath "$runner\bin\Runner.Listener.exe" -Value 'fixture'
$script:descriptor = $env:SANDRUN_ACL_INITIAL
$script:operations = @()
function Invoke-WebRequest { throw 'cached installation attempted a download' }
function Get-Acl($LiteralPath) {
  $script:operations += 'read'
  if ($env:SANDRUN_ACL_FAIL_AT -eq 'read') { throw 'injected initial ACL read failure' }
  if ($env:SANDRUN_ACL_FAIL_AT -eq 'read-back' -and $script:operations.Count -gt 1) {
    throw 'injected ACL read-back failure'
  }
  $result = [System.Security.AccessControl.DirectorySecurity]::new()
  $result.SetSecurityDescriptorSddlForm($script:descriptor)
  return $result
}
function Set-Acl($LiteralPath, $AclObject) {
  $script:operations += 'write'
  if ($env:SANDRUN_ACL_FAIL_AT -eq 'write') { throw 'injected ACL write failure' }
  if ($env:SANDRUN_ACL_FAIL_AT -ne 'apply') {
    $script:descriptor = $AclObject.GetSecurityDescriptorSddlForm('Owner, Access')
  }
}
try {
` + fixtureWindowsInstallScript() + `
  Write-Output 'Windows ACL assertions passed'
} finally {
  Write-Output ('ACL operations: ' + ($script:operations -join ','))
}
`
			out, err := runWindowsACLTestScript(t, script, "SANDRUN_ACL_INITIAL="+tc.initial, "SANDRUN_ACL_FAIL_AT="+tc.failAt)
			if !strings.Contains(out, "ACL operations: "+tc.wantOps+"\n") {
				t.Fatalf("expected ACL operations %q: %v\n%s", tc.wantOps, err, out)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(out, tc.wantErr) || strings.Contains(out, "Windows ACL assertions passed") {
					t.Fatalf("expected ACL error %q: %v\n%s", tc.wantErr, err, out)
				}
				return
			}
			if err != nil || !strings.Contains(out, "Windows ACL assertions passed") {
				t.Fatalf("Windows runner ACL test did not reach its final assertion: %v\n%s", err, out)
			}
		})
	}

	for _, scenario := range []string{"fresh", "retry", "cached"} {
		t.Run("ntfs/"+scenario, func(t *testing.T) {
			script := `$ErrorActionPreference = 'Stop'
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { exit 77 }
$env:TEMP = $PSScriptRoot
$env:PROCESSOR_ARCHITEW6432 = 'AMD64'
$runner = Join-Path $PSScriptRoot 'runner'
$source = Join-Path $PSScriptRoot 'source'
$fixture = Join-Path $PSScriptRoot 'fixture.zip'
if ($env:SANDRUN_ACL_SCENARIO -ne 'fresh') { $source = $runner }
New-Item -ItemType Directory -Path "$source\bin" -Force | Out-Null
Set-Content -LiteralPath "$source\run.cmd" -Value 'fixture script'
Set-Content -LiteralPath "$source\bin\Runner.Listener.exe" -Value 'fixture listener'
$startHash = (Get-FileHash -LiteralPath "$source\run.cmd").Hash
$listenerHash = (Get-FileHash -LiteralPath "$source\bin\Runner.Listener.exe").Hash
if ($env:SANDRUN_ACL_SCENARIO -eq 'retry') {
  & icacls.exe $runner /grant '*S-1-1-0:(OI)(CI)F' | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'could not seed the Everyone grant' }
} elseif ($env:SANDRUN_ACL_SCENARIO -eq 'cached') {
  $acl = Get-Acl -LiteralPath $runner
  $acl.SetSecurityDescriptorSddlForm('O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)', 'Owner, Access')
  Set-Acl -LiteralPath $runner -AclObject $acl
  function Set-Acl { throw 'compliant cache attempted an ACL write' }
} else {
  Compress-Archive -LiteralPath "$source\run.cmd", "$source\bin" -DestinationPath $fixture
  $fixtureHash = (Get-FileHash -LiteralPath $fixture).Hash.ToLowerInvariant()
}
function Invoke-WebRequest {
  param([switch] $UseBasicParsing, [string] $Uri, [string] $OutFile)
  if ($env:SANDRUN_ACL_SCENARIO -ne 'fresh') { throw 'reuse attempted a download' }
  Copy-Item -LiteralPath $fixture -Destination $OutFile
}
` + fixtureWindowsInstallScript() + `
if ((Get-FileHash -LiteralPath "$runner\run.cmd").Hash -ne $startHash -or
    (Get-FileHash -LiteralPath "$runner\bin\Runner.Listener.exe").Hash -ne $listenerHash) {
  throw 'runner files changed'
}
$file = Join-Path $runner '.sandrun-jitconfig'
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
			out, err := runWindowsACLTestScript(t, script, "SANDRUN_ACL_SCENARIO="+scenario)
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 77 {
				t.Skip("Windows ACL test requires an elevated PowerShell process")
			}
			if err != nil || !strings.Contains(out, "Windows ACL assertions passed") {
				t.Fatalf("Windows runner ACL test did not reach its final assertion: %v\n%s", err, out)
			}
		})
	}
}

func fixtureWindowsInstallScript() string {
	install := strings.Replace(windowsInstallScript(), "'"+windowsRunnerDir+"'", `(Join-Path $PSScriptRoot 'runner')`, 1)
	return strings.Replace(install, "'"+runnerSHA256["win-x64"]+"'", "$fixtureHash", 1)
}

func runWindowsACLTestScript(t *testing.T, script string, env ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-runner-acl.ps1")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return strings.ReplaceAll(string(out), "\r\n", "\n"), err
}
