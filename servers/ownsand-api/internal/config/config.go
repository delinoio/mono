// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config loads and validates the ownsand-api configuration from
// environment variables. Secrets come from the service's env file on the
// Droplet; .env.example lists only their names with placeholders.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Environment variable names.
const (
	EnvAddr               = "ADDR"
	EnvLogLevel           = "LOG_LEVEL"
	EnvDatabaseURL        = "DATABASE_URL"
	EnvLogtoEndpoint      = "LOGTO_ENDPOINT"
	EnvAPIResource        = "OWNSAND_API_RESOURCE"
	EnvCORSAllowedOrigins = "CORS_ALLOWED_ORIGINS"
)

// Defaults. The listen address is loopback only so that a local run is never
// exposed to the network by accident.
const (
	DefaultAddr               = "127.0.0.1:8300"
	DefaultAPIResource        = "https://ownsand.api.delino.io"
	DefaultCORSAllowedOrigins = "https://ownsand.delino.io"
)

// LogLevel is the accepted set of log levels.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// SlogLevel converts the level.
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

// Config is the validated configuration.
type Config struct {
	Addr     string
	LogLevel LogLevel
	// Postgres connection string. Secret: never logged.
	DatabaseURL string
	// Logto tenant endpoint, for example https://auth.example.com. Empty
	// disables user token validation: every Logto token is rejected.
	LogtoEndpoint string
	APIResource   string
	// Exact browser origins that CORS allows.
	CORSAllowedOrigins []string
	// Fields of later layers.
	Extra Extra
}

// LogtoIssuer returns the issuer of the tenant.
func (c Config) LogtoIssuer() string { return strings.TrimSuffix(c.LogtoEndpoint, "/") + "/oidc" }

// LogtoJWKSURI returns the JWKS URI of the tenant.
func (c Config) LogtoJWKSURI() string { return c.LogtoIssuer() + "/jwks" }

// Lookup has the signature of os.LookupEnv.
type Lookup func(string) (string, bool)

func get(lookup Lookup, name string) string {
	v, _ := lookup(name)
	return strings.Trim(v, "\t\n\v\f\r ")
}

// Load reads and validates the configuration.
func Load(lookup Lookup) (Config, error) {
	cfg := Config{Addr: DefaultAddr, LogLevel: LogLevelInfo, APIResource: DefaultAPIResource}
	if v := get(lookup, EnvAddr); v != "" {
		if err := validateAddr(EnvAddr, v); err != nil {
			return Config{}, err
		}
		cfg.Addr = v
	}
	if v := get(lookup, EnvLogLevel); v != "" {
		switch l := LogLevel(strings.ToLower(v)); l {
		case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
			cfg.LogLevel = l
		default:
			return Config{}, fmt.Errorf("%s must be one of debug, info, warn, error", EnvLogLevel)
		}
	}
	cfg.DatabaseURL = get(lookup, EnvDatabaseURL)
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	if v := get(lookup, EnvLogtoEndpoint); v != "" {
		if err := validateHTTPSURL(EnvLogtoEndpoint, v, true); err != nil {
			return Config{}, err
		}
		cfg.LogtoEndpoint = v
	}
	if v := get(lookup, EnvAPIResource); v != "" {
		cfg.APIResource = v
	}
	origins := get(lookup, EnvCORSAllowedOrigins)
	if origins == "" {
		origins = DefaultCORSAllowedOrigins
	}
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if err := validateOrigin(o); err != nil {
			return Config{}, err
		}
		cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, o)
	}
	extra, err := loadExtra(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.Extra = extra
	return cfg, nil
}

func validateAddr(name, value string) error {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address: %w", name, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%s port must be a number from 0 to 65535", name)
	}
	return nil
}

// validateHTTPSURL accepts https URLs, and http URLs on loopback hosts for
// local development.
func validateHTTPSURL(name, value string, allowLoopbackHTTP bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", name)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && allowLoopbackHTTP && isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s must use https", name)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s entries must be origins like https://ownsand.delino.io; got %q", EnvCORSAllowedOrigins, o)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%s allows http only for loopback origins; got %q", EnvCORSAllowedOrigins, o)
	}
	return nil
}
