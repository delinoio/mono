// SPDX-License-Identifier: Apache-2.0

package launcher

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

type request struct {
	cmd  *exec.Cmd
	done chan error
}

var (
	once     sync.Once
	requests chan request
)

// thread runs on its own locked OS thread and never unlocks it, so the
// thread lives as long as the agent process.
func thread() {
	runtime.LockOSThread()
	for req := range requests {
		req.done <- req.cmd.Start()
	}
}

func start(cmd *exec.Cmd) error {
	once.Do(func() {
		requests = make(chan request)
		go thread()
	})
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	done := make(chan error, 1)
	requests <- request{cmd: cmd, done: done}
	return <-done
}
