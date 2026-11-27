[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Agent
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$Agent = (Resolve-Path -LiteralPath $Agent).Path
$scratch = (New-Item -ItemType Directory -Path '.t3-tmp/windows-state-path' -Force).FullName
$fixture = (New-Item -ItemType Directory -Path (Join-Path $scratch ([guid]::NewGuid().ToString('N')))).FullName
$target = (New-Item -ItemType Directory -Path (Join-Path $fixture 'target')).FullName
$sentinel = Join-Path $target 'identity.json'
$state = Join-Path $fixture 'state'
$logLevel = $env:OWNSAND_AGENT_LOG_LEVEL
$runtime = $env:OWNSAND_AGENT_RUNTIME
try {
    $env:OWNSAND_AGENT_LOG_LEVEL = 'info'
    $env:OWNSAND_AGENT_RUNTIME = 'hyperv'
    Set-Content -LiteralPath $sentinel -Value 'disposable state fixture'
    $initialHash = (Get-FileHash -LiteralPath $sentinel).Hash
    $initialDacl = (Get-Acl -LiteralPath $target).Sddl
    New-Item -ItemType Junction -Path $state -Target $target | Out-Null

    foreach ($mode in @('run', 'reset')) {
        $arguments = @($mode, '--state-dir', $state, '--no-color')
        if ($mode -eq 'reset') {
            $arguments += '--yes'
        }
        $stderr = Join-Path $fixture "$mode.stderr"
        $stdout = Join-Path $fixture "$mode.stdout"
        & $Agent @arguments 1> $stdout 2> $stderr
        $code = $LASTEXITCODE
        $diagnostic = Get-Content -LiteralPath $stderr -Raw
        if ($code -ne 1 -or $diagnostic -notmatch 'reparse point') {
            throw "Unexpected $mode result. Exit $code. Diagnostic $diagnostic"
        }
        $entries = @(Get-ChildItem -Force -LiteralPath $target)
        if ($entries.Count -ne 1 -or $entries[0].Name -ne 'identity.json' -or
            (Get-FileHash -LiteralPath $sentinel).Hash -ne $initialHash -or
            (Get-Acl -LiteralPath $target).Sddl -ne $initialDacl) {
            throw "The disposable junction target changed after $mode"
        }
        Write-Output "PASS $mode rejected the root junction and preserved the sentinel and target ACL"
    }
} finally {
    $env:OWNSAND_AGENT_LOG_LEVEL = $logLevel
    $env:OWNSAND_AGENT_RUNTIME = $runtime
    if ([IO.Directory]::Exists($state)) {
        [IO.Directory]::Delete($state)
    }
    Remove-Item -LiteralPath $fixture -Force -Recurse
}
