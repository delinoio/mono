// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config loads and validates the accounts-api configuration
// from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/delinoio/mono/servers/internal/listenaddr"
)

// Environment variable names read by the server.
const (
	EnvAddr     = "ADDR"
	EnvLogLevel = "LOG_LEVEL"
)

// DefaultAddr listens on loopback only so that a local run is never exposed
// to the network by accident.
const DefaultAddr = "127.0.0.1:8100"

// LogLevel is the accepted set of log level values.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// SlogLevel converts the configured level to a slog level.
func (l LogLevel) SlogLevel() slog.Level {
	switch l {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// asciiLower returns value with only the ASCII letters A-Z converted to lower
// case. It deliberately does not use strings.ToLower, which applies Unicode
// case mapping: U+0130 (capital I with dot above) followed by "NFO" would
// lower-case to "info" and pass validation, although the value is not one of
// the documented level names.
func asciiLower(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func parseLogLevel(value string) (LogLevel, error) {
	switch level := LogLevel(asciiLower(value)); level {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return level, nil
	default:
		return "", fmt.Errorf("%s must be one of debug, info, warn, error; got %q", EnvLogLevel, value)
	}
}

// Config is the validated server configuration.
type Config struct {
	Addr     string
	LogLevel LogLevel
}

// Load reads the configuration through lookup, which has the signature of
// os.LookupEnv. Unset or empty values select the documented defaults.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{Addr: DefaultAddr, LogLevel: LogLevelInfo}

	if value := lookupNonEmpty(lookup, EnvAddr); value != "" {
		if err := listenaddr.Validate(EnvAddr, value); err != nil {
			return Config{}, err
		}
		cfg.Addr = value
	}

	if value := lookupNonEmpty(lookup, EnvLogLevel); value != "" {
		level, err := parseLogLevel(value)
		if err != nil {
			return Config{}, err
		}
		cfg.LogLevel = level
	}

	return cfg, nil
}

// asciiWhiteSpace is the set of ASCII white space characters that are removed
// from the edges of a configuration value: space, tab, LF, VT, FF and CR.
const asciiWhiteSpace = "\t\n\v\f\r "

// lookupNonEmpty returns the value of name without leading and trailing ASCII
// white space. It deliberately does not use strings.TrimSpace, which also
// removes Unicode white space such as U+00A0 and U+3000: such a value would
// count as unset, or be validated after its padding was removed, instead of
// being rejected as not valid.
func lookupNonEmpty(lookup func(string) (string, bool), name string) string {
	value, _ := lookup(name)
	return strings.Trim(value, asciiWhiteSpace)
}
