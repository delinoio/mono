//go:build linux

package firecracker

import (
	"os/exec"
	goruntime "runtime"
	"syscall"
)

// launcher starts every VM process from one dedicated OS thread that lives
// as long as the agent process.
//
// Linux sends the parent-death signal when the thread that forked the child
// exits, not only when the process exits. Go may end idle OS threads, so a
// VM started from an arbitrary goroutine could get SIGKILL while the agent
// still runs. The launcher goroutine locks its thread and never unlocks it,
// so the thread exits only with the agent process.
type launcher struct {
	reqs chan launchReq
}

type launchReq struct {
	cmd  *exec.Cmd
	done chan error
}

func newLauncher() *launcher {
	l := &launcher{reqs: make(chan launchReq)}
	started := make(chan struct{})
	go func() {
		goruntime.LockOSThread()
		// Never unlocked: the thread must live as long as the process.
		close(started)
		for r := range l.reqs {
			r.done <- r.cmd.Start()
		}
	}()
	<-started
	return l
}

// start starts cmd on the launcher thread with a parent-death signal and its
// own process group. The process stays in the agent's cgroup, which systemd
// kills when the agent exits (KillMode=control-group).
func (l *launcher) start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	cmd.SysProcAttr.Setpgid = true
	done := make(chan error, 1)
	l.reqs <- launchReq{cmd: cmd, done: done}
	return <-done
}
