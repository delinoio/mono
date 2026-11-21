// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// launchRecordFile is the name of the launch record inside a VM directory.
const launchRecordFile = "launch.json"

// launchRecordVersion is the format version of the launch record.
const launchRecordVersion = 1

// launchRecord proves that the process with PID was started by this agent
// for the sandbox: the agent writes it right after it starts firecracker.
// A later agent process signals a firecracker process only when the process
// still matches the record. The boot ID and the start time (field 22 of
// /proc/<pid>/stat) make a reused PID of another process, or of an earlier
// boot, not match.
type launchRecord struct {
	Version   int    `json:"version"`
	SandboxID string `json:"sandbox_id"`
	PID       int    `json:"pid"`
	BootID    string `json:"boot_id"`
	StartTime uint64 `json:"start_time"`
}

// launchSkipReason says why a process is not eligible for clean-up. The
// values are stable identifiers of the structured diagnostic.
type launchSkipReason string

const (
	// skipRecordMissing: the VM directory has no launch record. This is the
	// case for a VM of an agent version that did not write records.
	skipRecordMissing launchSkipReason = "launch_record_missing"
	// skipRecordUnreadable: the record cannot be read or parsed, or its
	// version is unknown.
	skipRecordUnreadable launchSkipReason = "launch_record_unreadable"
	// skipRecordForeign: the record or its directory is not private to this
	// agent (a link, another owner, group or world access) or names another
	// sandbox than its directory.
	skipRecordForeign launchSkipReason = "launch_record_foreign"
	// skipBootChanged: the record is of an earlier boot of the host.
	skipBootChanged launchSkipReason = "boot_id_mismatch"
	// skipProcessGone: the recorded process no longer exists.
	skipProcessGone launchSkipReason = "process_gone"
	// skipProcessChanged: a process with the recorded PID exists but is not
	// the recorded process (another start time or command line).
	skipProcessChanged launchSkipReason = "process_mismatch"
)

// errProcessGone reports that a process no longer exists.
var errProcessGone = errors.New("fcrt: process does not exist")

// errVerifyIO marks a failure to inspect the system (an unreadable /proc
// entry, an exhausted descriptor table) while verifying a launch. It is not
// evidence about the process: the caller must not treat the process as
// foreign or forget the VM, and retries later.
var errVerifyIO = errors.New("fcrt: cannot inspect the process")

// verifyIOError wraps err so that errors.Is reports errVerifyIO.
func verifyIOError(err error) error { return fmt.Errorf("%w: %w", errVerifyIO, err) }

// procHandle is a stable reference to one verified process. It stays bound
// to that process even when its PID is reused after the process exits.
type procHandle interface {
	// kill sends SIGKILL; it reports errProcessGone for an ended process.
	kill() error
	// waitExit waits up to d for the process to end and reports whether it
	// did.
	waitExit(d time.Duration) bool
	close()
}

// procHost reads the process table and opens process handles. The
// production implementation reads /proc; tests inject fixtures.
type procHost interface {
	bootID() (string, error)
	// startTime returns field 22 of /proc/<pid>/stat.
	startTime(pid int) (uint64, error)
	cmdline(pid int) ([]string, error)
	listPIDs() ([]int, error)
	// open returns a handle to the process now owning pid.
	open(pid int) (procHandle, error)
}

// linuxProcs is the procHost of the running system. root is /proc; tests
// point it at a fixture tree and read the live process table with it.
type linuxProcs struct{ root string }

func newLinuxProcs() procHost { return linuxProcs{root: "/proc"} }

func (p linuxProcs) bootID() (string, error) {
	raw, err := os.ReadFile(filepath.Join(p.root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (p linuxProcs) startTime(pid int) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "stat"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, errProcessGone
		}
		return 0, err
	}
	return parseStartTime(string(raw))
}

// parseStartTime extracts field 22 (starttime) of a /proc/<pid>/stat line.
// The command name (field 2) is in parentheses and may contain spaces and
// parentheses, so the parser counts from the last closing parenthesis.
func parseStartTime(stat string) (uint64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("fcrt: malformed stat line")
	}
	// The remainder starts with field 3 (state); starttime is field 22.
	fields := strings.Fields(stat[end+1:])
	const startTimeField = 22
	idx := startTimeField - 3
	if len(fields) <= idx {
		return 0, fmt.Errorf("fcrt: stat line has %d fields after the command name", len(fields))
	}
	return strconv.ParseUint(fields[idx], 10, 64)
}

func (p linuxProcs) cmdline(pid int) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(p.root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errProcessGone
		}
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
}

func (p linuxProcs) listPIDs() ([]int, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (p linuxProcs) open(pid int) (procHandle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, errProcessGone
		}
		return nil, err
	}
	return pidfd(fd), nil
}

// pidfd is a process file descriptor (Linux 5.3 and later).
type pidfd int

func (f pidfd) kill() error {
	err := unix.PidfdSendSignal(int(f), unix.SIGKILL, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return errProcessGone
	}
	return err
}

