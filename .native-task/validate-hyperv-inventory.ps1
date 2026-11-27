param(
    [Parameter(Mandatory = $true)][string]$ExpectedSha,
    [switch]$ElevatedHyperV
)

$ErrorActionPreference = 'Stop'
$root = (Get-Location).Path
if ((git rev-parse HEAD).Trim() -ne $ExpectedSha) { throw 'Unexpected validation revision' }
$scratch = (New-Item -ItemType Directory -Force -Path '.t3-tmp/hyperv-inventory-native').FullName
$env:TEMP = $scratch
$env:TMP = $scratch
$env:GOTMPDIR = $scratch

function Write-Overlay($path, $replacements) {
    [IO.File]::WriteAllText($path, (ConvertTo-Json -InputObject @{ Replace = $replacements } -Depth 4))
}

if (-not $ElevatedHyperV) {
    $pattern = '^(TestDecodeSystemList|TestConfirmedSystemExit|TestMissingInventory|TestRunning(EnumerationFailureReturnsError|ListsSurvivorsOfThisOwner|EmptyEnumeration|LoadFailureKeepsTrackedVMs))$'
    & go test '-count=1' '-v' "-run=$pattern" './cmds/ownsand-agent/internal/vmruntime/hcsrt' |
        Tee-Object -FilePath (Join-Path $scratch 'windows-hcsrt.log')
    if ($LASTEXITCODE -ne 0) { throw 'Windows HCS inventory tests failed' }

    # Use a test-only overlay because the non-elevated host account cannot own the protected state directory.
    $aclSource = Join-Path $root 'cmds/ownsand-agent/internal/store/acl_windows.go'
    $aclCopy = Join-Path $scratch 'acl_windows.go'
    $aclOverlay = Join-Path $scratch 'manager-overlay.json'
    $signature = 'func restrictStateDir(dir string) error {'
    $text = [IO.File]::ReadAllText($aclSource)
    if (-not $text.Contains($signature)) { throw 'State-directory restriction function changed' }
    $replacement = "func restrictStateDir(string) error { return nil }" + [Environment]::NewLine + 'func restrictStateDirForFixture(dir string) error {'
    [IO.File]::WriteAllText($aclCopy, $text.Replace($signature, $replacement))
    $replacements = @{}
    $replacements[$aclSource] = $aclCopy
    Write-Overlay $aclOverlay $replacements
    $pattern = '^TestRestart(FailsWhenRuntimeInventoryFails|DeletesSurvivorFromInventory)$'
    & go test "-overlay=$aclOverlay" '-count=1' '-v' "-run=$pattern" './cmds/ownsand-agent/internal/sandboxes' |
        Tee-Object -FilePath (Join-Path $scratch 'windows-manager.log')
    if ($LASTEXITCODE -ne 0) { throw 'Windows sandbox restart tests failed' }
    return
}

$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run the live HCS probe in an elevated session on a disposable Hyper-V host'
}

$hcsSource = Join-Path $root 'cmds/ownsand-agent/internal/vmruntime/hcsrt/hcs_windows.go'
$hcsCopy = Join-Path $scratch 'hcs_windows.go'
$probeTarget = Join-Path $root 'cmds/ownsand-agent/hcs_inventory_probe_windows_test.go'
$probeCopy = Join-Path $scratch 'hcs_inventory_probe_windows_test.go'
$probeOverlay = Join-Path $scratch 'live-hcs-overlay.json'
$decodeCall = 'return decodeSystemList(doc)'
$text = [IO.File]::ReadAllText($hcsSource)
if (-not $text.Contains($decodeCall) -or -not $text.Contains('"fmt"')) { throw 'HCS query source changed' }
$injection = @'
switch os.Getenv("OWNSAND_HCS_INVENTORY_PROBE") {
	case "empty":
		doc = ""
	case "null":
		doc = "null"
	}
	return decodeSystemList(doc)
