// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"log/slog"
	"runtime"
	"syscall"
)

// startupFailureReason is the stable, machine-readable cause of a startup
// failure. The server_exited event carries it as startup_failure_reason, so a
// supervisor (for example the end-to-end harness) can decide what to do
// without matching operating-system error text, which differs per platform
// and locale.
type startupFailureReason string

const (
	// startupFailureReasonAddressInUse reports that a listener address
	// (HTTP or STUN) is already bound by another socket.
	startupFailureReasonAddressInUse startupFailureReason = "address_in_use"
)

// startupFailureReasonKey is the log attribute that carries the reason.
const startupFailureReasonKey = "startup_failure_reason"

// winsockAddrInUse is WSAEADDRINUSE, the native Winsock error of a bind on an
// address that is in use. The syscall package on Windows defines EADDRINUSE as
// an invented value, and a net.Listen error carries the native number.
const winsockAddrInUse syscall.Errno = 10048

// isAddressInUseErrno reports whether errno is the address-in-use error of
// the operating system goos. goos is a parameter so that tests can cover both
// platform rules on any host.
func isAddressInUseErrno(errno syscall.Errno, goos string) bool {
	if goos == "windows" {
		return errno == winsockAddrInUse
	}
	return errno == syscall.EADDRINUSE
}

// classifyAddressInUse returns the reason address_in_use when err wraps the
// address-in-use errno of the running platform, and "" otherwise.
func classifyAddressInUse(err error, goos string) startupFailureReason {
	var errno syscall.Errno
	if errors.As(err, &errno) && isAddressInUseErrno(errno, goos) {
		return startupFailureReasonAddressInUse
	}
	return ""
}

// startupFailure wraps a startup error with its classified reason. The
// message and the cause of the original error stay unchanged.
type startupFailure struct {
	reason startupFailureReason
	err    error
}

func (f *startupFailure) Error() string { return f.err.Error() }
func (f *startupFailure) Unwrap() error { return f.err }

// classifyBindError wraps the error of a listener bind with its startup
// failure reason when it has one, and returns err otherwise.
func classifyBindError(err error) error {
	return classifyBindErrorFor(err, runtime.GOOS)
}

func classifyBindErrorFor(err error, goos string) error {
	if err == nil {
		return nil
	}
	if reason := classifyAddressInUse(err, goos); reason != "" {
		return &startupFailure{reason: reason, err: err}
	}
	return err
}

// startupFailureReasonOf returns the reason that err (or an error it wraps)
// carries, or "".
func startupFailureReasonOf(err error) startupFailureReason {
	var failure *startupFailure
	if errors.As(err, &failure) {
		return failure.reason
	}
	return ""
}

// logServerExited logs the server_exited event of a failed run. The original
// error stays in the event; startup_failure_reason is added only when the
// failure has a classified reason.
func logServerExited(logger *slog.Logger, err error) {
	attrs := []any{slog.Any("error", err)}
	if reason := startupFailureReasonOf(err); reason != "" {
		attrs = append(attrs, slog.String(startupFailureReasonKey, string(reason)))
	}
	logger.Error("server_exited", attrs...)
}
