// SPDX-License-Identifier: Apache-2.0

// Package sysinfo reads the host facts of the capability report: host OS,
// architecture, logical CPUs, memory and free disk.
package sysinfo

import (
	"runtime"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// HostOS returns the HostOs of the running agent.
func HostOS() ownsandv1.HostOs {
	switch runtime.GOOS {
	case "linux":
		return ownsandv1.HostOs_HOST_OS_LINUX
	case "darwin":
		return ownsandv1.HostOs_HOST_OS_MACOS
	case "windows":
		return ownsandv1.HostOs_HOST_OS_WINDOWS
	}
	return ownsandv1.HostOs_HOST_OS_UNSPECIFIED
}

// Arch returns the Arch of the running agent.
func Arch() ownsandv1.Arch {
	switch runtime.GOARCH {
	case "amd64":
		return ownsandv1.Arch_ARCH_AMD64
	case "arm64":
		return ownsandv1.Arch_ARCH_ARM64
	}
	return ownsandv1.Arch_ARCH_UNSPECIFIED
}

// Platform returns the release platform name, for example "linux-amd64".
func Platform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// LogicalCPUs returns the number of logical CPUs.
func LogicalCPUs() int32 {
	return int32(runtime.NumCPU())
}

// MemoryMiB returns the physical memory in MiB, or 0 when unknown.
func MemoryMiB() int64 {
	b, err := memoryBytes()
	if err != nil {
		return 0
	}
	return int64(b >> 20)
}

// FreeDiskBytes returns the free bytes of the file system of dir that the
// agent can use.
func FreeDiskBytes(dir string) (int64, error) {
	return freeDiskBytes(dir)
}
