// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config loads and validates the tide-api configuration from
// environment variables without a project prefix. The owning contract is
// the Configuration section of docs/servers-tide-api-foundation.md.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/delinoio/mono/servers/internal/listenaddr"
	"github.com/delinoio/mono/servers/internal/weborigin"
	"github.com/delinoio/mono/servers/tide-api/internal/secrets"
)

// Environment variable names read by the server. The OpenTelemetry SDK
// reads the OTEL_* variables itself.
const (
	EnvAddr                     = "ADDR"
	EnvLogLevel                 = "LOG_LEVEL"
	EnvDatabaseURL              = "DATABASE_URL"
	EnvRunMigrations            = "RUN_MIGRATIONS"
	EnvLogtoIssuer              = "LOGTO_ISSUER"
	EnvLogtoJWKSURL             = "LOGTO_JWKS_URL"
	EnvLogtoJWKSFile            = "LOGTO_JWKS_FILE"
	EnvLogtoAudience            = "LOGTO_AUDIENCE"
	EnvCORSOrigins              = "CORS_ORIGINS"
	EnvTrustProxyClientAddress  = "TRUST_PROXY_CLIENT_ADDRESS"
	EnvAllowDirectClientAddress = "ALLOW_DIRECT_CLIENT_ADDRESS"
	EnvSecretKeys               = "TIDE_SECRET_KEYS"
	EnvMetricsAddr              = "METRICS_ADDR"
	EnvOwnSandAPIOrigin         = "OWNSAND_API_ORIGIN"
	EnvPublicOrigin             = "PUBLIC_ORIGIN"

	// The tide GitHub App. Set all six or none: without them the GitHub
	// layer is off (see GitHubApp).
	EnvGitHubAppID            = "GITHUB_APP_ID"
	EnvGitHubAppClientID      = "GITHUB_APP_CLIENT_ID"
	EnvGitHubAppClientSecret  = "GITHUB_APP_CLIENT_SECRET"
	EnvGitHubAppPrivateKey    = "GITHUB_APP_PRIVATE_KEY"
	EnvGitHubAppWebhookSecret = "GITHUB_APP_WEBHOOK_SECRET"
	EnvGitHubAppSlug          = "GITHUB_APP_SLUG"
	EnvGitHubAPIOrigin        = "GITHUB_API_ORIGIN"
	EnvGitHubWebOrigin        = "GITHUB_WEB_ORIGIN"
)

// Defaults.
const (
	// DefaultAddr is the development HTTP port of the control plane (h2c).
	DefaultAddr = "127.0.0.1:18400"
	// DevelopmentMetricsAddr is the development metrics port. The server
	// default is empty (listener off); the development launcher sets it.
	DevelopmentMetricsAddr = "127.0.0.1:19400"
	// DefaultAudience is the Logto API resource indicator of tide.
	DefaultAudience = "https://tide.api.delino.io"
	// DefaultCORSOrigin is the production console origin.
	DefaultCORSOrigin = "https://tide.delino.io"
	// DefaultOwnSandAPIOrigin is the production origin of ownsand-api.
	DefaultOwnSandAPIOrigin = "https://ownsand.api.delino.io"
	// DefaultPublicOrigin is the public origin of tide-api, which the
	// webhook URLs that tide shows to admins use.
	DefaultPublicOrigin = "https://tide.api.delino.io"
	// DefaultGitHubAPIOrigin is the GitHub REST API of github.com.
	DefaultGitHubAPIOrigin = "https://api.github.com"
	// DefaultGitHubWebOrigin is github.com, which serves the app
	// installation page and the OAuth code exchange.
	DefaultGitHubWebOrigin = "https://github.com"
)

// ClientAddressMode selects where the client address of a request comes
// from. The address keys the per-client rate limit. The rules are the same
// as in ownsand-api.
type ClientAddressMode string

