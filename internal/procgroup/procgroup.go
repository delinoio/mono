// SPDX-License-Identifier: Apache-2.0

// Package procgroup starts a command in its own process group and signals
// that group, so that stopping a command also stops the children it
// started.
//
// On Linux and macOS, Set puts the command in a new process group
// (Setpgid) and Signal sends the signal to the whole group. Windows has no
// process-group signals: Set creates a new process group
// (CREATE_NEW_PROCESS_GROUP), Signal with SIGKILL kills only the group
// leader, and Signal with any other signal returns errors.ErrUnsupported.
package procgroup
