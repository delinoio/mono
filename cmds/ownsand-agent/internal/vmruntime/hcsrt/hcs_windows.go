// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// HRESULTs of the HCS. computecore.dll returns the HCS_E_* values of
// winerror.h (0x8037....); the 0xc037.... forms are the vmcompute.dll
// values that hcsshim internal/hcs/errors.go names, kept in case a Windows
// build returns them.
const (
	hrSystemNotFound          = 0x8037010e // HCS_E_SYSTEM_NOT_FOUND
	hrSystemAlreadyStopped    = 0x80370110 // HCS_E_SYSTEM_ALREADY_STOPPED
	hrVmcomputeNotFound       = 0xc037010e
	hrVmcomputeAlreadyStopped = 0xc0370110
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
	case hrSystemNotFound, hrSystemAlreadyStopped, hrVmcomputeNotFound, hrVmcomputeAlreadyStopped:
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
	return hcsRunWithin(name, time.Time{}, call)
}

func hcsRunWithin(name string, deadline time.Time, call func(op uintptr) uintptr) (string, error) {
	op, _, e := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("hcsrt: HcsCreateOperation: %w", e)
	}
	defer procHcsCloseOperation.Call(op)
	if hr := call(op); failed(hr) {
		return "", &hcsError{op: name, hr: uint32(hr)}
	}
	var result *uint16
	timeout := hcsOperationTimeout
	if !deadline.IsZero() {
		timeout = min(timeout, time.Until(deadline))
		if timeout <= 0 {
			return "", context.DeadlineExceeded
		}
	}
	hr, _, _ := procHcsWaitForOperationResult.Call(op, uintptr(timeout/time.Millisecond), uintptr(unsafe.Pointer(&result)))
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
	// Only confirmed exit closes exited. A disconnect calls for a status
	// check, and closed signals completion of handle and callback cleanup.
	exited         chan struct{}
	disconnected   chan struct{}
	closed         chan struct{}
	exitOnce       sync.Once
	disconnectOnce sync.Once
	closeOnce      sync.Once
	cbKey          uintptr
	// These functions replace HCS calls in tests without a Hyper-V host.
	terminateFn  func() error
	verifyExitFn func(string, time.Time) (bool, error)
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
	defer callbackMu.Unlock()
	s := callbackSys[key]
	if s != nil {
		switch event.Type {
		case hcsEventSystemExited:
			s.markExited()
		case hcsEventServiceDisconnect:
			s.disconnectOnce.Do(func() { close(s.disconnected) })
		}
	}
	return 0
}

func (s *system) markExited() { s.exitOnce.Do(func() { close(s.exited) }) }

func (s *system) exitConfirmed() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

var errSystemHandleClosed = errors.New("hcsrt: the compute system handle closed before exit was confirmed")

// waitExit waits for a confirmed exit. A disconnect starts one independent
// status query per wait. An inconclusive result leaves the exit event armed.
func (s *system) waitExit(ctx context.Context) error {
	type exitResult struct {
		confirmed bool
		err       error
	}
	disconnected := s.disconnected
	var result <-chan exitResult
	var statusErr error
	for !s.exitConfirmed() {
		select {
		case <-s.exited:
			return nil
		case <-s.closed:
			// Closing the handle can deliver a final exit callback.
			if s.exitConfirmed() {
				return nil
			}
			return errSystemHandleClosed
		case <-ctx.Done():
			if s.exitConfirmed() {
				return nil
			}
			return errors.Join(ctx.Err(), statusErr)
		case <-disconnected:
			disconnected = nil
			id, verify := s.id, s.verifyExitFn
			deadline := time.Now().Add(stopTimeout)
			if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
				deadline = end
			}
			// The HCS call can finish after this waiter. It holds no
			// reference to the VM or its original handle.
			done := make(chan exitResult, 1)
			result = done
			go func() {
				confirmed, err := verify(id, deadline)
				done <- exitResult{confirmed, err}
			}()
		case status := <-result:
			result = nil
			if status.confirmed {
				s.markExited()
				return nil
			}
			statusErr = status.err
		}
	}
	return nil
}

// watch registers the system callback.
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
	return &system{
		id: id, handle: handle, exited: make(chan struct{}), disconnected: make(chan struct{}),
		closed: make(chan struct{}), verifyExitFn: verifySystemExit,
	}
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
	var err error
	if s.terminateFn != nil {
		err = s.terminateFn()
	} else {
		_, err = s.call("HcsTerminateComputeSystem", procHcsTerminateComputeSystem, "")
	}
	if err != nil && isNotFoundOrStopped(err) {
		s.markExited()
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
	Stopped   bool   `json:"Stopped"`
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
	s.closeOnce.Do(func() {
		if s.handle != 0 {
			procHcsCloseComputeSystem.Call(s.handle)
		}
		callbackMu.Lock()
		delete(callbackSys, s.cbKey)
		callbackMu.Unlock()
		close(s.closed)
	})
}

// verifySystemExit queries HCS by ID without using the disconnected handle.
func verifySystemExit(id string, deadline time.Time) (bool, error) {
	list, err := querySystems(map[string][]string{"Ids": {id}}, deadline)
	if isNotFoundOrStopped(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return confirmedSystemExit(id, list)
}

func confirmedSystemExit(id string, list []systemProperties) (bool, error) {
	switch {
	case list == nil:
		return false, errors.New("hcsrt: status query returned no compute system list")
	case len(list) == 0:
		return true, nil
	case len(list) == 1 && strings.EqualFold(list[0].ID, id):
		const stoppedState = "Stopped"
		return list[0].State == stoppedState || (list[0].State == "" && list[0].Stopped), nil
	default:
		return false, errors.New("hcsrt: status query returned unexpected compute system IDs")
	}
}

// enumerateSystems lists the compute systems of owner.
func enumerateSystems(owner string) ([]systemProperties, error) {
	return querySystems(map[string][]string{"Owners": {owner}}, time.Time{})
}

func querySystems(query map[string][]string, deadline time.Time) ([]systemProperties, error) {
	q, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	qp, err := windows.UTF16PtrFromString(string(q))
	if err != nil {
		return nil, err
	}
	doc, err := hcsRunWithin("HcsEnumerateComputeSystems", deadline, func(op uintptr) uintptr {
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
