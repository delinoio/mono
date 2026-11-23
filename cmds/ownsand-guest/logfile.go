// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Log file of the Windows service in the state directory. A service has no
// standard error, so the service logs to this file; at start a file larger
// than logRotateBytes moves to logFile + ".1". The file logic lives here, and
// not in service_windows.go, so that it builds and runs in tests on every
// platform; only the Windows service uses it outside tests.
const (
	logFile        = "guest.log"
	logRotateBytes = 16 << 20
)

// logOutput is what an opener of the helper log returns: the writer for the
// log, and diagnostics of the setup that the opener could not log itself,
// because the logger does not exist before the opener returns.
type logOutput struct {
	W io.Writer
	// Notes run with the logger of the helper right after its creation.
	Notes []func(*slog.Logger)
	// Close releases W. run calls it after its last log record, when it
	// returns; nil when W needs no release. Without it the log file handle
	// outlives run, which blocks the removal of the file on Windows (tests
	// that run the helper in process).
	Close func() error
}

// rotateLog moves the file at path to path + ".1" when it is larger than
// limit bytes, replacing an earlier archive. A missing file and a file of at
// most limit bytes need no rotation. The error of a failed move is
// returned, with the file and the earlier archive unchanged.
func rotateLog(path string, limit int64) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.Size() <= limit {
		return nil
	}
	return os.Rename(path, path+".1")
}

// openLogFile opens the log file of the state directory for appending and
// rotates it first when it is larger than limit bytes. A failed rotation
// does not stop the helper: the log keeps growing in the active file until a
// later start rotates it, and the failure is returned as a note that logs
// one warning once the logger exists. A file that cannot be opened is an
// error.
func openLogFile(stateDir string, limit int64) (logOutput, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return logOutput{}, err
	}
	path := filepath.Join(stateDir, logFile)
	var out logOutput
	if err := rotateLog(path, limit); err != nil {
		out.Notes = append(out.Notes, func(logger *slog.Logger) {
			logger.Warn("log rotation failed; appending to the active log",
				"operation", "rotate", "path", path, "archive", path+".1", "limit_bytes", limit, "error", err)
		})
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return logOutput{}, err
	}
	out.W = f
	out.Close = f.Close
	return out, nil
}
