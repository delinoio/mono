// SPDX-License-Identifier: FSL-1.1-ALv2

package processrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/delinoio/mono/internal/procgroup"
	"golang.org/x/sys/unix"
)

const (
	supervisorArg = "--ownsand-process-runtime-supervisor"
	controlFD     = 3
	replyFD       = 4
)

type supervisorReply struct {
	PID   int
	Errno syscall.Errno
	Error string
}

// The process runtime also runs inside Go test binaries. Dispatch the
// re-exec here so that both those binaries and ownsand-agent use the same
// supervisor.
func init() {
	if len(os.Args) < 3 || os.Args[1] != supervisorArg {
		return
	}
	control := os.NewFile(controlFD, "guest-helper-control")
	replies := os.NewFile(replyFD, "guest-helper-replies")
	for _, pipe := range []*os.File{control, replies} {
		info, err := pipe.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			fmt.Fprintln(os.Stderr, "processrt: guest supervisor requires inherited pipes")
			os.Exit(1)
		}
	}
	// These inherited descriptors belong only to the supervisor. In
	// particular, the guest must not inherit the supervisor's reply writer.
	unix.CloseOnExec(controlFD)
	unix.CloseOnExec(replyFD)
	if err := superviseGuest(os.Args[2:], control, replies); err != nil {
		_ = json.NewEncoder(replies).Encode(supervisorReply{Error: err.Error()})
		os.Exit(1)
	}
	os.Exit(0)
}

func startHelper(cmd *exec.Cmd, _ *slog.Logger) (helperProcess, func(), error) {
	self, err := os.Executable()
	if err != nil {
		return helperProcess{}, nil, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return helperProcess{}, nil, err
	}
	defer controlRead.Close()
	replyRead, replyWrite, err := os.Pipe()
	if err != nil {
		controlWrite.Close()
		return helperProcess{}, nil, err
	}
	defer replyWrite.Close()

	supervisor := exec.Command(self, append([]string{supervisorArg}, cmd.Args...)...)
	supervisor.Dir, supervisor.Env = cmd.Dir, cmd.Env
	supervisor.Stdout, supervisor.Stderr = cmd.Stdout, cmd.Stderr
	supervisor.ExtraFiles = []*os.File{controlRead, replyWrite}
	// Keep the supervisor out of both the agent's and helper's process
	// groups. It must survive their signals long enough to reap the helper.
	procgroup.Set(supervisor)
	if err := supervisor.Start(); err != nil {
		controlWrite.Close()
		replyRead.Close()
		return helperProcess{}, nil, err
	}
	// The agent retains only the control writer and reply reader. EOF on
	// the control pipe then reports an agent exit, including SIGKILL.
	controlRead.Close()
	replyWrite.Close()
	wait := func() {
		_ = supervisor.Wait()
		controlWrite.Close()
		replyRead.Close()
	}

	decoder := json.NewDecoder(replyRead)
	var ready supervisorReply
	if err := decoder.Decode(&ready); err != nil {
		controlWrite.Close()
		wait()
		return helperProcess{}, nil, fmt.Errorf("read guest supervisor startup: %w", err)
	}
	if ready.PID <= 0 {
		controlWrite.Close()
		wait()
		if ready.Error == "" {
			return helperProcess{}, nil, errors.New("guest supervisor did not report a helper PID")
		}
		return helperProcess{}, nil, fmt.Errorf("guest supervisor startup: %s", ready.Error)
	}

	var mu sync.Mutex
	signal := func(sig syscall.Signal) error {
		// Keep each signal paired with its reply when lifecycle calls overlap.
		mu.Lock()
		defer mu.Unlock()
		if _, err := controlWrite.Write([]byte{byte(sig)}); err != nil {
			return errHelperExited
		}
		var reply supervisorReply
		if err := decoder.Decode(&reply); err != nil {
			return errHelperExited
		}
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		if reply.Errno != 0 {
			return reply.Errno
		}
		return nil
	}
	return helperProcess{pid: ready.PID, signal: signal}, wait, nil
}

func superviseGuest(args []string, control, replies *os.File) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("open guest supervisor kqueue: %w", err)
	}
	defer unix.Close(queue)
	unix.CloseOnExec(queue)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := false
	defer func() {
		if !exited {
			// SIGKILL also ends a paused helper. Keep the child PID reserved
			// until all signals to its process group have finished.
			_ = procgroup.Signal(cmd.Process, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()

	var changes [2]unix.Kevent_t
	unix.SetKevent(&changes[0], int(control.Fd()), unix.EVFILT_READ, unix.EV_ADD)
	unix.SetKevent(&changes[1], cmd.Process.Pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	changes[1].Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(queue, changes[:], nil, nil); err != nil {
		return fmt.Errorf("watch guest helper %d: %w", cmd.Process.Pid, err)
	}
	encoder := json.NewEncoder(replies)
	if err := encoder.Encode(supervisorReply{PID: cmd.Process.Pid}); err != nil {
		return nil // The agent closed its reply reader.
	}

	// One event loop owns all group signals and the wait. NOTE_EXIT reports
	// the child's exit without reaping it, so no signal can hit a reused PID.
	var events [1]unix.Kevent_t
	for {
		_, err := unix.Kevent(queue, nil, events[:], nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for guest helper %d: %w", cmd.Process.Pid, err)
		}
		event := events[0]
		if event.Filter == unix.EVFILT_PROC {
			exited = true
			return nil
		}
		if event.Flags&unix.EV_EOF != 0 {
			return nil
		}
		var request [1]byte
		if _, err := control.Read(request[:]); err != nil {
			return nil
		}
		sig := syscall.Signal(request[0])
		switch sig {
		case syscall.SIGSTOP, syscall.SIGCONT, syscall.SIGKILL:
		default:
			return fmt.Errorf("unsupported guest helper signal %d", sig)
		}
		var reply supervisorReply
		if err := procgroup.Signal(cmd.Process, sig); err != nil {
			if !errors.As(err, &reply.Errno) {
				reply.Error = err.Error()
			}
		}
		if err := encoder.Encode(reply); err != nil {
			return nil
		}
	}
}
