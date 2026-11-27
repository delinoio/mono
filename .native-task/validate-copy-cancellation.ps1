param([Parameter(Mandatory = $true)][string]$ExpectedSha)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$head = git rev-parse HEAD
if ($LASTEXITCODE -ne 0 -or $head -ne $ExpectedSha) {
    throw "Unexpected validation revision. Expected $ExpectedSha. Found $head."
}

$scratch = (New-Item -ItemType Directory -Path '.t3-tmp/native-copy-cancel' -Force).FullName
$drive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($scratch))
if ($drive.DriveFormat -ne 'NTFS') {
    throw "Use a writable NTFS checkout for the copy fixture. Found $($drive.DriveFormat)."
}
$env:TEMP = $scratch
$env:TMP = $scratch
$env:GOTMPDIR = $scratch
$package = './cmds/ownsand-agent/internal/vmruntime/hcsrt'
$positiveLog = Join-Path $scratch 'native-positive.log'
$negativeLog = Join-Path $scratch 'native-pre-entry-control.log'

go test '-v' '-count=10' '-timeout=2m' '-run' '^TestCopyFileEndsWithItsContext$' $package | Tee-Object -FilePath $positiveLog
if ($LASTEXITCODE -ne 0) {
    throw 'The native copy tests failed.'
}
$progressLines = @(Select-String -LiteralPath $positiveLog -Pattern 'bytes with a live context, then aborted with pbCancel set and removed the destination')
if ($progressLines.Count -ne 10) {
    throw "Expected 10 completed native cancellation checks. Found $($progressLines.Count)."
}

$target = (Resolve-Path "$package/runtime_windows_test.go").Path
$mutant = Join-Path $scratch 'runtime_windows_pre_entry_control_test.go'
$overlayPath = Join-Path $scratch 'pre-entry-overlay.json'
$source = [IO.File]::ReadAllText($target)
$call = 'err := copyFileWithProgress(ctx, src, dst, progress.attach)'
if ([regex]::Matches($source, [regex]::Escape($call)).Count -ne 1) {
    throw 'Expected one in-flight native copy call for the negative control.'
}
$control = @'
cancel()
		err := copyFileWithProgress(ctx, src, dst, progress.attach)
		if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stat after pre-entry cancellation = %v, want os.ErrNotExist", statErr)
		}
'@
[IO.File]::WriteAllText($mutant, $source.Replace($call, $control))
$files = @{}
$files[$target] = $mutant
[IO.File]::WriteAllText($overlayPath, (ConvertTo-Json -InputObject @{ Replace = $files } -Depth 4))

go test '-v' '-count=1' '-timeout=2m' "-overlay=$overlayPath" '-run' '^TestCopyFileEndsWithItsContext$/^in_flight$' $package | Tee-Object -FilePath $negativeLog
if ($LASTEXITCODE -ne 1) {
    throw 'Expected the pre-entry cancellation control to fail with exit code 1.'
}
if (-not (Select-String -Quiet -LiteralPath $negativeLog -Pattern 'CopyFileExW reported no strictly partial progress.*observed 0 of 0, copy error: context canceled')) {
    throw 'Expected the pre-entry cancellation control to fail the native progress assertion.'
}
Write-Output 'All 10 native runs passed. The pre-entry cancellation control failed at the expected assertion.'