const (
	// ClientAddressNone gives requests no client address, so the per-client
	// rate limit does not apply. It is the default: behind a reverse proxy
	// that is not verified, the TCP peer is the proxy and would put every
	// caller in one bucket.
	ClientAddressNone ClientAddressMode = "none"
	// ClientAddressDirect uses the TCP peer address and ignores forwarded
	// headers. Use it only when callers connect to the server directly.
	ClientAddressDirect ClientAddressMode = "direct"
	// ClientAddressVerifiedProxy uses the last entry of the last
	// X-Forwarded-For header, which is the value that the verified Caddy
	// reverse proxy appends.
	ClientAddressVerifiedProxy ClientAddressMode = "verified_proxy"
)

// ResolveClientAddressMode returns the client address mode for the two
// settings. The verified proxy takes precedence over direct connections.
func ResolveClientAddressMode(trustProxy, allowDirect bool) ClientAddressMode {
	switch {
	case trustProxy:
		return ClientAddressVerifiedProxy
	case allowDirect:
		return ClientAddressDirect
	default:
		return ClientAddressNone
	}
}

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

// Config is the validated server configuration. It holds secrets; never log
// it as a whole.
type Config struct {
	Addr        string
	LogLevel    LogLevel
	DatabaseURL string
	// RunMigrations applies the embedded migrations at startup.
	RunMigrations bool

	LogtoIssuer   string
	LogtoJWKSURL  string
	LogtoJWKSFile string
	LogtoAudience string

	CORSOrigins []string
	// TrustProxyClientAddress and AllowDirectClientAddress resolve to
	// ClientAddressMode.
	TrustProxyClientAddress  bool
	AllowDirectClientAddress bool
	ClientAddressMode        ClientAddressMode

	// SecretKeys encrypts the stored organization credentials.
	SecretKeys *secrets.Keyring

	// MetricsAddr is the listen address of the internal Prometheus
	// listener; empty turns it off.
	MetricsAddr string

	// OwnSandAPIOrigin is the origin of the OwnSand API that tide calls with
	// the OwnSand API key of each organization.
	OwnSandAPIOrigin string
	// PublicOrigin is the public origin of this server, for webhook URLs.
	PublicOrigin string

	// GitHub is the tide GitHub App, or nil when no GITHUB_APP_* variable
	// is set (the GitHub layer is then off).
	GitHub *GitHubApp
}

// GitHubApp holds the settings and service secrets of the tide GitHub App.
// It holds secrets; never log it.
type GitHubApp struct {
	// AppID is the numeric app ID. The app JWT uses the client ID as its
	// issuer, as GitHub recommends; the app ID checks the installations
	// that GitHub lists.
	AppID        int64
	ClientID     string
	ClientSecret string
	// PrivateKey signs the app JWT (RS256).
	PrivateKey    *rsa.PrivateKey
	WebhookSecret string
	// Slug is the app's URL name (https://github.com/apps/<slug>).
	Slug string
	// APIOrigin and WebOrigin are the GitHub REST API and web origins; tests
	// point them at a fake.
	APIOrigin string
	WebOrigin string
}

