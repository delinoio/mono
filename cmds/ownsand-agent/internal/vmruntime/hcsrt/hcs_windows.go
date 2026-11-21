// SPDX-License-Identifier: Apache-2.0

package hcsrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file binds the Host Compute Service API of computecore.dll
// (https://learn.microsoft.com/en-us/virtualization/api/hcs/overview).
// github.com/Microsoft/hcsshim keeps its schema-v2 types and its HCS calls
// in internal packages, and its public API creates schema-v1 containers
// only, so the runtime calls the documented C API directly.

var (
	modComputeCore = windows.NewLazySystemDLL("computecore.dll")

	procHcsCreateOperation            = modComputeCore.NewProc("HcsCreateOperation")
	procHcsCloseOperation             = modComputeCore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult     = modComputeCore.NewProc("HcsWaitForOperationResult")
	procHcsCreateComputeSystem        = modComputeCore.NewProc("HcsCreateComputeSystem")
	procHcsOpenComputeSystem          = modComputeCore.NewProc("HcsOpenComputeSystem")
	procHcsCloseComputeSystem         = modComputeCore.NewProc("HcsCloseComputeSystem")
	procHcsStartComputeSystem         = modComputeCore.NewProc("HcsStartComputeSystem")
	procHcsPauseComputeSystem         = modComputeCore.NewProc("HcsPauseComputeSystem")
	procHcsResumeComputeSystem        = modComputeCore.NewProc("HcsResumeComputeSystem")
	procHcsTerminateComputeSystem     = modComputeCore.NewProc("HcsTerminateComputeSystem")
	procHcsEnumerateComputeSystems    = modComputeCore.NewProc("HcsEnumerateComputeSystems")
	procHcsGetComputeSystemProperties = modComputeCore.NewProc("HcsGetComputeSystemProperties")
	procHcsSetComputeSystemCallback   = modComputeCore.NewProc("HcsSetComputeSystemCallback")
)

// HCS_EVENT_TYPE values (ComputeDefs.h).
const (
	hcsEventSystemExited      = 0x00000001
	hcsEventServiceDisconnect = 0x02000000
)

// HRESULTs of the HCS (hcsshim internal/hcs/errors.go names them).
const (
	hrSystemNotFound       = 0xc037010e
	hrSystemAlreadyStopped = 0x80370110
	hrAlreadyStopped       = 0xc0370110
)

// hcsOperationTimeout bounds one HCS operation.
const hcsOperationTimeout = 2 * time.Minute

// hcsError is a failed HCS call with its HRESULT and the error document of
// the operation result, if any.
type hcsError struct {
	op     string
	hr     uint32
	result string
}

func (e *hcsError) Error() string {
	msg := fmt.Sprintf("hcsrt: %s: HRESULT %#08x (%v)", e.op, e.hr, windows.Errno(e.hr))
	if e.result != "" {
		var doc struct {
			Error        int32
			ErrorMessage string
		}
		if json.Unmarshal([]byte(e.result), &doc) == nil && doc.ErrorMessage != "" {
			return msg + ": " + doc.ErrorMessage
		}
		return msg + ": " + e.result
	}
	return msg
}

// isNotFoundOrStopped reports an error of a call on a compute system that
// is gone or stopped already.
func isNotFoundOrStopped(err error) bool {
	var he *hcsError
	if !errors.As(err, &he) {
		return false
	}
	switch he.hr {
	case hrSystemNotFound, hrSystemAlreadyStopped, hrAlreadyStopped:
		return true
	}
	return false
}

func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

// takeString converts and frees a result document that the HCS allocated.
func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(p)))
	return s
}

// utf16 converts s for a call; an empty string is a NULL pointer.
func utf16(s string) (*uint16, error) {
	if s == "" {
		return nil, nil
	}
	return windows.UTF16PtrFromString(s)
}

// hcsLoad reports whether computecore.dll and its functions exist.
func hcsLoad() error {
	if err := modComputeCore.Load(); err != nil {
		return err
	}
	return procHcsSetComputeSystemCallback.Find()
}

// hcsRun runs one asynchronous HCS call: call starts it on a new operation
// and hcsRun waits for its result document.
func hcsRun(name string, call func(op uintptr) uintptr) (string, error) {
	op, _, e := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("hcsrt: HcsCreateOperation: %w", e)
	}
	defer procHcsCloseOperation.Call(op)
	if hr := call(op); failed(hr) {
		return "", &hcsError{op: name, hr: uint32(hr)}
	}
	var result *uint16
	hr, _, _ := procHcsWaitForOperationResult.Call(op, uintptr(hcsOperationTimeout/time.Millisecond), uintptr(unsafe.Pointer(&result)))
	doc := takeString(result)
	if failed(hr) {
		return "", &hcsError{op: name, hr: uint32(hr), result: doc}
	}
	return doc, nil
}

// system is an open compute system handle.
type system struct {
	id     string
	handle uintptr
	// exited is closed when the HCS reports that the system exited or
	// that the HCS service disconnected.
	exited   chan struct{}
	exitOnce sync.Once
	cbKey    uintptr
}

// Exit callbacks. windows.NewCallback never releases a callback, so one
// callback serves every system and finds it by its context key.
var (
	callbackOnce sync.Once
	callbackPtr  uintptr
	callbackMu   sync.Mutex
	callbackSys  = map[uintptr]*system{}
	callbackNext uintptr
)

// hcsEvent is HCS_EVENT.
type hcsEvent struct {
	Type      uint32
	EventData *uint16
	Operation uintptr
}

