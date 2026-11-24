// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package imagebuild

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fblocks
#cgo LDFLAGS: -framework Foundation -framework Virtualization
#include <stdlib.h>
#include "macos_install.h"
*/
import "C"

import (
	"context"
	"errors"
	"os"
	"runtime/cgo"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
)

type nativeMacOSInstallation struct {
	ptr    unsafe.Pointer
	handle cgo.Handle
	done   chan error
}

type macOSInstallCallbacks struct {
	ctx  context.Context
	done chan<- error
}

// Own the install VM and its queue because vz.MacOSInstaller.Install can
// return a context error without saying whether it registered its callback.
func startNativeMacOSInstall(ctx context.Context, m vzrt.MacOSMachine, ipsw string) (macOSInstallation, error) {
	if _, err := os.Stat(ipsw); err != nil {
		return nil, err
	}
	disk, aux, restore := C.CString(m.Disk), C.CString(m.AuxStorage), C.CString(ipsw)
	defer C.free(unsafe.Pointer(disk))
	defer C.free(unsafe.Pointer(aux))
	defer C.free(unsafe.Pointer(restore))

	done := make(chan error, 1)
	handle := cgo.NewHandle(macOSInstallCallbacks{ctx: ctx, done: done})
	var nativeError *C.char
	ptr := C.ownsandMacOSInstallStart(disk, aux, restore, unsafe.Pointer(unsafe.SliceData(m.HardwareModel)),
		C.size_t(len(m.HardwareModel)), C.size_t(m.CPUs), C.uint64_t(m.MemoryMiB)<<20,
		C.uintptr_t(handle), &nativeError)
	if ptr == nil {
		handle.Delete()
		if nativeError != nil {
			defer C.free(unsafe.Pointer(nativeError))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if nativeError != nil {
			return nil, errors.New(C.GoString(nativeError))
		}
		return nil, errors.New("could not start the native macOS installer")
	}
	return &nativeMacOSInstallation{ptr: ptr, handle: handle, done: done}, nil
}

func (i *nativeMacOSInstallation) completion() <-chan error { return i.done }

func (i *nativeMacOSInstallation) cancel() { C.ownsandMacOSInstallCancel(i.ptr) }

func (i *nativeMacOSInstallation) fractionCompleted() float64 {
	return float64(C.ownsandMacOSInstallFractionCompleted(i.ptr))
}

func (i *nativeMacOSInstallation) close(stopVM bool) {
	C.ownsandMacOSInstallClose(i.ptr, C.bool(stopVM))
	i.handle.Delete()
}

//export ownsandMacOSInstallCanStart
func ownsandMacOSInstallCanStart(handle C.uintptr_t) C.bool {
	callbacks := cgo.Handle(handle).Value().(macOSInstallCallbacks)
	return C.bool(callbacks.ctx.Err() == nil)
}

//export ownsandMacOSInstallCompleted
func ownsandMacOSInstallCompleted(handle C.uintptr_t, domain *C.char, code C.int64_t, description, userInfo *C.char) {
	callbacks := cgo.Handle(handle).Value().(macOSInstallCallbacks)
	if domain == nil {
		callbacks.done <- nil
		return
	}
	callbacks.done <- &vz.NSError{
		Domain:               C.GoString(domain),
		Code:                 int(code),
		LocalizedDescription: C.GoString(description),
		UserInfo:             C.GoString(userInfo),
	}
}