// Load reads the configuration through lookup, which has the signature of
// os.LookupEnv. Error messages never contain secret values.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		Addr:          DefaultAddr,
		LogLevel:      LogLevelInfo,
		RunMigrations: true,
		LogtoAudience: DefaultAudience,
		CORSOrigins:   []string{DefaultCORSOrigin},

		OwnSandAPIOrigin: DefaultOwnSandAPIOrigin,
		PublicOrigin:     DefaultPublicOrigin,
	}
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.Trim(value, "\t\n\v\f\r ")
	}

	if v := get(EnvAddr); v != "" {
		if err := listenaddr.Validate(EnvAddr, v); err != nil {
			return Config{}, err
		}
		cfg.Addr = v
	}
	if v := get(EnvLogLevel); v != "" {
		switch level := LogLevel(asciiLower(v)); level {
		case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
			cfg.LogLevel = level
		default:
			return Config{}, fmt.Errorf("%s must be one of debug, info, warn, error", EnvLogLevel)
		}
	}
	cfg.DatabaseURL = get(EnvDatabaseURL)
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	var err error
	if cfg.RunMigrations, err = parseBool(EnvRunMigrations, get(EnvRunMigrations), true); err != nil {
		return Config{}, err
	}

	cfg.LogtoIssuer = get(EnvLogtoIssuer)
	if cfg.LogtoIssuer == "" {
		return Config{}, fmt.Errorf("%s is required", EnvLogtoIssuer)
	}
	if err := validateURL(EnvLogtoIssuer, cfg.LogtoIssuer); err != nil {
		return Config{}, err
	}
	cfg.LogtoJWKSFile = get(EnvLogtoJWKSFile)
	cfg.LogtoJWKSURL = get(EnvLogtoJWKSURL)
	if cfg.LogtoJWKSURL == "" && cfg.LogtoJWKSFile == "" {
		cfg.LogtoJWKSURL = strings.TrimSuffix(cfg.LogtoIssuer, "/") + "/jwks"
	}
	if cfg.LogtoJWKSURL != "" {
		if err := validateURL(EnvLogtoJWKSURL, cfg.LogtoJWKSURL); err != nil {
			return Config{}, err
		}
	}
	if v := get(EnvLogtoAudience); v != "" {
		cfg.LogtoAudience = v
	}

	if v := get(EnvCORSOrigins); v != "" {
		cfg.CORSOrigins = nil
		for _, origin := range splitList(v) {
			u, err := url.Parse(origin)
			// ForceQuery detects a bare "?". Check the raw origin for a bare "#".
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
				u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") {
				return Config{}, fmt.Errorf("%s must list origins such as https://tide.delino.io", EnvCORSOrigins)
			}
			// The CORS middleware matches the browser's Origin header exactly.
			canonical, ok := weborigin.Canonical(u)
			if !ok {
				return Config{}, fmt.Errorf("%s must list origins such as https://tide.delino.io", EnvCORSOrigins)
			}
			cfg.CORSOrigins = append(cfg.CORSOrigins, canonical)
		}
	}
	if cfg.TrustProxyClientAddress, err = parseBool(EnvTrustProxyClientAddress, get(EnvTrustProxyClientAddress), false); err != nil {
		return Config{}, err
	}
	if cfg.AllowDirectClientAddress, err = parseBool(EnvAllowDirectClientAddress, get(EnvAllowDirectClientAddress), false); err != nil {
		return Config{}, err
	}
	cfg.ClientAddressMode = ResolveClientAddressMode(cfg.TrustProxyClientAddress, cfg.AllowDirectClientAddress)

	keys := get(EnvSecretKeys)
	if keys == "" {
		return Config{}, fmt.Errorf("%s is required: one or more <key ID>:<base64 of 32 random bytes> entries, separated by commas", EnvSecretKeys)
	}
	if cfg.SecretKeys, err = secrets.ParseKeys(keys); err != nil {
		// The secrets package names entries by position or key ID only.
		return Config{}, fmt.Errorf("%s: %w", EnvSecretKeys, err)
	}

	if v := get(EnvMetricsAddr); v != "" {
		if err := listenaddr.Validate(EnvMetricsAddr, v); err != nil {
			return Config{}, err
		}
		cfg.MetricsAddr = v
	}
	if v := get(EnvOwnSandAPIOrigin); v != "" {
		if cfg.OwnSandAPIOrigin, err = parseOrigin(EnvOwnSandAPIOrigin, v, "https://ownsand.api.delino.io"); err != nil {
			return Config{}, err
		}
	}
	if v := get(EnvPublicOrigin); v != "" {
		if cfg.PublicOrigin, err = parseOrigin(EnvPublicOrigin, v, "https://tide.api.delino.io"); err != nil {
			return Config{}, err
		}
	}
	if cfg.GitHub, err = loadGitHubApp(get); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadGitHubApp reads the GitHub App settings. With none of the six app