func systemCallback(event *hcsEvent, key uintptr) uintptr {
	if event == nil {
		return 0
	}
	if event.Type != hcsEventSystemExited && event.Type != hcsEventServiceDisconnect {
		return 0
	}
	callbackMu.Lock()
	s := callbackSys[key]
	callbackMu.Unlock()
	if s != nil {
		s.markExited()
	}
	return 0
}

func (s *system) markExited() { s.exitOnce.Do(func() { close(s.exited) }) }

// watch registers the exit callback of s.
func (s *system) watch() error {
	callbackOnce.Do(func() { callbackPtr = windows.NewCallback(systemCallback) })
	callbackMu.Lock()
	callbackNext++
	s.cbKey = callbackNext
	callbackSys[s.cbKey] = s
	callbackMu.Unlock()
	// HcsEventOptionNone (0).
	hr, _, _ := procHcsSetComputeSystemCallback.Call(s.handle, 0, s.cbKey, callbackPtr)
	if failed(hr) {
		callbackMu.Lock()
		delete(callbackSys, s.cbKey)
		callbackMu.Unlock()
		return &hcsError{op: "HcsSetComputeSystemCallback", hr: uint32(hr)}
	}
	return nil
}

func newSystem(id string, handle uintptr) *system {
	return &system{id: id, handle: handle, exited: make(chan struct{})}
}

// createSystem creates (but does not start) a compute system from doc.
func createSystem(id string, doc any) (*system, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	idp, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	cfg, err := windows.UTF16PtrFromString(string(b))
	if err != nil {
		return nil, err
	}
	var handle uintptr
	if _, err := hcsRun("HcsCreateComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateComputeSystem.Call(uintptr(unsafe.Pointer(idp)), uintptr(unsafe.Pointer(cfg)), op, 0, uintptr(unsafe.Pointer(&handle)))
		return hr
	}); err != nil {
		if handle != 0 {
			procHcsCloseComputeSystem.Call(handle)
		}
		return nil, err
	}
	s := newSystem(id, handle)
	if err := s.watch(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// openSystem opens an existing compute system with full access.
func openSystem(id string) (*system, error) {
	idp, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	var handle uintptr
	hr, _, _ := procHcsOpenComputeSystem.Call(uintptr(unsafe.Pointer(idp)), windows.GENERIC_ALL, uintptr(unsafe.Pointer(&handle)))
	if failed(hr) {
		return nil, &hcsError{op: "HcsOpenComputeSystem", hr: uint32(hr)}
	}
	s := newSystem(id, handle)
	if err := s.watch(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// call runs an HCS operation on s with an optional options document.
func (s *system) call(name string, proc *windows.LazyProc, options string) (string, error) {
	opts, err := utf16(options)
	if err != nil {
		return "", err
	}
	return hcsRun(name, func(op uintptr) uintptr {
		hr, _, _ := proc.Call(s.handle, op, uintptr(unsafe.Pointer(opts)))
		return hr
	})
}

func (s *system) start() error {
	_, err := s.call("HcsStartComputeSystem", procHcsStartComputeSystem, "")
	return err
}

func (s *system) pause() error {
	_, err := s.call("HcsPauseComputeSystem", procHcsPauseComputeSystem, "")
	return err
}

func (s *system) resume() error {
	_, err := s.call("HcsResumeComputeSystem", procHcsResumeComputeSystem, "")
	return err
}

// terminate stops s at once; a system that is stopped already is no error.
func (s *system) terminate() error {
	_, err := s.call("HcsTerminateComputeSystem", procHcsTerminateComputeSystem, "")
	if err != nil && isNotFoundOrStopped(err) {
		return nil
	}
	return err
}

// systemProperties is the part of the compute system properties that the
// runtime reads.
type systemProperties struct {
	ID        string `json:"Id"`
	Owner     string `json:"Owner"`
	RuntimeID string `json:"RuntimeId"`
	State     string `json:"State"`
}

func (s *system) properties() (systemProperties, error) {
	doc, err := s.call("HcsGetComputeSystemProperties", procHcsGetComputeSystemProperties, "")
	if err != nil {
		return systemProperties{}, err
	}
	var p systemProperties
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return systemProperties{}, fmt.Errorf("hcsrt: compute system properties: %w", err)
	}
	return p, nil
}

// close closes the handle of s. With ShouldTerminateOnLastHandleClosed the
// HCS terminates the system when this is its last handle.
func (s *system) close() {
	if s.handle != 0 {
		procHcsCloseComputeSystem.Call(s.handle)
		s.handle = 0
	}
	callbackMu.Lock()
	delete(callbackSys, s.cbKey)
	callbackMu.Unlock()
}

// enumerateSystems lists the compute systems of owner.
func enumerateSystems(owner string) ([]systemProperties, error) {
	q, err := json.Marshal(map[string][]string{"Owners": {owner}})
	if err != nil {
		return nil, err
	}
	qp, err := windows.UTF16PtrFromString(string(q))
	if err != nil {
		return nil, err
	}
	doc, err := hcsRun("HcsEnumerateComputeSystems", func(op uintptr) uintptr {
		hr, _, _ := procHcsEnumerateComputeSystems.Call(uintptr(unsafe.Pointer(qp)), op)
		return hr
	})
	if err != nil {
		return nil, err
	}
	var list []systemProperties
	if doc == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(doc), &list); err != nil {
		return nil, fmt.Errorf("hcsrt: compute system list: %w", err)
	}
	return list, nil
}