// waitExit polls the descriptor, which becomes readable when the process
// ends.
func (f pidfd) waitExit(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left < 0 {
			left = 0
		}
		fds := []unix.PollFd{{Fd: int32(f), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n > 0
	}
}

func (f pidfd) close() { _ = unix.Close(int(f)) }

// writeLaunchRecord writes the launch record of the process pid into the VM
// directory dir. The file is private (mode 0600) and replaced atomically.
func writeLaunchRecord(host procHost, dir, sandboxID string, pid int) error {
	boot, err := host.bootID()
	if err != nil {
		return fmt.Errorf("read boot ID: %w", err)
	}
	start, err := host.startTime(pid)
	if err != nil {
		return fmt.Errorf("read process start time: %w", err)
	}
	raw, err := json.Marshal(launchRecord{Version: launchRecordVersion, SandboxID: sandboxID, PID: pid, BootID: boot, StartTime: start})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, launchRecordFile+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, launchRecordFile))
}

// privateToAgent reports whether the file is owned by the agent's user and
// closed to group and world access.
func privateToAgent(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid() && st.Mode().Perm()&0o077 == 0
}

// readLaunchRecord reads and structurally validates the record of the VM
// directory dir of the sandbox.
func readLaunchRecord(dir, sandboxID string) (launchRecord, launchSkipReason, error) {
	dst, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return launchRecord{}, skipRecordMissing, nil
		}
		return launchRecord{}, skipRecordUnreadable, verifyIOError(err)
	}
	if !dst.IsDir() || !privateToAgent(dst) {
		return launchRecord{}, skipRecordForeign, fmt.Errorf("VM directory %s is not a private directory of this agent", dir)
	}
	path := filepath.Join(dir, launchRecordFile)
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return launchRecord{}, skipRecordMissing, nil
		}
		return launchRecord{}, skipRecordUnreadable, verifyIOError(err)
	}
	if !st.Mode().IsRegular() || !privateToAgent(st) {
		return launchRecord{}, skipRecordForeign, fmt.Errorf("launch record %s is not a private regular file of this agent", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return launchRecord{}, skipRecordMissing, nil
		}
		return launchRecord{}, skipRecordUnreadable, verifyIOError(err)
	}
	var rec launchRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return launchRecord{}, skipRecordUnreadable, err
	}
	if rec.Version != launchRecordVersion || rec.PID <= 0 || rec.BootID == "" {
		return launchRecord{}, skipRecordUnreadable, fmt.Errorf("launch record has an unsupported shape (version %d)", rec.Version)
	}
	if rec.SandboxID != sandboxID {
		return launchRecord{}, skipRecordForeign, fmt.Errorf("launch record names sandbox %q, its directory is of %q", rec.SandboxID, sandboxID)
	}
	return rec, "", nil
}

// ownedProcess is a firecracker process that this agent started and that
// still runs.
type ownedProcess struct {
	sandboxID string
	pid       int
	handle    procHandle
}

// cmdlineHasID reports whether the command line carries the --id argument
// of the sandbox.
func cmdlineHasID(args []string, sandboxID string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--id" && args[i+1] == idPrefix+sandboxID {
			return true
		}
	}
	return false
}

// verifyLaunch is the one check that decides whether the agent may signal a
// process of a sandbox it did not track in memory. It returns the process
// when the launch record in the VM directory matches a live process, and
// otherwise why not. The caller closes the returned handle.
func verifyLaunch(host procHost, vmDir, sandboxID string) (*ownedProcess, launchSkipReason, error) {
	rec, reason, err := readLaunchRecord(vmDir, sandboxID)
	if reason != "" {
		return nil, reason, err
	}
	boot, err := host.bootID()
	if err != nil {
		return nil, skipRecordUnreadable, verifyIOError(fmt.Errorf("read boot ID: %w", err))
	}
	if boot != rec.BootID {
		return nil, skipBootChanged, nil
	}
	matches := func() (launchSkipReason, error) {
		start, err := host.startTime(rec.PID)
		if errors.Is(err, errProcessGone) {
			return skipProcessGone, nil
		}
		if err != nil {
			return skipProcessChanged, verifyIOError(err)
		}
		if start != rec.StartTime {
			return skipProcessChanged, nil
		}
		args, err := host.cmdline(rec.PID)
		if errors.Is(err, errProcessGone) {
			return skipProcessGone, nil
		}
		if err != nil {
			return skipProcessChanged, verifyIOError(err)
		}
		if !cmdlineHasID(args, sandboxID) {
			return skipProcessChanged, nil
		}
		return "", nil
	}
	if reason, err := matches(); reason != "" {
		return nil, reason, err
	}
	handle, err := host.open(rec.PID)
	if errors.Is(err, errProcessGone) {
		return nil, skipProcessGone, nil
	}
	if err != nil {
		return nil, skipProcessChanged, verifyIOError(fmt.Errorf("open process handle: %w", err))
	}
	// The PID may have been reused between the check and the open: the
	// handle then refers to the new process. Check again, now that the
	// handle pins the process the PID names.
	if reason, err := matches(); reason != "" {
		handle.close()
		return nil, reason, err
	}
	return &ownedProcess{sandboxID: sandboxID, pid: rec.PID, handle: handle}, "", nil
}
