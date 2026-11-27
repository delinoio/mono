// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func TestMCPWaitSessionReturnsDuringCredentialPublication(t *testing.T) {
	f := newFixture(t)
	rotation := newRotationFixture(t, func(_ *rotationFixture, _ string, _ int) string {
		return tokenResponse(f.srv.AccessToken, "rt-1", 3600)
	})
	f.env[EnvConfigDir] = rotation.store.Dir
	ctx, client := startMCPCommand(t, f)

	lock, err := os.OpenFile(filepath.Join(rotation.store.Dir, credstore.LockFileName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	const procedure = sandrunv1connect.DevSessionServiceGetDevSessionProcedure
	args := map[string]any{"session_id": "missing", "wait_seconds": 1}
	if got := mcpCommandToolError(t, ctx, client, "wait_session", args); !strings.HasPrefix(got, "deadline_exceeded:") {
		t.Fatalf("wait with the store locked = %q, want deadline_exceeded", got)
	}
	rotation.assertCalls("rt-0")
	rotation.assertStored("rt-0", "expired")
	if got := f.srv.Calls(procedure); got != 0 {
		t.Fatalf("expired wait sent %d RPCs", got)
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	args["wait_seconds"] = 0
	if got := mcpCommandToolError(t, ctx, client, "wait_session", args); !strings.HasPrefix(got, "not_found:") {
		t.Fatalf("lookup after unlocking the store = %q, want not_found from SandRun", got)
	}
	rotation.assertCalls("rt-0")
	rotation.assertStored("rt-1", f.srv.AccessToken)
	if got := f.srv.Calls(procedure); got != 1 {
		t.Errorf("GetDevSession calls = %d, want one from the live lookup", got)
	}
}
