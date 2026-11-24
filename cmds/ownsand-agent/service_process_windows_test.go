// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestLauncherStopsPayloadProcess(t *testing.T) {
	const role = "OWNSAND_TEST_LAUNCHER_ROLE"
	const readyPath = "OWNSAND_TEST_PAYLOAD_READY"

	switch os.Getenv(role) {
	case "payload":
		ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
		defer stop()
		if err := os.WriteFile(os.Getenv(readyPath), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the payload did not receive the console interrupt")
		}
	case "launcher":
		t.Setenv(role, "payload")
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(os.Getenv(readyPath)); err == nil {
						stop()
						return
					}
				}
			}
		}()
		code, err := runPayloadProcess(ctx, os.Args[0], []string{"-test.run=^TestLauncherStopsPayloadProcess$"}, os.Stderr)
		if err != nil || code != exitOK {
			t.Fatalf("stop the payload: code %d, error %v", code, err)
		}
	default:
		t.Setenv(role, "launcher")
		t.Setenv(readyPath, filepath.Join(t.TempDir(), "ready"))
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLauncherStopsPayloadProcess$")
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("launch and stop a payload: %v\n%s", err, output)
		}
	}
}
