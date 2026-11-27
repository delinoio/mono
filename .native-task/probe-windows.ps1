param([Parameter(Mandatory = $true)][string]$Binary)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$serviceName = 'ownsand-guest'
$sourceKey = 'HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\ownsand-guest'
$binaryPath = (Resolve-Path -LiteralPath $Binary).Path
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this probe from an elevated PowerShell session.'
}
if ((Get-Service -Name $serviceName -ErrorAction SilentlyContinue) -or (Test-Path -LiteralPath $sourceKey)) {
    throw 'Use a disposable Windows guest with neither the service nor its event source present.'
}

function Wait-For([string]$Description, [scriptblock]$Condition) {
    $deadline = [DateTime]::UtcNow.AddSeconds(25)
    do {
        if (& $Condition) { return }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Description."
}

function Get-Checkpoint {
    $latest = Get-WinEvent -LogName Application -MaxEvents 1 -ErrorAction SilentlyContinue
    if ($null -eq $latest) { return 0 }
    return $latest.RecordId
}

function Get-StartupEvents([long]$After) {
    Get-WinEvent -LogName Application -FilterXPath "*[System[Provider[@Name='$serviceName'] and EventRecordID > $After]]" -ErrorAction SilentlyContinue
}

function Assert-StartupEvent($Event, [string]$Operation, [int]$Code) {
    if ($Event.Id -ne 1 -or $Event.Level -ne 2 -or $Event.Properties.Count -ne 1) {
        throw 'Expected event ID 1, level Error and a single diagnostic string.'
    }
    $payload = [string]$Event.Properties[0].Value
    $prefix = [regex]::Escape("$Operation (Windows error ${Code}: ")
    if ($payload -notmatch "^$prefix.+\)$" -or
        $payload.IndexOf('private-guest-state', [StringComparison]::OrdinalIgnoreCase) -ge 0) {
        throw 'The event lacks the operation or Win32 diagnostic, or contains the configured path.'
    }
    if ([string]::IsNullOrWhiteSpace($Event.Message) -or -not $Event.Message.Contains($payload)) {
        throw 'The Application Event Log could not render the diagnostic.'
    }
}

function Test-FailedStart([string]$Operation, [int]$Code) {
    $checkpoint = Get-Checkpoint
    & sc.exe start $serviceName | Out-Null
    Wait-For 'the startup diagnostic' { @(Get-StartupEvents $checkpoint).Count -ge 1 }
    Wait-For 'the service to stop' { (Get-Service -Name $serviceName).Status -eq 'Stopped' }
    $stopped = Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'"
    if ($stopped.ExitCode -ne 1066 -or $stopped.ServiceSpecificExitCode -ne 1) {
        throw "Unexpected SCM status $($stopped.ExitCode) / $($stopped.ServiceSpecificExitCode)."
    }
    $events = @(Get-StartupEvents $checkpoint)
    if ($events.Count -ne 1) { throw "Expected one event for a manual failed start. Got $($events.Count)." }
    Assert-StartupEvent $events[0] $Operation $Code
    Write-Output "$Operation reported Windows error $Code once with SCM status 1066 / 1."
}

function Wait-Ready {
    Wait-For 'GuestService.CheckReady' {
        try {
            $ready = Invoke-RestMethod -Uri "http://127.0.0.1:$port/delino.ownsand.v1.GuestService/CheckReady" -Method Post -ContentType 'application/json' -Headers @{'Connect-Protocol-Version' = '1'} -Body '{}' -TimeoutSec 2
            return $ready.guestOs -eq 'GUEST_OS_WINDOWS'
        } catch { return $false }
    }
}

function Invoke-SC {
    & sc.exe @args
    if ($LASTEXITCODE -ne 0) { throw "sc.exe $($args[0]) failed with exit code $LASTEXITCODE." }
}

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = $listener.LocalEndpoint.Port
$listener.Stop()
$fixture = Join-Path (Split-Path -Parent $binaryPath) ('fixture-' + [Guid]::NewGuid().ToString('N'))
$state = Join-Path $fixture 'private-guest-state'
$log = Join-Path $state 'guest.log'
$archive = "$log.1"
New-Item -ItemType Directory -Path $fixture | Out-Null
$created = $false
try {
    $commandLine = '"{0}" --state-dir "{1}" --listen tcp://127.0.0.1:{2} --clock ignore --log-level info' -f $binaryPath, $state, $port
    New-Service -Name $serviceName -BinaryPathName $commandLine -StartupType Manual | Out-Null
    $created = $true
    $account = (Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'").StartName
    if ($account -ne 'LocalSystem') { throw 'The fixture service does not run as LocalSystem.' }
    if (Test-Path -LiteralPath $sourceKey) { throw 'The event source exists before first start.' }

    [IO.File]::WriteAllText($state, 'blocks directory creation')
    Test-FailedStart 'could not create the service state directory' 3
    if (-not (Test-Path -LiteralPath $sourceKey)) { throw 'LocalSystem did not register the event source.' }

    Remove-Item -LiteralPath $state
    New-Item -ItemType Directory -Path $log -Force | Out-Null
    Test-FailedStart 'could not open guest.log' 5

    Remove-Item -LiteralPath $log
    $checkpoint = Get-Checkpoint
    $sentinel = "prior log content`n"
    [IO.File]::WriteAllText($log, $sentinel)
    Start-Service -Name $serviceName
    Wait-Ready
    Stop-Service -Name $serviceName
    $contents = [IO.File]::ReadAllText($log)
    if (-not $contents.StartsWith($sentinel) -or -not $contents.Contains('guest helper serving') -or
        @(Get-StartupEvents $checkpoint).Count -ne 0) {
        throw 'A successful service start did not append to guest.log or produced a fallback event.'
    }
    Write-Output 'Valid LocalSystem start served CheckReady and appended to guest.log with zero fallback events.'

    $size = 16MB + 1
    $active = [IO.File]::OpenWrite($log)
    try { $active.SetLength($size) } finally { $active.Dispose() }
    [IO.File]::WriteAllText($archive, 'held archive')
    $held = [IO.File]::Open($archive, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        Start-Service -Name $serviceName
        Wait-Ready
        Stop-Service -Name $serviceName
    } finally { $held.Dispose() }
    $active = [IO.File]::OpenRead($log)
    try {
        [void]$active.Seek($size, [IO.SeekOrigin]::Begin)
        $reader = [IO.StreamReader]::new($active)
        try { $records = @($reader.ReadToEnd() -split '\r?\n' | Where-Object { $_ } | ConvertFrom-Json) } finally { $reader.Dispose() }
    } finally { $active.Dispose() }
    $warning = 'log rotation failed; appending to the active log'
    if ($records.Count -eq 0 -or $records[0].msg -ne $warning -or $records[0].level -ne 'WARN' -or
        @($records | Where-Object { $_.msg -eq $warning }).Count -ne 1 -or
        [IO.File]::ReadAllText($archive) -ne 'held archive' -or @(Get-StartupEvents $checkpoint).Count -ne 0) {
        throw 'The blocked rotation did not leave a single warning in the active log and zero fallback events.'
    }
    Write-Output 'Blocked rotation served CheckReady, retained the archive and appended one warning to guest.log.'

    Invoke-SC failure $serviceName 'reset=' 86400 'actions=' 'restart/1000/restart/2000/restart/5000' | Out-Null
    Invoke-SC failureflag $serviceName 1 | Out-Null
    $policy = (Invoke-SC qfailure $serviceName | Out-String) + (Invoke-SC qfailureflag $serviceName | Out-String)
    Remove-Item -LiteralPath $state -Recurse
    [IO.File]::WriteAllText($state, 'blocks directory creation during recovery')
    $checkpoint = Get-Checkpoint
    & sc.exe start $serviceName | Out-Null
    Wait-For 'an automatic restart after the manual failure' { @(Get-StartupEvents $checkpoint).Count -ge 2 }
    $restartEvents = @(Get-StartupEvents $checkpoint | Sort-Object RecordId)
    for ($i = 0; $i -lt $restartEvents.Count; $i++) {
        Assert-StartupEvent $restartEvents[$i] 'could not create the service state directory' 3
        if ($i -gt 0 -and ($restartEvents[$i].TimeCreated - $restartEvents[$i - 1].TimeCreated).TotalMilliseconds -lt 750) {
            throw 'Two diagnostics occurred within one SCM restart interval.'
        }
    }
    $policyAfter = (Invoke-SC qfailure $serviceName | Out-String) + (Invoke-SC qfailureflag $serviceName | Out-String)
    if ($policyAfter -ne $policy) { throw 'The runtime changed the SCM recovery policy.' }
    Write-Output 'The image recovery policy remained configured and SCM automatically restarted the failed service.'
} finally {
    if ($created) {
        & sc.exe config $serviceName 'start=' 'disabled' | Out-Null
        Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        & sc.exe delete $serviceName | Out-Null
        if (Test-Path -LiteralPath $sourceKey) { Remove-Item -LiteralPath $sourceKey -Force }
    }
    Remove-Item -LiteralPath $fixture -Force -Recurse
}
Write-Output 'PASS: Windows SCM and Application Event Log probe.'
