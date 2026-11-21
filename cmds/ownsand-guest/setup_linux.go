// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ext4IocResizeFS is EXT4_IOC_RESIZE_FS (_IOW('f', 16, __u64)).
const ext4IocResizeFS = 0x40086610

// ext4SuperMagic is the statfs f_type of ext4.
const ext4SuperMagic = 0xEF53

// growRoot grows a mounted ext4 root file system online to the size of its
// block device. The runtimes enlarge a sandbox disk image to the requested
// disk size without resizing its file system (macOS and Windows hosts have
// no e2fsprogs), so the guest grows it at boot. It changes nothing when the
// file system already fills the device.
func growRoot() error {
	var fs unix.Statfs_t
	if err := unix.Statfs("/", &fs); err != nil {
		return err
	}
	if fs.Type != ext4SuperMagic {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Stat("/", &st); err != nil {
		return err
	}
	raw, err := os.ReadFile(fmt.Sprintf("/sys/dev/block/%d:%d/size", unix.Major(st.Dev), unix.Minor(st.Dev)))
	if err != nil {
		return err
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return err
	}
	blocks := sectors * 512 / uint64(fs.Bsize)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// The kernel returns at once when the file system already has this
	// size.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ext4IocResizeFS, uintptr(unsafe.Pointer(&blocks))); errno != 0 {
		return errno
	}
	return nil
}

// setFlagsUp sets IFF_UP on the interface name.
func setFlagsUp(sock int, name string) error {
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr)
}

// upLoopback brings the loopback interface up, which no other component of
// the minimal base image does.
func upLoopback() error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sock)
	return setFlagsUp(sock, "lo")
}
