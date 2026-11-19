// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"fmt"
	"strings"
	"testing"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{EnvAddr: "  ", EnvLogLevel: ""}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	if cfg.LogLevel != LogLevelInfo {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelInfo)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{EnvAddr: "0.0.0.0:9000", EnvLogLevel: "DEBUG"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "0.0.0.0:9000")
	}
	if cfg.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelDebug)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"missing port":   {EnvAddr: "127.0.0.1"},
		"port too large": {EnvAddr: "127.0.0.1:70000"},
		"named port":     {EnvAddr: "127.0.0.1:http"},
		"unknown level":  {EnvLogLevel: "verbose"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(lookupFrom(values)); err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
		})
	}
}

func TestLoadRejectsInvalidAddrHost(t *testing.T) {
	for name, tc := range map[string]struct{ addr, host string }{
		"space in host":        {"not a host:8100", "not a host"},
		"userinfo":             {"user@127.0.0.1:8100", "user@127.0.0.1"},
		"path":                 {"127.0.0.1/path:8100", "127.0.0.1/path"},
		"trailing space":       {"127.0.0.1 :8100", "127.0.0.1 "},
		"empty label":          {"127..0.1:8100", "127..0.1"},
		"leading hyphen":       {"-bad.example.com:8100", "-bad.example.com"},
		"trailing hyphen":      {"bad-.example.com:8100", "bad-.example.com"},
		"underscore":           {"bad_host:8100", "bad_host"},
		"non-ASCII":            {"höst:8100", "höst"},
		"only a dot":           {".:8100", "."},
		"label too long":       {strings.Repeat("a", 64) + ":8100", strings.Repeat("a", 64)},
		"hostname too long":    {strings.Repeat("a.", 127) + "a:8100", strings.Repeat("a.", 127) + "a"},
		"bad IPv6 in brackets": {"[::1%]:8100", "::1%"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: tc.addr}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.HasPrefix(err.Error(), EnvAddr+" host ") {
				t.Errorf("error = %q, want prefix %q", err, EnvAddr+" host ")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.host)) {
				t.Errorf("error = %q, want it to name host %q", err, tc.host)
			}
		})
	}
}

func TestLoadAcceptsValidAddrHost(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:8100",
		"0.0.0.0:9000",
		":8100",
		"[::1]:8100",
		"[::]:8100",
		"[fe80::1%lo0]:8100",
		"localhost:8100",
		"example.com:8100",
		"example.com.:8100",
		"my-host.internal:0",
		"A1.b2.C3:65535",
		strings.Repeat("a", 63) + ":8100",
	} {
		t.Run(addr, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Addr != addr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, addr)
			}
		})
	}
}

func TestLoadKeepsExistingAddrMessages(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":       "ADDR must be a host:port address",
		":70000":          "ADDR port must be a number from 0 to 65535",
		":http":           "ADDR port must be a number from 0 to 65535",
		"not a host:http": "ADDR port must be a number from 0 to 65535",
	} {
		t.Run(addr, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Load() error = %v, want prefix %q", err, want)
			}
		})
	}
}

func TestSlogLevel(t *testing.T) {
	for level, want := range map[LogLevel]string{
		LogLevelDebug: "DEBUG",
		LogLevelInfo:  "INFO",
		LogLevelWarn:  "WARN",
		LogLevelError: "ERROR",
	} {
		if got := level.SlogLevel().String(); got != want {
			t.Errorf("%q.SlogLevel() = %s, want %s", level, got, want)
		}
	}
}

// unicodeWhiteSpace lists Unicode white space characters that
// strings.TrimSpace removes but that are not ASCII white space.
var unicodeWhiteSpace = map[string]string{
	"U+00A0": " ",
	"U+0085": "\u0085",
	"U+1680": " ",
	"U+2000": " ",
	"U+200A": " ",
	"U+2028": " ",
	"U+2029": " ",
	"U+202F": " ",
	"U+205F": " ",
	"U+3000": "　",
}

func TestLoadRejectsUnicodeWhiteSpaceAddr(t *testing.T) {
	values := map[string]string{
		"U+3000 before address":    "　127.0.0.1:9000",
		"U+00A0 after address":     "127.0.0.1:9000 ",
		"ASCII space then U+00A0":  "  ",
		"U+00A0 before port":       " :8100",
		"U+00A0 around ASCII pads": "  127.0.0.1:9000  ",
	}
	for name, value := range unicodeWhiteSpace {
		values[name+" only"] = value
	}
	for name, addr := range values {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAddr: addr}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), EnvAddr) {
				t.Errorf("error = %q, want it to name %s", err, EnvAddr)
			}
		})
	}
}

func TestLoadRejectsUnicodeWhiteSpaceLogLevel(t *testing.T) {
	values := map[string]string{
		"U+00A0 only":            " ",
		"U+3000 only":            "　",
		"U+00A0 before level":    " debug",
		"U+00A0 after level":     "debug ",
		"U+2028 before level":    " info",
		"ASCII and U+00A0 mixed": "   ",
	}
	for name, level := range values {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvLogLevel: level}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), EnvLogLevel) {
				t.Errorf("error = %q, want it to name %s", err, EnvLogLevel)
			}
		})
	}
}

func TestLoadDefaultsForEveryASCIIWhiteSpace(t *testing.T) {
	for name, value := range map[string]string{
		"space":           " ",
		"tab":             "\t",
		"carriage return": "\r",
		"line feed":       "\n",
		"vertical tab":    "\v",
		"form feed":       "\f",
		"all six":         " \t\r\n\v\f",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvAddr: value, EnvLogLevel: value}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Addr != DefaultAddr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
			}
			if cfg.LogLevel != LogLevelInfo {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelInfo)
			}
		})
	}
}

func TestLoadTrimsASCIIPadding(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvAddr:     " \t127.0.0.1:9000\r\n\v\f ",
		EnvLogLevel: " debug ",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "127.0.0.1:9000")
	}
	if cfg.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelDebug)
	}
}

func TestLoadRejectsNonASCIILogLevelThatFoldsToLevelName(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  string
	}{
		"U+0130 then NFO":             {value: "İNFO", want: "İNFO"},
		"U+0130 then nfo":             {value: "İnfo", want: "İnfo"},
		"U+0130 with ASCII padding":   {value: " İNFO ", want: "İNFO"},
		"U+212A (Kelvin sign) in nfo": {value: "KNFO", want: "KNFO"},
		"U+0131 (dotless i) nfo":      {value: "ınfo", want: "ınfo"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvLogLevel: tc.value}))
			if err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
			want := fmt.Sprintf("LOG_LEVEL must be one of debug, info, warn, error; got %q", tc.want)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
		})
	}
}

func TestLoadAcceptsASCIILogLevelInAnyCase(t *testing.T) {
	for value, want := range map[string]LogLevel{
		"DEBUG": LogLevelDebug,
		"WARN":  LogLevelWarn,
		"Error": LogLevelError,
		"INFO":  LogLevelInfo,
		"iNfO":  LogLevelInfo,
	} {
		t.Run(value, func(t *testing.T) {
			cfg, err := Load(lookupFrom(map[string]string{EnvLogLevel: value}))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.LogLevel != want {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, want)
			}
		})
	}
}
