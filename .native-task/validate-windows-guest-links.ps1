param(
    [Parameter(Mandatory=$true)][ValidateSet('Permitted', 'Denied', 'Restored')][string]$Phase
)
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$utf8 = [Console]::OutputEncoding
if (!$env:SANDRUN_API_URL -or ([Uri]$env:SANDRUN_API_URL).Host -eq 'sandrun.api.delino.io' -or
    !$env:SANDRUN_ORG -or !$env:SANDRUN_LINK_VALIDATION_SESSION) {
    throw 'Set SANDRUN_API_URL to a non-production API and supply SANDRUN_ORG and SANDRUN_LINK_VALIDATION_SESSION'
}
$id = $env:SANDRUN_LINK_VALIDATION_SESSION
$bin = Join-Path $PSScriptRoot 'sandrun.exe'
function Invoke-SandRun([string[]]$Arguments) {
    $output = & $bin --no-color --output json @Arguments
    if ($LASTEXITCODE -ne 0) { throw "SandRun exited $LASTEXITCODE" }
    return $output
}
function Invoke-Git([string[]]$Arguments) {
    $output = & git.exe -C $fixture @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Git exited $LASTEXITCODE" }
    return $output
}
$session = (Invoke-SandRun @('session', 'get', $id)) | ConvertFrom-Json
if ($session.guest_os -ne 'GUEST_OS_WINDOWS' -or !$session.workspace_path) {
    throw 'The selected session must be a running disposable Windows dev session'
}
$ws = $session.workspace_path.TrimEnd('\')
$failedPath = 'dir/caf' + [char]0x00e9
$links = [ordered]@{}
if ($Phase -eq 'Permitted') {
    $links['links/main.txt'] = '../src/main.txt'
    $links['links/src'] = '../src'
    $links['links/absolute'] = ($ws + '\src\main.txt').Replace('\', '/')
    $fixture = Join-Path $PSScriptRoot 'fixture-permitted'
} else {
    $links[$failedPath] = '../src/main.txt'
    $fixture = Join-Path $PSScriptRoot 'fixture-permission-retry'
}

if ($Phase -ne 'Restored') {
    New-Item -ItemType Directory -Path $fixture | Out-Null
    Invoke-Git @('init', '-q') | Out-Null
    Invoke-Git @('config', '--local', 'core.symlinks', 'false') | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $fixture 'src') | Out-Null
    [IO.File]::WriteAllText((Join-Path $fixture 'src/main.txt'), 'payload', $utf8)
    Invoke-Git @('add', '--', 'src/main.txt') | Out-Null
    foreach ($entry in $links.GetEnumerator()) {
        $path = Join-Path $fixture $entry.Key
        [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($path)) | Out-Null
        [IO.File]::WriteAllText($path, $entry.Value, $utf8)
        $oid = Invoke-Git @('hash-object', '-w', '--', $entry.Key)
        Invoke-Git @('update-index', '--add', '--cacheinfo', "120000,$oid,$($entry.Key)") | Out-Null
    }
} elseif (!(Test-Path -LiteralPath (Join-Path $fixture '.git'))) {
    throw 'Run the denied phase first and keep its local Git worktree'
}

$inspect = @'
$ErrorActionPreference = 'Stop'
$ws = $env:SANDRUN_LINK_PROBE_WORKSPACE
if ([IO.File]::ReadAllText((Join-Path $ws 'src/main.txt')) -cne 'payload') { throw 'Incorrect regular file' }
if ($env:SANDRUN_LINK_PROBE_PHASE -eq 'Permitted') {
    $checks = @{'links\main.txt' = '..\src\main.txt'; 'links\src' = '..\src'; 'links\absolute' = ($ws + '\src\main.txt')}
} else {
    $path = 'dir\caf' + [char]0x00e9
    $checks = @{}
    if ($env:SANDRUN_LINK_PROBE_PHASE -eq 'Denied') {
        if (Test-Path -LiteralPath (Join-Path $ws $path)) { throw 'A denied link was created' }
    } else {
        $checks[$path] = '..\src\main.txt'
    }
}
foreach ($entry in $checks.GetEnumerator()) {
    $path = Join-Path $ws $entry.Key
    $item = Get-Item -LiteralPath $path
    $target = $item.Target | Select-Object -First 1
    if ($item.LinkType -ne 'SymbolicLink' -or $target -cne $entry.Value) {
        throw "Wrong link type or target for $($entry.Key)"
    }
    if ($entry.Key -eq 'links\src') { $path = Join-Path $path 'main.txt' }
    if ([IO.File]::ReadAllText($path) -cne 'payload') { throw "Cannot read through $($entry.Key)" }
}
[Console]::WriteLine([Convert]::ToBase64String([IO.File]::ReadAllBytes($ws + '.sandrun-sync.json')))
'@
$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($inspect))
function Check-Manifest {
    $events = Invoke-SandRun @('session', 'exec', '--env', "SANDRUN_LINK_PROBE_WORKSPACE=$ws", '--env', "SANDRUN_LINK_PROBE_PHASE=$Phase", $id, '--',
        'powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded)
    $body = (($events | ForEach-Object { $_ | ConvertFrom-Json } | Where-Object event -eq 'stdout' | ForEach-Object data) -join '').Trim()
    $manifest = $utf8.GetString([Convert]::FromBase64String($body)) | ConvertFrom-Json
    if ($manifest.version -ne 1 -or !$manifest.files.'src/main.txt') { throw 'Missing v1 manifest or regular-file row' }
    if ($Phase -eq 'Denied') {
        if (@($manifest.files.PSObject.Properties).Count -ne 1) { throw 'Denied links appear in the manifest' }
        return
    }
    if (@($manifest.files.PSObject.Properties).Count -ne $links.Count+1) { throw 'Incorrect manifest count' }
    $hash = [Security.Cryptography.SHA256]::Create()
    try {
        foreach ($entry in $links.GetEnumerator()) {
            $bytes = $utf8.GetBytes($entry.Value)
            $digest = [BitConverter]::ToString($hash.ComputeHash($bytes)).Replace('-', '').ToLowerInvariant()
            $row = $manifest.files.PSObject.Properties[$entry.Key].Value
            if ($row.mode -ne 40960 -or $row.size -ne $bytes.Length -or $row.sha256 -cne $digest) {
                throw "Incorrect symlink manifest row for $($entry.Key)"
            }
        }
    } finally { $hash.Dispose() }
}

foreach ($pass in 1, 2) {
    $result = (Invoke-SandRun @('session', 'sync', $id, $fixture)) | ConvertFrom-Json
    $expectedUploads = 0
    if ($pass -eq 1) {
        $expectedUploads = if ($Phase -eq 'Permitted') { 4 } else { 1 }
    }
    if ($result.uploaded -ne $expectedUploads) { throw "Unexpected uploaded count in $Phase pass $pass" }
    if ($Phase -eq 'Denied') {
        $warnings = @($result.warnings)
        if ($warnings.Count -ne 1 -or $warnings[0] -cne "${failedPath}: the guest could not create the symbolic link") {
            throw 'Missing exact Unicode warning'
        }
        if ($pass -eq 2) {
            $record = $failedPath.Replace('/', '\') + [char]9 + '..\src\main.txt' + [char]10
            if ($result.skipped -ne 1 -or $result.bytes_sent -ne $utf8.GetByteCount($record)) {
                throw 'Unchanged denied sync did not send exactly the original one-link record'
            }
        }
    } elseif ($result.warnings -or ($pass -eq 2 -and ($result.bytes_sent -ne 0 -or $result.skipped -ne $links.Count+1))) {
        throw "Unexpected warnings or re-upload in $Phase"
    }
    Check-Manifest
    Write-Output "PASS $Phase sync $pass (uploaded=$($result.uploaded), skipped=$($result.skipped), bytes=$($result.bytes_sent))"
}
