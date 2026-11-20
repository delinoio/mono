// Package hostinfo reads the host facts of the capability report.
package hostinfo

import (
	goruntime "runtime"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// HostOS returns the host OS enum.
func HostOS() ownsandv1.HostOs {
	switch goruntime.GOOS {
	case "linux":
		return ownsandv1.HostOs_HOST_OS_LINUX
	case "darwin":
		return ownsandv1.HostOs_HOST_OS_MACOS
	case "windows":
		return ownsandv1.HostOs_HOST_OS_WINDOWS
	default:
		return ownsandv1.HostOs_HOST_OS_UNSPECIFIED
	}
}

// Arch returns the host architecture enum.
func Arch() ownsandv1.Arch {
	switch goruntime.GOARCH {
	case "amd64":
		return ownsandv1.Arch_ARCH_AMD64
	case "arm64":
		return ownsandv1.Arch_ARCH_ARM64
	default:
		return ownsandv1.Arch_ARCH_UNSPECIFIED
	}
}

// Platform returns "<goos>-<goarch>", the release artifact key.
func Platform() string { return goruntime.GOOS + "-" + goruntime.GOARCH }

// LogicalCPUs returns the logical CPU count.
func LogicalCPUs() int64 { return int64(goruntime.NumCPU()) }
