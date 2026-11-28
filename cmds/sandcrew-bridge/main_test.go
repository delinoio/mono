// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/bridge"
)

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"run"}, {"run", "--state-dir"}, {"run", "--state-dir", filepath.Join(t.TempDir(), "missing")}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != bridge.ExitUsage {
			t.Fatalf("run(%q) = %d, want %d", args, code, bridge.ExitUsage)
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%q) wrote events: %s", args, stdout.String())
		}
	}
}

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "protocol 1.0") {
		t.Fatalf("version: %d %q", code, stdout.String())
	}
}

// A state directory without start.json fails the start with exit code 2,
// after hello and a bridge_error on stdout, and writes bridge.log.
func TestRunWithoutStartConfig(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--state-dir", dir}, &stdout, &stderr); code != bridge.ExitStartFailed {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"hello"`) || !strings.Contains(lines[2], "AGENT_START_FAILED") {
		t.Fatalf("stdout %q", lines)
	}
	if !strings.Contains(stderr.String(), `"msg":"start failed"`) {
		t.Fatalf("stderr %q", stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(dir, "bridge.log")); err != nil || !bytes.Contains(data, []byte(`"msg":"start failed"`)) {
		t.Fatalf("bridge.log %q %v", data, err)
	}
}
