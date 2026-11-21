// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package processrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const supported = true

var (
	sigStop = syscall.SIGSTOP
	sigCont = syscall.SIGCONT
	sigKill = syscall.SIGKILL
)

var hostGuestOS, hostBaseImage = func() (ownsandv1.GuestOs, ownsandv1.BaseImage) {
	if runtime.GOOS == "darwin" {
		return ownsandv1.GuestOs_GUEST_OS_MACOS, ownsandv1.BaseImage_BASE_IMAGE_MACOS
	}
	return ownsandv1.GuestOs_GUEST_OS_LINUX, ownsandv1.BaseImage_BASE_IMAGE_LINUX
}()

// psTimeout bounds one process listing.
const psTimeout = 5 * time.Second

// signalGroupID sends sig to every process of the process group pgid. A
// group that no longer exists is not an error: its workers ended on their
// own between the listing and the signal.
func signalGroupID(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// descendantGroups returns the distinct process groups of the processes
// below rootPID in the parent-process tree, without the group of rootPID,
// the agent's own group and the groups 0 and 1. The result is sorted.
//
// `ps -A -o pid=,ppid=,pgid=` lists the same columns on Linux (procps) and
// macOS.
func descendantGroups(ctx context.Context, rootPID int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs, err := parseProcessList(out)
	if err != nil {
		return nil, err
	}
	// The helper leads its own process group (procgroup.Set), so rootPID is
	// also the group of the helper.
	exclude := map[int]bool{0: true, 1: true, rootPID: true}
	if pgid, err := syscall.Getpgid(os.Getpid()); err == nil {
		exclude[pgid] = true
	}
	return groupsBelow(procs, rootPID, exclude), nil
}

// process is one row of the process listing.
type process struct{ pid, ppid, pgid int }

// parseProcessList parses the output of `ps -A -o pid=,ppid=,pgid=`: three
// integer columns per line. A malformed line is an error, so that a changed
// output format never silently yields an empty tree.
func parseProcessList(out []byte) ([]process, error) {
	var procs []process
	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected process list line with %d columns", len(fields))
		}
		var cols [3]int
		for i, f := range fields {
			n, err := strconv.Atoi(string(f))
			if err != nil {
				return nil, fmt.Errorf("unexpected process list column: %w", err)
			}
			cols[i] = n
		}
		procs = append(procs, process{pid: cols[0], ppid: cols[1], pgid: cols[2]})
	}
	return procs, nil
}

// groupsBelow walks the parent links from rootPID and returns the sorted,
// distinct process groups of its descendants that are not in exclude.
func groupsBelow(procs []process, rootPID int, exclude map[int]bool) []int {
	children := map[int][]process{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	seen := map[int]bool{rootPID: true}
	groups := map[int]bool{}
	queue := []int{rootPID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if seen[c.pid] {
				continue
			}
			seen[c.pid] = true
			queue = append(queue, c.pid)
			if !exclude[c.pgid] {
				groups[c.pgid] = true
			}
		}
	}
	out := make([]int, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	sort.Ints(out)
	return out
}
