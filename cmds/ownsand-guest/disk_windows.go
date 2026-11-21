// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// growScript extends the partition of C: to the largest size that the disk
// allows and prints the sizes as JSON. The agent grows the VHDX of a
// sandbox to the requested disk size before the boot, and the image build
// places the Windows partition last on the disk without a recovery
// partition behind it, so the free space follows C:. Resize-Partition also
// moves the backup GPT header to the new end of the disk.
const growScript = `$ErrorActionPreference = 'Stop'
$p = Get-Partition -DriveLetter C
$max = (Get-PartitionSupportedSize -DriveLetter C).SizeMax
$before = $p.Size
if ($max -gt $before + 16MB) { Resize-Partition -DriveLetter C -Size $max }
[pscustomobject]@{ before = $before; after = (Get-Partition -DriveLetter C).Size; max = $max } | ConvertTo-Json -Compress`

// growSystemDisk extends the Windows partition (C:) to the end of its disk
// with the Storage module of PowerShell, which every Windows edition with
// the guest helper includes.
func growSystemDisk(logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	ps := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
	out, err := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", growScript).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// The output holds sizes and PowerShell error records only.
		logger.Warn("grow the system disk failed", "error", err, "output", text)
		return
	}
	var sizes struct {
		Before int64 `json:"before"`
		After  int64 `json:"after"`
		Max    int64 `json:"max"`
	}
	if err := json.Unmarshal([]byte(text), &sizes); err != nil {
		logger.Warn("grow the system disk: unexpected output", "output", text)
		return
	}
	logger.Info("system disk checked", "before_bytes", sizes.Before, "after_bytes", sizes.After,
		"grown", sizes.After > sizes.Before, "duration_ms", time.Since(start).Milliseconds())
}
