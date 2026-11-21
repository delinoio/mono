// SPDX-License-Identifier: Apache-2.0

// Package loglevel parses the log level names of the command-line programs
// under cmds/. It is stricter than slog.Level.UnmarshalText, which folds case
// with Unicode mapping and also accepts offset forms such as "info+1".
package loglevel

import (
	"fmt"
	"log/slog"
)

// Parse converts one of the level names debug, info, warn and error to a
// slog.Level. The match folds only the ASCII letters A-Z, so "INFO" and
// "iNfO" select info. Any other character makes the value not valid: a value
// that matches a level name only under Unicode case mapping, such as U+0131
// (dotless i) followed by "nfo" or U+0130 followed by "NFO", and an offset
// form such as "info+1" or "warn-2" are rejected.
func Parse(value string) (slog.Level, error) {
	switch asciiLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log level must be one of debug, info, warn, error; got %q", value)
	}
}

// asciiLower returns value with only the ASCII letters A-Z converted to lower
// case. It deliberately does not use strings.ToLower or strings.ToUpper,
// which apply Unicode case mapping.
func asciiLower(value string) string {
	b := []byte(value)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
