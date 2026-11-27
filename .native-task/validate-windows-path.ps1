$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$serviceName = 'ownsand-guest'
$helper = Join-Path $root 'ownsand-guest.exe'
$probe = Join-Path $root 'exec-path-probe.exe'
$tests = Join-Path $root 'guest.test.exe'
$state = Join-Path $root 'state'
$fixtures = Join-Path $root 'fixtures'
$testTemp = Join-Path $root 'test-temp'

if (Get-Service -Name $serviceName -ErrorAction SilentlyContinue) {
    throw 'Use a disposable Windows guest without an existing ownsand-guest service.'
}
foreach ($path in @($helper, $probe, $tests)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Missing test artifact $path"
    }
}
if (Test-Path -LiteralPath $state) {
    throw 'Use a fresh validation directory for isolated service state.'
}
New-Item -ItemType Directory -Path $fixtures, $testTemp -Force | Out-Null
Copy-Item -LiteralPath $probe -Destination (Join-Path $fixtures 'cmd.exe')
Copy-Item -LiteralPath $probe -Destination (Join-Path $fixtures 'ownsand-path-fixture.exe')
$env:TEMP = $testTemp
$env:TMP = $testTemp
$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = $listener.LocalEndpoint.Port
$listener.Stop()
$address = '127.0.0.1:{0}' -f $port
$binaryPath = '"{0}" --listen tcp://{1} --state-dir "{2}" --clock ignore' -f $helper, $address, $state

Start-Transcript -Path (Join-Path $root 'windows-path-validation.log') | Out-Null
$created = $false
try {
    Get-FileHash -Algorithm SHA256 $helper, $probe, $tests | Format-Table -AutoSize
    & $tests '-test.v' '-test.count=1' '-test.run=^(TestLookPathInWindows|TestExecBareNameUsesRequestPathWindows|TestRequestSetsPathFoldsKeyCase)$'
    if ($LASTEXITCODE -ne 0) {
        throw "Native guest regression tests exited $LASTEXITCODE"
    }

    New-Service -Name $serviceName -BinaryPathName $binaryPath -StartupType Manual | Out-Null
    $created = $true
    Start-Service -Name $serviceName
    $service = Get-CimInstance Win32_Service -Filter "Name='$serviceName'"
    $service | Select-Object Name, State, StartName
    if ($service.State -ne 'Running' -or $service.StartName -ne 'LocalSystem') {
        throw 'The guest helper must be running as a LocalSystem service.'
    }
    & $probe '-url' "http://$address" '-fixtures' $fixtures
    if ($LASTEXITCODE -ne 0) {
        throw "Service exec probe exited $LASTEXITCODE"
    }
} finally {
    try {
        if ($created) {
            Stop-Service -Name $serviceName -ErrorAction Continue
            sc.exe delete $serviceName
            if ($LASTEXITCODE -ne 0) {
                throw "Service removal exited $LASTEXITCODE"
            }
        }
    } finally {
        Stop-Transcript | Out-Null
    }
}
