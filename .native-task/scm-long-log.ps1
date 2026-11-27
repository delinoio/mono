$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$serviceName = 'ownsand-agent'
$policyKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem'
$longPathsEnabled = Get-ItemPropertyValue -LiteralPath $policyKey -Name 'LongPathsEnabled' -ErrorAction SilentlyContinue
if ($longPathsEnabled -eq 1) {
    throw 'Use a disposable Windows host that already has LongPathsEnabled disabled.'
}
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (!$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'This SCM and Event Log probe needs an elevated PowerShell session.'
}
if (Get-Service -Name $serviceName -ErrorAction SilentlyContinue) {
    throw 'Use a disposable host with no existing ownsand-agent service.'
}

New-Item -ItemType Directory -Path '.t3-tmp' -Force | Out-Null
$env:TEMP = (Resolve-Path '.t3-tmp').Path
$env:TMP = $env:TEMP
$env:GOTMPDIR = $env:TEMP
$probeDir = Join-Path (Resolve-Path '.t3-tmp').Path ('scm-log-' + [Guid]::NewGuid().ToString('N'))
$stateDir = $probeDir + '\' + ('a' * 90) + '\' + ('b' * 90) + '\' + ('c' * 90)
$logFile = '\\?\' + $stateDir + '\agent.log'
$exe = Join-Path $probeDir 'ownsand-agent.exe'
$serviceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$serviceName"
$sourceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\$serviceName"
$ownedSource = !(Test-Path -LiteralPath $sourceKey)
$createdService = $false
$invalidLevel = 'invalid-native-log-probe'
$expectedSetting = 'invalid OWNSAND_AGENT_LOG_LEVEL: expected debug, info, warn or error'

function Wait-ProbeResult([string] $Description, [scriptblock] $Condition) {
    $deadline = (Get-Date).AddSeconds(30)
    do {
        $service = Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'"
        if ($service.State -eq 'Stopped' -and $service.ExitCode -eq 1066 -and
            $service.ServiceSpecificExitCode -eq 2 -and (& $Condition)) {
            return
        }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    throw "Timed out checking $Description. Last SCM state was $($service.State), exit $($service.ExitCode), service exit $($service.ServiceSpecificExitCode)."
}

try {
    [IO.Directory]::CreateDirectory($probeDir) | Out-Null
    go build '-trimpath' '-o' $exe './cmds/ownsand-agent'
    if ($LASTEXITCODE -ne 0) { throw 'Could not build ownsand-agent.' }
    New-Service -Name $serviceName -BinaryPathName ('"{0}" run' -f $exe) -StartupType Manual | Out-Null
    $createdService = $true
    New-ItemProperty -Path $serviceKey -Name 'Environment' -PropertyType MultiString -Value @(
        "OWNSAND_AGENT_STATE_DIR=$stateDir", "OWNSAND_AGENT_LOG_LEVEL=$invalidLevel"
    ) | Out-Null

    foreach ($count in 1, 2) {
        # This fixture deliberately exits with a service-specific usage error.
        & sc.exe start $serviceName | Out-Host
        Wait-ProbeResult "JSON startup record $count" {
            if (![IO.File]::Exists($logFile)) { return $false }
            $lines = @([IO.File]::ReadAllLines($logFile))
            if ($lines.Length -ne $count) { return $false }
            foreach ($line in $lines) {
                $record = $line | ConvertFrom-Json
                if ($record.msg -ne 'invalid configuration' -or
                    $record.error -ne $expectedSetting -or $line.Contains($invalidLevel)) {
                    throw "Unexpected JSON startup record at service start $count."
                }
            }
            return $true
        }
    }
    Write-Output 'PASS: the service started twice and appended two JSON startup records.'

    [IO.File]::Delete($logFile)
    [IO.Directory]::CreateDirectory($logFile) | Out-Null
    $startedAt = Get-Date
    & sc.exe start $serviceName | Out-Host
    Wait-ProbeResult 'Application Event Log fallback' {
        $events = @(Get-WinEvent -FilterHashtable @{
            LogName = 'Application'
            Id = 1
            StartTime = $startedAt
        } -ErrorAction SilentlyContinue)
        foreach ($event in $events) {
            if ($event.ProviderName -ne $serviceName) { continue }
            $message = [string] $event.Properties[0].Value
            if ($message.StartsWith('could not open agent.log (Windows error ') -and
                $message.Contains($expectedSetting) -and
                !$message.Contains($stateDir) -and !$message.Contains($invalidLevel)) {
                return $true
            }
        }
        return $false
    }
    Write-Output 'PASS: an obstructed agent.log produced a redacted Application Event Log diagnostic.'
} finally {
    if ($createdService) {
        Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        & sc.exe delete $serviceName | Out-Host
        if ($ownedSource -and (Test-Path -LiteralPath $sourceKey)) {
            Remove-Item -LiteralPath $sourceKey
        }
    }
    if ([IO.Directory]::Exists($probeDir)) {
        [IO.Directory]::Delete(('\\?\' + $probeDir), $true)
    }
}
