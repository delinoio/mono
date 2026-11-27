param(
    [Parameter(Mandatory)][string]$Agent,
    [Parameter(Mandatory)][string]$WindowsAmd64Helper,
    [Parameter(Mandatory)][string]$WindowsArm64Helper,
    [Parameter(Mandatory)][string]$MacOSAmd64Helper,
    [Parameter(Mandatory)][string]$MacOSArm64Helper,
    [Parameter(Mandatory)][string]$Scratch
)
$ErrorActionPreference = 'Stop'
$Agent = (Resolve-Path -LiteralPath $Agent).Path
$WindowsAmd64Helper = (Resolve-Path -LiteralPath $WindowsAmd64Helper).Path
$WindowsArm64Helper = (Resolve-Path -LiteralPath $WindowsArm64Helper).Path
$MacOSAmd64Helper = (Resolve-Path -LiteralPath $MacOSAmd64Helper).Path
$MacOSArm64Helper = (Resolve-Path -LiteralPath $MacOSArm64Helper).Path
$root = Join-Path ((Resolve-Path -LiteralPath $Scratch).Path) ([guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $root
$originalPath = $env:PATH
$originalColor = $env:NO_COLOR
$exclusiveHandle = $null
$originalAgentEnv = @(Get-ChildItem Env:OWNSAND_AGENT_* | ForEach-Object {
    [pscustomobject]@{ Name = $_.Name; Value = $_.Value }
})

function Assert-Command($state, $options, $code, $diagnostic) {
    $ErrorActionPreference = 'Continue'
    $PSNativeCommandUseErrorActionPreference = $false
    $cliArgs = @('image', 'build', '--state-dir', $state) + $options
    $output = (& $Agent @cliArgs 2>&1 | Out-String).Trim()
    $actual = $LASTEXITCODE
    Write-Host "$(Split-Path -Leaf $state): exit $actual | $output"
    if ($actual -ne $code -or -not $output.Contains($diagnostic)) {
        throw "Expected exit $code and diagnostic '$diagnostic', got exit $actual and '$output'"
    }
}

try {
    foreach ($item in $originalAgentEnv) {
        Remove-Item -LiteralPath ("Env:" + $item.Name)
    }
    $env:PATH = $root
    $env:NO_COLOR = '1'
    $iso = Join-Path $root 'placeholder.iso'
    $text = Join-Path $root 'text-helper'
    $missing = Join-Path $root 'missing-helper'
    $unreadable = Join-Path $root 'unreadable-helper'
    $absent = Join-Path $root 'absent-state'
    $existing = Join-Path $root 'existing-state'
    $file = Join-Path $root 'state-file'
    $null = New-Item -ItemType Directory -Path $existing
    $sentinel = Join-Path $existing 'sentinel'
    [IO.File]::WriteAllText($iso, '')
    [IO.File]::WriteAllText($text, 'not an executable')
    [IO.File]::WriteAllText($unreadable, 'not an executable')
    $exclusiveHandle = [IO.File]::Open($unreadable, 'Open', 'Read', 'None')
    [IO.File]::WriteAllText($sentinel, 'keep')
    [IO.File]::WriteAllText($file, 'keep')

    # Compare the SDDL for both the state root and its existing child.
    $stateSddl = (Get-Acl -LiteralPath $existing).Sddl
    $sentinelSddl = (Get-Acl -LiteralPath $sentinel).Sddl
    $windows = @('--os', 'windows', '--iso', $iso)
    $mac = @('--os', 'macos')
    $cases = @(
        @{ Options = @('--os', 'windows', '--guest-binary', $missing); Code = 2; Diagnostic = 'needs --iso' }
        @{ Options = $windows + @('--guest-binary', $text); Code = 2; Diagnostic = 'is not a Windows executable' }
        @{ Options = $mac + @('--guest-binary', $text); Code = 2; Diagnostic = 'is not a macOS executable' }
        @{ Options = $windows + @('--guest-binary', $WindowsArm64Helper); Code = 2; Diagnostic = 'Windows guests need GOOS=windows GOARCH=amd64' }
        @{ Options = $mac + @('--guest-binary', $MacOSAmd64Helper); Code = 2; Diagnostic = 'macOS guests need arm64' }
        @{ Options = $windows + @('--guest-binary', $missing); Code = 1; Diagnostic = "open $missing" }
        @{ Options = $mac + @('--guest-binary', $missing); Code = 1; Diagnostic = "open $missing" }
        @{ Options = $windows + @('--guest-binary', $unreadable); Code = 1; Diagnostic = "open $unreadable" }
        @{ Options = $mac + @('--guest-binary', $unreadable); Code = 1; Diagnostic = "open $unreadable" }
        @{ Options = $windows; Code = 1; Diagnostic = 'ownsand-guest.exe was not found next to the agent or on PATH' }
        @{ Options = $mac; Code = 1; Diagnostic = 'ownsand-guest was not found next to the agent or on PATH' }
    )
    foreach ($case in $cases) {
        foreach ($state in @($absent, $existing, $file)) {
            Assert-Command $state $case.Options $case.Code $case.Diagnostic
            if (Test-Path -LiteralPath $absent) {
                throw 'Rejected input created state'
            }
            $entries = @(Get-ChildItem -Force -LiteralPath $existing)
            if ($entries.Count -ne 1 -or $entries[0].Name -cne 'sentinel' -or [IO.File]::ReadAllText($sentinel) -cne 'keep') {
                throw 'Rejected input changed state entries or the sentinel'
            }
            if ((Get-Acl -LiteralPath $existing).Sddl -cne $stateSddl -or (Get-Acl -LiteralPath $sentinel).Sddl -cne $sentinelSddl) {
                throw 'Rejected input changed state ACLs'
            }
            if ([IO.File]::ReadAllText($file) -cne 'keep') {
                throw 'Rejected input changed the state file'
            }
        }
    }
    Assert-Command $file ($windows + @('--guest-binary', $WindowsAmd64Helper)) 1 "store: create $file"
    Assert-Command $file ($mac + @('--guest-binary', $MacOSArm64Helper)) 1 "store: create $file"
    Write-Host 'CLI matrix passed. Rejected inputs preserved entries, contents and DACLs.'
}
finally {
    if ($null -ne $exclusiveHandle) {
        $exclusiveHandle.Dispose()
    }
    $env:PATH = $originalPath
    $env:NO_COLOR = $originalColor
    foreach ($item in $originalAgentEnv) {
        Set-Item -LiteralPath ("Env:" + $item.Name) -Value $item.Value
    }
    Remove-Item -LiteralPath $root -Recurse -Force
}
