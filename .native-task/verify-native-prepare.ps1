param(
    [Parameter(Mandatory = $true)][string]$Repository,
    [Parameter(Mandatory = $true)][string]$Bundle,
    [Parameter(Mandatory = $true)][string]$Revision,
    [Parameter(Mandatory = $true)][string]$ScratchRoot
)

$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne 'Win32NT') {
    throw 'Run this probe in native Windows PowerShell.'
}

$repositoryPath = (Resolve-Path -LiteralPath $Repository).ProviderPath
$bundlePath = (Resolve-Path -LiteralPath $Bundle).ProviderPath
$node = (Get-Command node.exe -CommandType Application | Select-Object -First 1).Source
$git = (Get-Command git.exe -CommandType Application | Select-Object -First 1).Source
$pnpm = (Get-Command pnpm.cmd,pnpm.exe -CommandType Application -ErrorAction SilentlyContinue |
    Select-Object -First 1).Source
if (-not $pnpm) { throw 'Install pnpm 10.26.2 with a native Windows launcher on PATH.' }

$systemDirectory = [Environment]::SystemDirectory
$env:ComSpec = Join-Path $systemDirectory 'cmd.exe'
$env:PATH = (@(
    (Split-Path -Parent $node)
    (Split-Path -Parent $git)
    (Split-Path -Parent $pnpm)
    $systemDirectory
    $env:SystemRoot
) | Select-Object -Unique) -join ';'
$gitBash = Get-Command bash -All -CommandType Application -ErrorAction SilentlyContinue |
    Where-Object { (Split-Path -Parent $_.Source) -ne $systemDirectory }
if ((Get-Command sh -CommandType Application -ErrorAction SilentlyContinue) -or $gitBash) {
    throw 'Remove sh and Git Bash from PATH before testing.'
}

$null = New-Item -ItemType Directory -Force -Path $ScratchRoot
$probeRoot = Join-Path (Resolve-Path -LiteralPath $ScratchRoot).ProviderPath ('native prepare ' + [guid]::NewGuid())
$null = New-Item -ItemType Directory -Path $probeRoot
$tempPath = Join-Path $probeRoot 'tmp'
$null = New-Item -ItemType Directory -Path $tempPath
$env:TEMP = $tempPath
$env:TMP = $tempPath
Get-ChildItem Env: | Where-Object {
    $_.Name -match '^(GIT_|LEFTHOOK|NPM_CONFIG_(SCRIPT_SHELL|SHELL_EMULATOR|IGNORE_SCRIPTS|USERCONFIG|GLOBALCONFIG)$)'
} | ForEach-Object { Remove-Item -LiteralPath ('Env:' + $_.Name) }
$env:GIT_CONFIG_GLOBAL = 'NUL'
$env:GIT_CONFIG_NOSYSTEM = '1'
$emptyConfig = Join-Path $probeRoot 'empty.npmrc'
$null = New-Item -ItemType File -Path $emptyConfig
$env:npm_config_userconfig = $emptyConfig
$env:npm_config_globalconfig = $emptyConfig

function Invoke-Checked {
    param([string]$Executable, [string[]]$Arguments)
    & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Executable $Arguments exited with $LASTEXITCODE"
    }
}

function New-ProbeClone {
    param([string]$Path)
    Invoke-Checked $git @('clone', '--no-local', '--no-checkout', $repositoryPath, $Path)
    Invoke-Checked $git @('-C', $Path, 'fetch', '--no-tags', $bundlePath, 'HEAD')
    Invoke-Checked $git @('-C', $Path, 'checkout', '--detach', $Revision)
    $actual = Invoke-Checked $git @('-C', $Path, 'rev-parse', 'HEAD')
    if ($actual -ne $Revision) { throw "Expected $Revision, got $actual" }
    if (Test-Path -LiteralPath (Join-Path $Path '.git/hooks/pre-commit')) {
        throw "Fresh clone already has a pre-commit hook at $Path"
    }
}

$nodeVersion = Invoke-Checked $node @('--version')
$pnpmVersion = Invoke-Checked $pnpm @('--version')
$gitVersion = Invoke-Checked $git @('--version')
Write-Output "$nodeVersion, pnpm $pnpmVersion, $gitVersion"
if ($nodeVersion -notmatch '^v24\.' -or $pnpmVersion -ne '10.26.2' -or $gitVersion -notmatch '\.windows\.') {
    throw 'Expected Node 24, pnpm 10.26.2 and Git for Windows.'
}
Write-Output 'sh and Git Bash are absent from PATH.'

try {
    $enabledClone = Join-Path $probeRoot 'scripts enabled'
    New-ProbeClone $enabledClone
    Push-Location -LiteralPath $enabledClone
    try {
        Invoke-Checked $pnpm @('install', '--frozen-lockfile')
        if (-not (Select-String -LiteralPath '.git/hooks/pre-commit' -Pattern 'LEFTHOOK' -Quiet)) {
            throw 'Default install did not create a Lefthook-managed pre-commit hook.'
        }
        Write-Output "Default install created a managed hook at revision $Revision."
        Invoke-Checked $pnpm @('run', 'prepare')
        Invoke-Checked $node @('--test', 'scripts/ci/install-git-hooks.test.mjs')
    } finally {
        Pop-Location
    }

    $disabledClone = Join-Path $probeRoot 'scripts disabled'
    New-ProbeClone $disabledClone
    Push-Location -LiteralPath $disabledClone
    try {
        Invoke-Checked $pnpm @('install', '--frozen-lockfile', '--ignore-scripts')
        if (Test-Path -LiteralPath '.git/hooks/pre-commit') {
            throw '--ignore-scripts unexpectedly installed a pre-commit hook.'
        }
        Write-Output "The scripts-disabled control left the hook absent at revision $Revision."
    } finally {
        Pop-Location
    }
} finally {
    Remove-Item -LiteralPath $probeRoot -Recurse -Force
}
