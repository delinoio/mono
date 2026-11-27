param(
    [Parameter(Mandatory = $true)][string]$Binary,
    [Parameter(Mandatory = $true)][string]$Scratch
)

$ErrorActionPreference = 'Stop'
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run the SCM probe from an elevated PowerShell session on a disposable Windows host.'
}

$name = 'ownsand-agent'
$key = "HKLM:\SYSTEM\CurrentControlSet\Services\$name"
$eventSource = "HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\$name"
if ((Get-Service -Name $name -ErrorAction SilentlyContinue) -or (Test-Path -LiteralPath $key) -or (Test-Path -LiteralPath $eventSource)) {
    throw "The probe requires a disposable host without an existing $name service or Application Event Log source."
}

$agent = (Resolve-Path -LiteralPath $Binary).Path
$root = Join-Path (Resolve-Path -LiteralPath $Scratch).Path ("scm-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
$launcher = Join-Path $root 'ownsand-service.exe'
Copy-Item -LiteralPath $agent -Destination $launcher
$rejected = 'fixture-invalid-level'
$diagnostic = 'invalid OWNSAND_AGENT_LOG_LEVEL: expected debug, info, warn or error'
$sc = Join-Path $env:SystemRoot 'System32\sc.exe'

function Invoke-Case([string]$caseName, [string]$mode) {
    $dir = Join-Path $root $caseName
    $explicit = Join-Path $dir 'Agent State'
    $fallback = Join-Path $dir 'Fallback State'
    $level = $rejected
    $imagePath = '"{0}" run --state-dir "{1}"' -f $agent, $explicit
    $logPath = Join-Path $explicit 'agent.log'
    New-Item -ItemType Directory -Path $dir | Out-Null
    switch ($mode) {
        'migrated' { $imagePath = '"{0}" service launch run "--state-dir={1}"' -f $launcher, $explicit }
        'fallback' {
            $imagePath = '"{0}" run "--state-dir=  "' -f $agent
            $logPath = Join-Path $fallback 'agent.log'
        }
        'blocked' { New-Item -ItemType Directory -Path $logPath -Force | Out-Null }
        'valid' { $level = 'info' }
    }

    $registered = $false
    try {
        New-Service -Name $name -BinaryPathName $imagePath -StartupType Manual | Out-Null
        $registered = $true
        New-ItemProperty -LiteralPath $key -Name Environment -PropertyType MultiString -Value @(
            "OWNSAND_AGENT_LOG_LEVEL=$level"
            "OWNSAND_AGENT_STATE_DIR=$fallback"
        ) | Out-Null
        $started = Get-Date
        $start = Start-Process -FilePath $sc -ArgumentList "start $name" -Wait -PassThru -NoNewWindow `
            -RedirectStandardOutput (Join-Path $dir 'sc-start.txt') -RedirectStandardError (Join-Path $dir 'sc-stderr.txt')
        "sc.exe start exit code: $($start.ExitCode)" | Add-Content -LiteralPath (Join-Path $dir 'sc-start.txt')
        $deadline = (Get-Date).AddSeconds(30)
        do {
            $service = Get-CimInstance -ClassName Win32_Service -Filter "Name='$name'"
            if ($mode -eq 'blocked') {
                $event = @(Get-WinEvent -FilterHashtable @{
                    LogName = 'Application'; ProviderName = $name; Id = 1; StartTime = $started
                } -ErrorAction SilentlyContinue)
                $observed = $event.Count -gt 0
            } else {
                $observed = (Test-Path -LiteralPath $logPath -PathType Leaf) -and
                    ((Get-Content -LiteralPath $logPath -Raw) -match 'agent exits under the Windows service')
            }
            if ($service.State -eq 'Stopped' -and $observed) { break }
            Start-Sleep -Milliseconds 250
        } until ((Get-Date) -ge $deadline)
        $service | Select-Object State, ExitCode, ServiceSpecificExitCode |
            ConvertTo-Json | Set-Content -LiteralPath (Join-Path $dir 'scm-status.json')
        if ($service.State -ne 'Stopped' -or -not $observed) {
            throw "$caseName did not stop with the expected diagnostic. See $dir."
        }
        if ($mode -eq 'valid') {
            if ($service.ExitCode -ne 0 -or $service.ServiceSpecificExitCode -ne 0) {
                throw "$caseName did not stop successfully at missing enrollment."
            }
        } elseif ($service.ExitCode -ne 1066 -or $service.ServiceSpecificExitCode -ne 2) {
            throw "$caseName did not report SCM exit 1066 and service-specific exit 2."
        }
        if ($mode -eq 'blocked') {
            $message = ($event[0].Properties | ForEach-Object { $_.Value }) -join ' '
            $message | Set-Content -LiteralPath (Join-Path $dir 'application-event.txt')
            if ($event.Count -ne 1 -or -not $message.Contains('could not open agent.log') -or
                -not $message.Contains($diagnostic) -or $message.Contains($rejected) -or $message.Contains($explicit)) {
                throw "$caseName did not report one redacted Application Event Log diagnostic."
            }
        } else {
            $raw = Get-Content -LiteralPath $logPath -Raw
            if ($mode -eq 'valid') {
                if ($raw -notmatch 'the device is not enrolled' -or $raw.Contains('invalid configuration')) {
                    throw "$caseName did not reach normal agent startup."
                }
            } elseif ([regex]::Matches($raw, '"msg":"invalid configuration"').Count -ne 1 -or
                      -not $raw.Contains($diagnostic) -or $raw.Contains($rejected)) {
                throw "$caseName did not write one redacted configuration diagnostic."
            }
        }
        if ($mode -eq 'fallback') {
            if (Test-Path -LiteralPath $explicit) { throw "$caseName created the explicit directory." }
        } elseif (Test-Path -LiteralPath $fallback) {
            throw "$caseName created the environment fallback directory."
        }
        Write-Output ("{0}: SCM exit {1}, service-specific exit {2}" -f $caseName, $service.ExitCode, $service.ServiceSpecificExitCode)
    } finally {
        if ($registered) {
            $service = Get-Service -Name $name
            if ($service.Status -ne 'Stopped') {
                Stop-Service -Name $name -ErrorAction SilentlyContinue
            }
            $service.Dispose()
            & $sc delete $name | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "Cannot delete the probe service $name." }
            $deadline = (Get-Date).AddSeconds(15)
            while (Get-Service -Name $name -ErrorAction SilentlyContinue) {
                if ((Get-Date) -ge $deadline) { throw "The probe service is still registered." }
                Start-Sleep -Milliseconds 250
            }
        }
    }
}

try {
    Invoke-Case 'direct' 'direct'
    Invoke-Case 'migrated-command' 'migrated'
    Invoke-Case 'environment-fallback' 'fallback'
    Invoke-Case 'blocked-log' 'blocked'
    Invoke-Case 'valid-configuration' 'valid'
    Write-Output "Evidence: $root"
} finally {
    if (Test-Path -LiteralPath $eventSource) {
        Remove-Item -LiteralPath $eventSource
    }
}
