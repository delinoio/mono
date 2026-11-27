param(
    [Parameter(Mandatory=$true)]
    [string] $ExpectedSha
)

$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) {
    throw 'Run this script with Windows PowerShell 5.1 (powershell.exe).'
}

$repo = & git rev-parse --show-toplevel
if ($LASTEXITCODE -ne 0 -or (Get-Location).Path -ne (Resolve-Path -LiteralPath $repo).Path) {
    throw 'Run this script from the root of the validation checkout.'
}
$actualSha = & git rev-parse HEAD
if ($LASTEXITCODE -ne 0 -or $actualSha -ne $ExpectedSha) {
    throw "Expected revision $ExpectedSha, found $actualSha."
}
if (& git status --porcelain --untracked-files=no) {
    throw 'The validation checkout has uncommitted changes.'
}

function Invoke-ValidationTests {
    param([string] $Pattern, [string] $Log, [string[]] $Required)
    & go test -count=1 -json ./go/sandrun -run $Pattern | Tee-Object -FilePath $Log
    if ($LASTEXITCODE -ne 0) {
        throw "Go tests failed. See $Log."
    }
    $events = Get-Content -LiteralPath $Log | ForEach-Object { ConvertFrom-Json -InputObject $_ }
    if ($events | Where-Object { $_.Action -eq 'skip' }) {
        throw "At least one validation test was skipped. See $Log."
    }
    foreach ($test in $Required) {
        if (-not ($events | Where-Object { $_.Action -eq 'pass' -and $_.Test -eq $test })) {
            throw "Missing passing result for $test. See $Log."
        }
    }
}

$scratch = Join-Path $repo '.t3-tmp'
New-Item -ItemType Directory -Path $scratch -Force | Out-Null
$previousTemp = $env:TEMP
$previousTmp = $env:TMP
$previousGoTmp = $env:GOTMPDIR
try {
    $env:TEMP = $scratch
    $env:TMP = $scratch
    $env:GOTMPDIR = $scratch
    Write-Output "Revision: $actualSha"
    Write-Output "Windows PowerShell: $($PSVersionTable.PSVersion)"
    & go version
    if ($LASTEXITCODE -ne 0) { throw 'Go is required.' }

    $listTests = @(
        'TestWindowsGuestSyncListPaths',
        'TestWindowsGuestApplyReplacementAndPruneListPaths',
        'TestWindowsGuestApplyLinkListPaths',
        'TestSyncPathListEncoding',
        'TestSyncWindowsLinkListEncoding',
        'TestGuestCommandsAndPaths'
    )
    Invoke-ValidationTests -Pattern ('^(' + ($listTests -join '|') + ')$') `
        -Log (Join-Path $scratch 'windows-sync-list-preamble.jsonl') -Required $listTests

    & "$env:SystemRoot\System32\tar.exe" --version
    if ($LASTEXITCODE -ne 0) { throw 'Windows tar.exe is required for the ASCII retry control.' }
    $retry = 'TestWindowsGuestSyncRetryFileToDirectory'
    $scenarios = @('first_apply', 'manifest_write_failure', 'ordinary_deletion_failure')
    $retryTests = @($scenarios | ForEach-Object { "$retry/$_" })
    Invoke-ValidationTests -Pattern ('^' + $retry + '$/^(' + ($scenarios -join '|') + ')$') `
        -Log (Join-Path $scratch 'windows-sync-ascii-retry.jsonl') -Required $retryTests
} finally {
    $env:TEMP = $previousTemp
    $env:TMP = $previousTmp
    $env:GOTMPDIR = $previousGoTmp
}
