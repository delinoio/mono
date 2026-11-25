// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: publish-lock <lock-path> (with the lock file open on stdin)")
		os.Exit(2)
	}
	path := os.Args[1]
	if err := lockPublication(path); err != nil {
		fmt.Fprintf(os.Stderr, "could not acquire image publication lock: %s: %v\n", path, err)
		os.Exit(1)
	}
}

func lockPublication(path string) error {
	opened, err := os.Stdin.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("lock path does not name the opened regular file")
	}

	for {
		err := syscall.Flock(int(os.Stdin.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errors.New("another publisher holds the lock")
		default:
			return err
		}
	}
}
