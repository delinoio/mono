// SPDX-License-Identifier: Apache-2.0

package loglevel

import (
	"log/slog"
	"testing"
)

func TestParse(t *testing.T) {
	valid := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"DEBUG": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"iNfO":  slog.LevelInfo,
		"Warn":  slog.LevelWarn,
		"warn":  slog.LevelWarn,
		"Error": slog.LevelError,
		"ERROR": slog.LevelError,
	}
	for value, want := range valid {
		got, err := Parse(value)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", value, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %v, want %v", value, got, want)
		}
	}
	invalid := []string{
		"",
		" info",
		"info ",
		"verbose",
		"ınfo",   // dotless i uppercases to I under Unicode mapping
		"ınfo+1", // dotless i plus offset
		"İNFO",   // capital I with dot above is not an ASCII letter
		"Kerror", // Kelvin sign is not an ASCII letter
		"info+1",
		"warn-2",
		"debug+0",
		"error-4",
		"info\x00",
	}
	for _, value := range invalid {
		if got, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", value, got)
		}
	}
}
