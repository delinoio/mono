$ErrorActionPreference = 'Stop'

$testPattern = '^TestCopyConsole(CapsLogAcrossRedials|EndsCloserWhenPipeEnds)$'
$testNames = 'TestCopyConsoleCapsLogAcrossRedials', 'TestCopyConsoleEndsCloserWhenPipeEnds'
$legacyNames = 'ownsand-console-cap-test-com1', 'ownsand-console-closer-test-com1'
$baseline = Join-Path $PSScriptRoot 'hcsrt-start.test.exe'
$fixed = Join-Path $PSScriptRoot 'hcsrt-fixed.test.exe'
$env:TEMP = $PSScriptRoot
$env:TMP = $PSScriptRoot

function Start-FixtureRun($binary, $label, $count) {
    $log = Join-Path $PSScriptRoot "$label.stdout.log"
    $errLog = Join-Path $PSScriptRoot "$label.stderr.log"
    $testArgs = @("-test.run=$testPattern", "-test.count=$count", '-test.v', '-test.timeout=2m')
    $process = Start-Process -FilePath $binary -ArgumentList $testArgs -PassThru -NoNewWindow `
        -RedirectStandardOutput $log -RedirectStandardError $errLog
    return @{ Process = $process; Log = $log; ErrorLog = $errLog }
}

function Wait-FixtureRun($run) {
    if (!$run.Process.WaitForExit(150000)) {
        throw "Fixture process $($run.Process.Id) timed out. See $($run.Log) and $($run.ErrorLog)."
    }
}

function Assert-FixturesPassed($run, $count) {
    Wait-FixtureRun $run
    if ($run.Process.ExitCode -ne 0) {
        throw "Fixture process $($run.Process.Id) exited with $($run.Process.ExitCode). See $($run.Log) and $($run.ErrorLog)."
    }
    foreach ($testName in $testNames) {
        $passes = @(Select-String -Path $run.Log -SimpleMatch "--- PASS: $testName (")
        if ($passes.Count -ne $count) {
            throw "Expected $count passes for $testName. Found $($passes.Count) in $($run.Log)."
        }
    }
}

$holders = @()
$runs = @()
try {
    foreach ($legacyName in $legacyNames) {
        $holders += [System.IO.Pipes.NamedPipeServerStream]::new($legacyName)
    }

    $before = Start-FixtureRun $baseline 'baseline-held' 1
    $runs += $before
    Wait-FixtureRun $before
    if ($before.Process.ExitCode -eq 0) {
        throw 'The START binary unexpectedly passed with the original pipe names held.'
    }
    foreach ($legacyName in $legacyNames) {
        if (!(Select-String -Path $before.Log -SimpleMatch -Quiet "listen on \\.\pipe\${legacyName}:")) {
            throw "Expected a listener failure for $legacyName in $($before.Log)."
        }
    }
    Write-Output 'START failed to listen on both held legacy names.'

    $after = Start-FixtureRun $fixed 'fixed-held' 1
    $runs += $after
    Assert-FixturesPassed $after 1
    Write-Output 'Both fixed fixtures passed while both legacy names were held.'

    $first = Start-FixtureRun $fixed 'fixed-concurrent-1' 20
    $runs += $first
    $second = Start-FixtureRun $fixed 'fixed-concurrent-2' 20
    $runs += $second
    Assert-FixturesPassed $first 20
    Assert-FixturesPassed $second 20
    if ($first.Process.Id -eq $second.Process.Id -or
        $first.Process.ExitTime -le $second.Process.StartTime -or
        $second.Process.ExitTime -le $first.Process.StartTime) {
        throw 'The two fixed fixture processes did not overlap.'
    }
    foreach ($run in @($first, $second)) {
        Write-Output ("PID {0}: {1:o} to {2:o}. Both fixtures passed 20 times." -f `
            $run.Process.Id, $run.Process.StartTime.ToUniversalTime(), $run.Process.ExitTime.ToUniversalTime())
    }
} finally {
    foreach ($holder in $holders) {
        $holder.Dispose()
    }
    foreach ($run in $runs) {
        if (!$run.Process.HasExited) {
            $run.Process.Kill()
            $null = $run.Process.WaitForExit(5000)
        }
        $run.Process.Dispose()
    }
}
