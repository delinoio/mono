// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// forceAfterTerminate: Windows guests terminate the process tree at once.
const forceAfterTerminate = false

// treeRounds is how often terminateTree walks the process tree. A process
// that an unassigned tree member starts during a walk joins no job, so a
// second walk picks it up; children of assigned processes join the job by
// themselves.
const treeRounds = 3

// terminate ends the process tree of p through a job object: it assigns p
// and every descendant to a new job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
// and terminates the job, so children that the tree starts meanwhile die
// too. The shared exec code has no hook between process creation and the
// command's first instruction, so the job is created at the timeout and
// finds the tree by parent process ID; a descendant whose parent already
// exited is no longer linked to the tree and survives. taskkill /T /F
// remains the fallback when the job cannot be created.
func terminate(p *os.Process) {
	if err := terminateTree(uint32(p.Pid)); err != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	}
	_ = p.Kill()
}

func kill(p *os.Process) {
	_ = p.Kill()
}

// exitStatus keeps the full 32-bit exit code; Windows has no signals.
func exitStatus(state *os.ProcessState) (code int64, signal int32, signaled bool) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		return int64(ws.ExitCode), 0, false
	}
	return int64(state.ExitCode()), 0, false
}

// terminateTree terminates the process root and its descendants.
func terminateTree(root uint32) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set job limits: %w", err)
	}
	assigned := map[uint32]bool{}
	for range treeRounds {
		tree, err := processTree(root)
		if err != nil {
			return err
		}
		added := 0
		for _, pid := range tree {
			if assigned[pid] {
				continue
			}
			assigned[pid] = true
			h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
			if err != nil {
				// The process exited meanwhile.
				continue
			}
			if err := windows.AssignProcessToJobObject(job, h); err != nil {
				// A process that cannot join the job ends on its own.
				_ = windows.TerminateProcess(h, 1)
			}
			windows.CloseHandle(h)
			added++
		}
		if added == 0 {
			break
		}
	}
	if !assigned[root] {
		return fmt.Errorf("process %d is not running", root)
	}
	return windows.TerminateJobObject(job, 1)
}

// processTree returns root and its descendants by parent process ID. A
// descendant must have started after its parent, so a reused parent ID does
// not pull in unrelated processes.
func processTree(root uint32) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	children := map[uint32][]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID != e.ParentProcessID {
			children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		}
	}
	tree := []uint32{root}
	start := map[uint32]int64{root: startTime(root)}
	for i := 0; i < len(tree); i++ {
		parent := tree[i]
		for _, c := range children[parent] {
			if _, seen := start[c]; seen {
				continue
			}
			t := startTime(c)
			if t == 0 || start[parent] == 0 || t < start[parent] {
				continue
			}
			start[c] = t
			tree = append(tree, c)
		}
	}
	return tree, nil
}

// startTime returns the creation time of a process in 100 ns units, or zero
// when the process is gone.
func startTime(pid uint32) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0
	}
	return created.Nanoseconds() / 100
}
