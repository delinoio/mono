// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestValidateResources(t *testing.T) {
	// The oversized values are built at run time: the constants of the
	// limits are 64-bit and must not be converted to a 32-bit int in source.
	tests := []struct {
		name                  string
		diskGiB, cpus, memMiB int64
		wantFlag              string // empty: accepted
	}{
		{name: "defaults"},
		{name: "supported", diskGiB: 64, cpus: 4, memMiB: 8192},
		{name: "largest disk", diskGiB: MaxDiskGiB},
		{name: "largest cpus", cpus: math.MaxInt32},
		{name: "largest memory", memMiB: MaxMemoryMiB},
		{name: "disk one over", diskGiB: MaxDiskGiB + 1, wantFlag: "--disk-gib"},
		{name: "disk wraps to zero", diskGiB: 1 << 34, wantFlag: "--disk-gib"},
		{name: "disk wraps to 64 GiB", diskGiB: 1<<34 + 64, wantFlag: "--disk-gib"},
		{name: "disk max int64", diskGiB: math.MaxInt64, wantFlag: "--disk-gib"},
		{name: "cpus one over", cpus: math.MaxInt32 + 1, wantFlag: "--cpus"},
		{name: "cpus wraps to zero", cpus: 1 << 32, wantFlag: "--cpus"},
		{name: "cpus wraps to four", cpus: 1<<32 + 4, wantFlag: "--cpus"},
		{name: "memory one over", memMiB: MaxMemoryMiB + 1, wantFlag: "--memory-mib"},
		{name: "memory wraps to zero", memMiB: 1 << 44, wantFlag: "--memory-mib"},
		{name: "memory max int64", memMiB: math.MaxInt64, wantFlag: "--memory-mib"},
		{name: "negative disk", diskGiB: -1, wantFlag: "must not be negative"},
		{name: "negative cpus", cpus: -1, wantFlag: "must not be negative"},
		{name: "negative memory", memMiB: -1, wantFlag: "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResources(tc.diskGiB, tc.cpus, tc.memMiB)
			if tc.wantFlag == "" {
				if err != nil {
					t.Fatalf("ValidateResources: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantFlag) {
				t.Fatalf("ValidateResources error = %v, want %q", err, tc.wantFlag)
			}
		})
	}
	if got, want := strconv.FormatInt(MaxDiskGiB, 10), "8589934591"; got != want {
		t.Fatalf("MaxDiskGiB = %s, want %s", got, want)
	}
	if got, want := MaxDiskGiB<<30, int64(math.MaxInt64)&^(1<<30-1); got != want {
		t.Fatalf("MaxDiskGiB<<30 = %d, want %d", got, want)
	}
	if got := MaxMemoryMiB << 20; got <= 0 {
		t.Fatalf("MaxMemoryMiB<<20 = %d overflows", got)
	}
}

// TestBuildRejectsOversizedResources checks that the programmatic entry point
// rejects oversized disk and memory values before creating image state. The
// vCPU count is an int32 there and cannot exceed its limit.
func TestBuildRejectsOversizedResources(t *testing.T) {
	guests := []struct {
		name string
		os   ownsandv1.GuestOs
	}{
		{name: "macos", os: ownsandv1.GuestOs_GUEST_OS_MACOS},
		{name: "windows", os: ownsandv1.GuestOs_GUEST_OS_WINDOWS},
	}
	resources := []struct {
		flag string
		opts Options
	}{
		{flag: "--disk-gib", opts: Options{DiskGiB: MaxDiskGiB + 1}},
		{flag: "--memory-mib", opts: Options{MemoryMiB: MaxMemoryMiB + 1}},
	}
	for _, guest := range guests {
		for _, resource := range resources {
			checkBuildError := func(t *testing.T, imagesDir string) {
				t.Helper()
				opts := resource.opts
				opts.ImagesDir = imagesDir
				opts.GuestOS = guest.os
				err := Build(context.Background(), opts)
				if err == nil || !strings.Contains(err.Error(), resource.flag) {
					t.Errorf("Build error = %v, want %q", err, resource.flag)
				}
			}
			t.Run(guest.name+"/"+resource.flag+"/missing images directory", func(t *testing.T) {
				imagesDir := filepath.Join(t.TempDir(), "images")
				checkBuildError(t, imagesDir)
				if _, err := os.Lstat(imagesDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Lstat(%q) = %v, want path not found", imagesDir, err)
				}
			})
			t.Run(guest.name+"/"+resource.flag+"/empty images directory", func(t *testing.T) {
				imagesDir := filepath.Join(t.TempDir(), "images")
				if err := os.Mkdir(imagesDir, 0o700); err != nil {
					t.Fatal(err)
				}
				checkBuildError(t, imagesDir)
				lockPath := filepath.Join(imagesDir, guest.name+".build.lock")
				if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Lstat(%q) = %v, want path not found", lockPath, err)
				}
				entries, err := os.ReadDir(imagesDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("images directory entries = %v, want empty", entries)
				}
			})
		}
	}
}
