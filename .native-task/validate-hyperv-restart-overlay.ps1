$ErrorActionPreference = 'Stop'

$source = (Resolve-Path -LiteralPath '.\servers\ownsand-api\e2e\hyperv_test.go').Path
$scratch = (Resolve-Path -LiteralPath '.\.t3-tmp').Path
$replacement = Join-Path $scratch 'hyperv-failed-replacement_test.go'
$manifest = Join-Path $scratch 'hyperv-failed-replacement.json'
$contents = [System.IO.File]::ReadAllText($source)
$pattern = '(?m)^(\ts\.killAPI\(t\)\r?\n)(\ts\.restartAPI\(t\))'
if ([regex]::Matches($contents, $pattern).Count -ne 1) {
    throw 'Expected one control plane stop followed by a restart.'
}
# Keep initial setup intact and use an empty directory after the API stops.
$injected = [regex]::Replace($contents, $pattern, ('${1}' + "`ts.bins = t.TempDir()`n" + '${2}'))
$encoding = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($replacement, $injected, $encoding)
$replace = @{}
$replace[$source] = $replacement
[System.IO.File]::WriteAllText($manifest, (@{ Replace = $replace } | ConvertTo-Json), $encoding)
