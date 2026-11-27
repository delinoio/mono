param(
    [Parameter(Mandatory = $true)][string]$ExpectedRevision,
    [Parameter(Mandatory = $true)][string]$LinuxKernel,
    [Parameter(Mandatory = $true)][string]$LinuxRootfs,
    [Parameter(Mandatory = $true)][string]$WindowsISO,
    [Parameter(Mandatory = $true)][string]$Edition
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

if ((git rev-parse HEAD).Trim() -ne $ExpectedRevision -or (git status --porcelain --untracked-files=no)) {
    throw 'Use a clean checkout at the recorded worker revision'
}
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run the Hyper-V and image-build checks from elevated PowerShell'
}

function Get-WorkerAces([string]$Path) {
    $bytes = (Get-Acl -LiteralPath $Path).GetSecurityDescriptorBinaryForm()
    $descriptor = [Security.AccessControl.RawSecurityDescriptor]::new($bytes, 0)
    if ($null -eq $descriptor.DiscretionaryAcl) {
        throw "NULL DACL on $Path"
    }
    foreach ($ace in $descriptor.DiscretionaryAcl) {
        if ($ace.SecurityIdentifier.Value -like 'S-1-5-83-*') {
            $ace
        }
    }
}

function Assert-NoWorkerAces([string]$Path) {
    if (@(Get-WorkerAces $Path).Count -ne 0) {
        throw "Hyper-V worker ACE remains on $Path"
    }
    Write-Output "No Hyper-V worker ACEs on $Path"
}

function Invoke-LoggedNative([string]$Log, [string]$Program, [string[]]$Arguments, [int]$ExpectedExit = 0) {
    # Windows PowerShell wraps redirected native stderr in ErrorRecord values.
    $ErrorActionPreference = 'Continue'
    & $Program @Arguments 2>&1 | ForEach-Object { "$_" } | Tee-Object -FilePath $Log
    if ($LASTEXITCODE -ne $ExpectedExit) {
        throw "$Program exited with $LASTEXITCODE. Expected $ExpectedExit. See $Log."
    }
}

$LinuxKernel = (Resolve-Path -LiteralPath $LinuxKernel).Path
$LinuxRootfs = (Resolve-Path -LiteralPath $LinuxRootfs).Path
$WindowsISO = (Resolve-Path -LiteralPath $WindowsISO).Path
New-Item -ItemType Directory -Path '.t3-tmp' -Force | Out-Null
$work = (New-Item -ItemType Directory -Path (Join-Path '.t3-tmp' ('hv-' + [guid]::NewGuid().ToString('N')))).FullName
$env:TEMP = $work
$env:TMP = $work
$env:GOTMPDIR = $work

$agent = Join-Path $work 'ownsand-agent.exe'
$guest = Join-Path $work 'ownsand-guest.exe'
go build -o $agent ./cmds/ownsand-agent
if ($LASTEXITCODE -ne 0) { throw 'Agent build failed' }
go build -o $guest ./cmds/ownsand-guest
if ($LASTEXITCODE -ne 0) { throw 'Guest helper build failed' }

# Build a disposable state tree with the legacy inheritable worker group grant.
$legacy = Join-Path $work 'legacy-state'
$legacyImage = (New-Item -ItemType Directory -Path (Join-Path $legacy 'images/windows') -Force).FullName
icacls.exe $legacyImage /grant '*S-1-5-83-0:(OI)(CI)(M)' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not seed the legacy image ACL' }
$legacyDisk = Join-Path $legacyImage 'disk.vhdx'
Set-Content -LiteralPath $legacyDisk -Value 'ACL fixture only'
if (@(Get-WorkerAces $legacyDisk).Count -eq 0) { throw 'Missing legacy inherited ACE' }
$env:OWNSAND_AGENT_STATE_DIR = $legacy
$env:OWNSAND_AGENT_RUNTIME = 'hyperv'
Invoke-LoggedNative -Log (Join-Path $work 'legacy-startup.log') -Program $agent -Arguments @('run') -ExpectedExit 3
Assert-NoWorkerAces $legacyImage
Assert-NoWorkerAces $legacyDisk

Invoke-LoggedNative -Log (Join-Path $work 'store-acl.log') -Program 'go' -Arguments @(
    'test', '-count=1', '-v', '-run', '^(TestOpenCorrectsExistingStateDirACL|TestOpenKeepsPerVMEntriesAndDropsGroupWrite)$',
    './cmds/ownsand-agent/internal/store'
)

# Use a clean state directory for the ISO build and confirm the published ACLs
# before the runtime has an opportunity to sanitize the image at first use.
$env:OWNSAND_AGENT_STATE_DIR = Join-Path $work 'build-state'
Invoke-LoggedNative -Log (Join-Path $work 'image-build.log') -Program $agent -Arguments @(
    'image', 'build', '--os', 'windows', '--iso', $WindowsISO, '--edition', $Edition, '--guest-binary', $guest
)
$env:OWNSAND_HV_TEST_WINDOWS_IMAGE = Join-Path $env:OWNSAND_AGENT_STATE_DIR 'images/windows'
Assert-NoWorkerAces $env:OWNSAND_HV_TEST_WINDOWS_IMAGE
Assert-NoWorkerAces (Join-Path $env:OWNSAND_HV_TEST_WINDOWS_IMAGE 'disk.vhdx')
Assert-NoWorkerAces (Join-Path $env:OWNSAND_HV_TEST_WINDOWS_IMAGE 'image.json')

$env:OWNSAND_HV_TEST_KERNEL = Join-Path $work 'vmlinux'
Copy-Item -LiteralPath $LinuxKernel -Destination $env:OWNSAND_HV_TEST_KERNEL
$env:OWNSAND_HV_TEST_ROOTFS = $LinuxRootfs
$env:OWNSAND_HV_TEST_BUILD_VM = '1'
Remove-Item Env:OWNSAND_HV_TEST_STATE_DIR -ErrorAction SilentlyContinue
Remove-Item Env:OWNSAND_HV_TEST_OUTBOUND -ErrorAction SilentlyContinue
Invoke-LoggedNative -Log (Join-Path $work 'hyperv-lifecycle.log') -Program 'go' -Arguments @(
    'test', '-timeout=60m', '-count=1', '-v', '-run',
    '^(TestHyperVIntegration|TestHyperVWindowsIntegration|TestBuildVMDuplicateIDKeepsExistingVM)$',
    './cmds/ownsand-agent/internal/vmruntime/hcsrt'
)
Write-Output "Hyper-V ACL validation passed. Evidence directory: $work"
