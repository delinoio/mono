// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestTypeScriptSDKRelay runs packages/ownsand-sdk/src/e2e.test.ts against
// the real control plane and agent over the relay. It needs the workspace
// dependencies (`pnpm install`) and is skipped when pnpm is not on PATH.
func TestTypeScriptSDKRelay(t *testing.T) {
	requireE2E(t)
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm is not on PATH; skipping the TypeScript SDK end-to-end test")
	}
	s := newStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pnpm, "exec", "rstest", "run", "src/e2e.test.ts")
	cmd.Dir = filepath.Join(moduleRoot(t), "packages", "ownsand-sdk")
	cmd.Env = append(os.Environ(),
		"OWNSAND_E2E_API_URL="+s.apiURL,
		"OWNSAND_E2E_API_KEY="+s.apiKey,
		"OWNSAND_E2E_ORG="+s.orgID,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TypeScript SDK end-to-end test: %v\n%s", err, out)
	}
	t.Logf("TypeScript SDK end-to-end test:\n%s", out)
}
