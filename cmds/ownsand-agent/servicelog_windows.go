// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

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
