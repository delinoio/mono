// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// envInitChild marks the guest helper process that the init process
// started.
const envInitChild = "OWNSAND_GUEST_INIT_CHILD"

// isInit reports whether the helper runs as PID 1 of a Linux guest (the
// Linux base image boots it as /sbin/init).
func isInit() bool {
	return os.Getpid() == 1 && os.Getenv(envInitChild) == ""
}

// isInitChild reports whether the helper is the child that the init process
// started.
func isInitChild() bool {
	return os.Getenv(envInitChild) != ""
}

// runInit is the init process of the Linux base image: it mounts the
// kernel file systems, grows the root file system to its disk, brings up
// the loopback interface, writes /etc/resolv.conf from the kernel's IP
// autoconfiguration, starts the guest helper as its child (restarting it
// when it exits) and reaps orphaned processes. The helper runs as a child
// so that the reaper never takes the exit status of an exec.
func runInit(args []string) int {
	for _, m := range []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "gid=5,mode=0620"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
	} {
		_ = os.MkdirAll(m.target, 0o755)
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil && !errors.Is(err, unix.EBUSY) {
			fmt.Fprintf(os.Stderr, "ownsand-guest init: mount %s: %v\n", m.target, err)
		}
	}
	_ = unix.Sethostname([]byte("sandbox"))
	if err := growRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "ownsand-guest init: grow the root file system: %v\n", err)
	}
	if err := upLoopback(); err != nil {
		fmt.Fprintf(os.Stderr, "ownsand-guest init: bring up lo: %v\n", err)
	}
	if pnp, err := os.ReadFile("/proc/net/pnp"); err == nil {
		var lines []string
		for _, l := range strings.Split(string(pnp), "\n") {
			if strings.HasPrefix(l, "nameserver ") {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			_ = os.WriteFile("/etc/resolv.conf", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		}
	}
	if os.Getenv("PATH") == "" {
		os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if os.Getenv("HOME") == "" {
		os.Setenv("HOME", "/root")
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "/proc/self/exe"
	}
	for {
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), envInitChild+"=1")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "ownsand-guest init: start helper: %v\n", err)
			time.Sleep(time.Second)
			continue
		}
		helper := cmd.Process.Pid
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, 0, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil || pid == helper {
				break
			}
		}
		fmt.Fprintln(os.Stderr, "ownsand-guest init: helper exited; restarting")
		time.Sleep(time.Second)
	}
}
