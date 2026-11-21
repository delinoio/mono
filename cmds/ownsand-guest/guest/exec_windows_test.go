// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestTerminateEndsTheProcessTree starts cmd.exe with a ping child and
// checks that terminate ends both through the job object.
func TestTerminateEndsTheProcessTree(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 60 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var tree []uint32
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		tree, err = processTree(uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(tree) < 2 {
		t.Fatalf("process tree %v has no child", tree)
	}
	var handles []windows.Handle
	for _, pid := range tree {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
		if err != nil {
			t.Fatalf("open process %d: %v", pid, err)
		}
		defer windows.CloseHandle(h)
		handles = append(handles, h)
	}
	terminate(cmd.Process)
	for i, h := range handles {
		if ev, err := windows.WaitForSingleObject(h, 10000); err != nil || ev != windows.WAIT_OBJECT_0 {
			t.Fatalf("process %d still runs after terminate (%#x, %v)", tree[i], ev, err)
		}
	}
	_ = cmd.Wait()
}
