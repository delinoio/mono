param([Parameter(Mandatory = $true)][string]$ExpectedSha)

$ErrorActionPreference = 'Stop'
$actualSha = git rev-parse HEAD
if ($LASTEXITCODE -ne 0 -or $actualSha -ne $ExpectedSha) {
    throw "Expected checkout $ExpectedSha, found $actualSha."
}
$source = 'cmds/ownsand-agent/service_windows_test.go'
$trackedChanges = git status --porcelain --untracked-files=no
if ($LASTEXITCODE -ne 0 -or $trackedChanges) {
    throw 'Use a clean checkout of the expected revision.'
}
Write-Host "Revision $actualSha"
go version
if ($LASTEXITCODE -ne 0) { throw 'Go is required.' }

$scratch = Join-Path (Get-Location).Path '.t3-tmp/windows-startup-log-fixture'
New-Item -ItemType Directory -Path $scratch -Force | Out-Null
$positiveLog = Join-Path $scratch 'positive.log'
$negativeLog = Join-Path $scratch 'negative.log'
$package = './cmds/ownsand-agent'
$test = '^TestStartupDiagnosticForInvalidConfiguration$'

& go test -mod=readonly -count=1 -v $package -run $test | Tee-Object -FilePath $positiveLog
if ($LASTEXITCODE -ne 0 -or (Get-Content -Raw $positiveLog) -notmatch '--- PASS: TestStartupDiagnosticForInvalidConfiguration/file_log_works') {
    throw 'The normal fixture did not pass the working-file case.'
}

$sourcePath = (Resolve-Path -LiteralPath $source).Path
$copyPath = Join-Path $scratch 'service_windows_test.go'
$overlayPath = Join-Path $scratch 'overlay.json'
$encoding = [System.Text.UTF8Encoding]::new($false)
$original = [IO.File]::ReadAllText($sourcePath)
$needle = "`t`t`tdata, err := os.ReadFile(path)"
if ([regex]::Matches($original, [regex]::Escape($needle)).Count -ne 1) {
    throw 'Could not identify one log read in the working-file fixture.'
}
$injection = @'
			fixtureLog, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = fixtureLog.WriteString(rawPath + "\n")
			closeErr := fixtureLog.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
'@
$replace = @{}
$replace[$sourcePath] = $copyPath
try {
    [IO.File]::WriteAllText($copyPath, $original.Replace($needle, $injection + "`n" + $needle), $encoding)
    $overlay = @{ Replace = $replace } | ConvertTo-Json -Depth 3
    [IO.File]::WriteAllText($overlayPath, $overlay, $encoding)

    & go test -mod=readonly -count=1 -v "-overlay=$overlayPath" $package -run "$test/^file_log_works$" | Tee-Object -FilePath $negativeLog
    $negativeExit = $LASTEXITCODE
    $failure = Get-Content -Raw $negativeLog
    if ($negativeExit -ne 1 -or $failure -notmatch '--- FAIL: TestStartupDiagnosticForInvalidConfiguration/file_log_works' -or $failure -notmatch 'unexpected log contents: ' -or $failure -notmatch 'private-fixture-state' -or $failure -match 'private-fixture-level') {
        throw "Expected the log assertion to reject only the appended state-path marker. Go test exit was $negativeExit."
    }
    Write-Host 'The working-file assertion rejected the appended synthetic state-path marker.'
} finally {
    Remove-Item -LiteralPath $copyPath, $overlayPath -ErrorAction SilentlyContinue
}

$trackedChanges = git status --porcelain --untracked-files=no
if ($LASTEXITCODE -ne 0 -or $trackedChanges) {
    throw 'The tracked checkout changed during validation.'
}
