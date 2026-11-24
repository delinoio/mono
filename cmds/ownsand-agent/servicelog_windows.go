// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
)

func setupServiceLog(stateDir string) (*os.File, string) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, serviceLogFailure("could not create the service state directory", err)
	}
	f, err := openServiceLog(filepath.Join(stateDir, "agent.log"))
	if err != nil {
		return nil, serviceLogFailure("could not open agent.log", err)
	}
	return f, ""
}

func serviceLogFailure(operation string, err error) string {
	// PathError can contain the value of OWNSAND_AGENT_STATE_DIR. Only report
	// the operating system error code and its system-supplied description.
	var code windows.Errno
	if errors.As(err, &code) {
		return fmt.Sprintf("%s (Windows error %d: %v)", operation, code, code)
	}
	return operation
}

const startupDiagnosticEventID = 1

func writeStartupEvent(message string) error {
	// Provision the source on demand so upgraded services can use it too.
	// Try to write even when provisioning fails, since the source may already
	// be registered and the service account may lack registry write access.
	_ = ensureStartupEventSource()
	log, err := eventlog.Open(service.Name)
	if err != nil {
		return err
	}
	defer log.Close()
	return log.Error(startupDiagnosticEventID, message)
}

func ensureStartupEventSource() error {
	const sourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + service.Name
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, sourceKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	// Use the system message resource for insertion strings, as x/sys does
	// in InstallAsEventCreate. Set the values even if the key exists so a
	// subsequent start can repair a partially registered source.
	if err := key.SetExpandStringValue("EventMessageFile", `%SystemRoot%\System32\EventCreate.exe`); err != nil {
		return err
	}
	if err := key.SetDWordValue("CustomSource", 1); err != nil {
		return err
	}
	return key.SetDWordValue("TypesSupported", eventlog.Error)
}

// openServiceLog opens (creating it when needed) the log file in append
// mode with FILE_SHARE_DELETE, in addition to read and write sharing.
//
// This is a Windows-specific workaround: os.OpenFile never sets
// FILE_SHARE_DELETE, so a file that the service keeps open for its whole run
// cannot be deleted. The device removal deletes the state directory content,
// including agent.log, while the service still runs and logs; without the
// delete sharing, that deletion fails and the device key can stay on disk.
// With it, the delete completes when the handle closes. Remove this helper
// when the Go standard library opens files with FILE_SHARE_DELETE.
func openServiceLog(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// FILE_APPEND_DATA without FILE_WRITE_DATA makes every write append,
	// as O_APPEND does in os.OpenFile.
	h, err := windows.CreateFile(
		name,
		windows.FILE_APPEND_DATA,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