'@
[IO.File]::WriteAllText($hcsCopy, $text.Replace('"fmt"', ('"fmt"' + [Environment]::NewLine + '"os"')).Replace($decodeCall, $injection))
$probe = @'
// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/hcsrt"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestLiveHCSInventoryStartup(t *testing.T) {
	t.Setenv("OWNSAND_HCS_INVENTORY_PROBE", "")
	rt, err := hcsrt.New(hcsrt.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := rt.Running(t.Context())
	rt.Close()
	if err != nil || len(ids) != 0 {
		t.Fatalf("need a disposable Hyper-V host without OwnSand systems, got IDs %v and error %v", ids, err)
	}

	for _, scenario := range []string{"empty", "null", "normal"} {
		t.Run(scenario, func(t *testing.T) {
			cp := testcp.New(t)
			dir := filepath.Join(t.TempDir(), "state")
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			client, err := cpclient.New(cp.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Enroll(t.Context(), st, client, testcp.Token, "fixture", slog.Default()); err != nil {
				t.Fatal(err)
			}
			id, err := uuidv7.NewString()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			record := sandboxes.Record{
				SandboxID: id, State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
				Revision: 1, CreateTime: now, IdleDeadline: now.Add(time.Hour), MaxDeadline: now.Add(time.Hour),
			}
			if _, err := st.UpdateAgent(func(state *store.AgentState) error {
				state.RevisionCounter = record.Revision
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveSandbox(id, record); err != nil {
				t.Fatal(err)
			}
			marker := st.Path(store.VMsDir, id, "fixture-marker")
			if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			readRecord := func() []byte {
				t.Helper()
				data, err := os.ReadFile(st.Path(store.SandboxesDir, id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			before := readRecord()
			t.Setenv("OWNSAND_HCS_INVENTORY_PROBE", scenario)
			cfg := config.Config{StateDir: dir, Runtime: config.RuntimeKindHyperV}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			if scenario != "normal" {
				if code := runAgent(ctx, cfg); code != exitError {
					t.Fatalf("runAgent returned %d, want %d", code, exitError)
				}
				if !bytes.Equal(before, readRecord()) {
					t.Fatal("startup changed the sandbox record")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("startup removed fixture files: %v", err)
				}
				if cp.Channels() != 0 {
					t.Fatal("the agent opened its control channel after a missing inventory")
				}
				return
			}

			done := make(chan int, 1)
			go func() { done <- runAgent(ctx, cfg) }()
			env, _ := cp.WaitFor(20*time.Second, 0, func(e *ownsandv1.AgentEnvelope) bool {
				return e.GetCapabilityReport() != nil
			})
			cancel()
			select {
			case code := <-done:
				if code != exitOK {
					t.Fatalf("control startup exit = %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("agent did not stop after cancellation")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("startup did not remove the lost sandbox files, stat error = %v", err)
			}
			for _, check := range env.GetCapabilityReport().GetCapabilities().GetHypervisorChecks() {
				if check.GetCheckId() == "hyper-v" && check.GetPassed() {
					return
				}
			}
			t.Fatal("control plane did not receive a passing hyper-v check")
		})
	}
}
'@
[IO.File]::WriteAllText($probeCopy, $probe)
$replacements = @{}
$replacements[$hcsSource] = $hcsCopy
$replacements[$probeTarget] = $probeCopy
Write-Overlay $probeOverlay $replacements
$liveLog = Join-Path $scratch 'windows-live-hcs.log'
& go test "-overlay=$probeOverlay" '-count=1' '-v' '-timeout=4m' '-run=^TestLiveHCSInventoryStartup$' './cmds/ownsand-agent' |
    Tee-Object -FilePath $liveLog
if ($LASTEXITCODE -ne 0) { throw 'Live HCS startup probe failed' }
$failures = @(Select-String -LiteralPath $liveLog -Pattern 'agent start failed.*hcsrt: compute system list: missing result document')
if ($failures.Count -ne 2) { throw 'Expected startup to report the missing list diagnostic for both injected results' }
