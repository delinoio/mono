// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

const startupDiagnosticEventID = 1

func (h *serviceHandler) openServiceLog(stateDir string) (logOutput, error) {
	out, err := openLogFile(stateDir, logRotateBytes)
	if err != nil {
		_ = h.reportStartupError(serviceLogFailure(err))
	}
	return out, err
}

func serviceLogFailure(err error) string {
	operation := "could not set up " + logFile
	var setupErr *logSetupError
	if errors.As(err, &setupErr) {
		operation = string(setupErr.operation)
	}
	// A filesystem error can contain the configured state path. Include only
	// the operation and the system-supplied description of the error code.
	var code windows.Errno
	if !errors.As(err, &code) {
		return operation
	}
	// Go's Windows open translates ERROR_ACCESS_DENIED to EISDIR when
	// the target is a directory. Recover the Win32 code for the event.
	if code == syscall.EISDIR {
		code = windows.ERROR_ACCESS_DENIED
	}
	if code&syscall.APPLICATION_ERROR != 0 {
		return fmt.Sprintf("%s: %v", operation, code)
	}
	return fmt.Sprintf("%s (Windows error %d: %v)", operation, code, code)
}

func writeStartupEvent(message string) error {
	// An existing guest service may have no registered event source. Try
	// writing even if registration fails, since the source may already exist.
	_ = ensureStartupEventSource()
	log, err := eventlog.Open(serviceName)
	if err != nil {
		return err
	}
	defer log.Close()
	return log.Error(startupDiagnosticEventID, message)
}

func ensureStartupEventSource() error {
	const sourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + serviceName
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, sourceKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	// Use the system message resource for insertion strings, as x/sys does in
	// InstallAsEventCreate. Set all values to repair a partial registration.
	if err := key.SetExpandStringValue("EventMessageFile", `%SystemRoot%\System32\EventCreate.exe`); err != nil {
		return err
	}
	if err := key.SetDWordValue("CustomSource", 1); err != nil {
		return err
	}
	return key.SetDWordValue("TypesSupported", eventlog.Error)
}
