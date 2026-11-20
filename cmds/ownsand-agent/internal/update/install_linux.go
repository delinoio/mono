//go:build linux

package update

import (
	"fmt"
	"os"
	"path/filepath"
)

// InstallBinary replaces the running agent binary with the verified
// artifact. The caller then exits, and systemd (Restart=always) starts the
// new binary.
//
// Workaround: this is a plain replace-binary-and-exit installation without a
// rollback. It exists until the release process defines a package-based
// installation (for example, a signed package with a health check and a
// rollback); remove it then.
func InstallBinary(artifact string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(artifact)
	if err != nil {
		return err
	}
	tmp := exe + ".update"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("write new binary: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}