// variables set it returns nil, so development setups without a GitHub App
// still start; with some but not all of them it fails.
func loadGitHubApp(get func(string) string) (*GitHubApp, error) {
	names := []string{EnvGitHubAppID, EnvGitHubAppClientID, EnvGitHubAppClientSecret, EnvGitHubAppPrivateKey, EnvGitHubAppWebhookSecret, EnvGitHubAppSlug}
	var missing []string
	for _, n := range names {
		if get(n) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) == len(names) {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the GitHub App needs all of %s; missing: %s", strings.Join(names, ", "), strings.Join(missing, ", "))
	}
	app := &GitHubApp{
		ClientID: get(EnvGitHubAppClientID), ClientSecret: get(EnvGitHubAppClientSecret), WebhookSecret: get(EnvGitHubAppWebhookSecret),
		Slug: get(EnvGitHubAppSlug), APIOrigin: DefaultGitHubAPIOrigin, WebOrigin: DefaultGitHubWebOrigin,
	}
	id, err := strconv.ParseInt(get(EnvGitHubAppID), 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("%s must be a positive integer", EnvGitHubAppID)
	}
	app.AppID = id
	if !printableToken(app.ClientID) {
		return nil, fmt.Errorf("%s must be the app client ID (printable ASCII without spaces)", EnvGitHubAppClientID)
	}
	if !printableToken(app.ClientSecret) {
		return nil, fmt.Errorf("%s must be printable ASCII without spaces", EnvGitHubAppClientSecret)
	}
	if len(app.WebhookSecret) < 16 {
		return nil, fmt.Errorf("%s must have at least 16 characters", EnvGitHubAppWebhookSecret)
	}
	if !validSlug(app.Slug) {
		return nil, fmt.Errorf("%s must be the app slug (lowercase letters, digits and '-')", EnvGitHubAppSlug)
	}
	if app.PrivateKey, err = ParseRSAPrivateKey(get(EnvGitHubAppPrivateKey)); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvGitHubAppPrivateKey, err)
	}
	if v := get(EnvGitHubAPIOrigin); v != "" {
		if app.APIOrigin, err = parseOrigin(EnvGitHubAPIOrigin, v, DefaultGitHubAPIOrigin); err != nil {
			return nil, err
		}
	}
	if v := get(EnvGitHubWebOrigin); v != "" {
		if app.WebOrigin, err = parseOrigin(EnvGitHubWebOrigin, v, DefaultGitHubWebOrigin); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// ParseRSAPrivateKey parses the PEM of the GitHub App private key: the
// PKCS #1 block ("RSA PRIVATE KEY") that GitHub generates or a PKCS #8
// block ("PRIVATE KEY"). A value with literal "\n" sequences instead of line
// breaks is accepted, because many secret stores keep one line. Errors never
// contain key material.
func ParseRSAPrivateKey(value string) (*rsa.PrivateKey, error) {
	value = strings.ReplaceAll(value, `\n`, "\n")
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("must be a PEM private key")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid PKCS #1 RSA private key")
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid PKCS #8 private key")
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("must be an RSA private key")
		}
		return rsaKey, nil
	}
	return nil, errors.New("must be an RSA PRIVATE KEY or PRIVATE KEY PEM block")
}

func printableToken(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] >= 0x7f {
			return false
		}
	}
	return true
}

func validSlug(v string) bool {
	if v == "" || len(v) > 100 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// parseOrigin accepts an http or https origin without path (a trailing "/"
// is removed), query, fragment or user info.
func parseOrigin(name, value, example string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.User != nil || strings.Contains(value, "#") {
		return "", fmt.Errorf("%s must be an origin such as %s", name, example)
	}
	return strings.TrimSuffix(value, "/"), nil
}

func parseBool(name, value string, def bool) (bool, error) {
	if value == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return b, nil
}

// asciiLower converts only the ASCII letters A-Z to lower case, so a value
// such as "İNFO" does not pass as "info" through Unicode case mapping.
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

func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func validateURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s must be an http or https URL", name)
	}
	return nil
}
