// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config loads and validates the ownsand-api configuration from
// environment variables. The owning contract is
// docs/servers-ownsand-api-foundation.md (Configuration).
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	pionstun "github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/internal/listenaddr"
)

// Environment variable names read by the server.
const (
	EnvAddr                     = "ADDR"
	EnvLogLevel                 = "LOG_LEVEL"
	EnvDatabaseURL              = "DATABASE_URL"
	EnvLogtoIssuer              = "LOGTO_ISSUER"
	EnvLogtoJWKSURL             = "LOGTO_JWKS_URL"
	EnvLogtoJWKSFile            = "LOGTO_JWKS_FILE"
	EnvLogtoAudience            = "LOGTO_AUDIENCE"
	EnvNonceKey                 = "NONCE_KEY"
	EnvNonceKeyPrevious         = "NONCE_KEY_PREVIOUS"
	EnvCORSOrigins              = "CORS_ORIGINS"
	EnvTrustProxyClientAddress  = "TRUST_PROXY_CLIENT_ADDRESS"
	EnvAllowDirectClientAddress = "ALLOW_DIRECT_CLIENT_ADDRESS"
	EnvRunMigrations            = "RUN_MIGRATIONS"
	EnvSTUNURLs                 = "STUN_URLS"
	EnvR2Endpoint               = "R2_ENDPOINT"
	EnvR2Bucket                 = "R2_BUCKET"
	EnvR2AccessKeyID            = "R2_ACCESS_KEY_ID"
	EnvR2SecretAccessKey        = "R2_SECRET_ACCESS_KEY"
	EnvSnapshotPartSizeBytes    = "SNAPSHOT_PART_SIZE_BYTES"
	EnvSTUNAddr                 = "STUN_ADDR"
	EnvSTUNRateBytesPerSecond   = "STUN_RATE_LIMIT_BYTES_PER_SECOND"
	EnvSTUNRateBurstBytes       = "STUN_RATE_LIMIT_BURST_BYTES"
	EnvAccountsAPIOrigin        = "ACCOUNTS_API_ORIGIN"
	EnvLogtoTokenEndpoint       = "LOGTO_TOKEN_ENDPOINT"
	EnvAccountsM2MClientID      = "ACCOUNTS_M2M_CLIENT_ID"
	EnvAccountsM2MClientSecret  = "ACCOUNTS_M2M_CLIENT_SECRET"
	EnvMetricsAddr              = "METRICS_ADDR"
)

// Defaults.
const (
	// DefaultAddr is the development port of the control plane (h2c).
	DefaultAddr = "127.0.0.1:18300"
	// DefaultAudience is the Logto API resource indicator of OwnSand.
	DefaultAudience = "https://ownsand.api.delino.io"
	// DefaultCORSOrigin is the production console origin.
	DefaultCORSOrigin = "https://ownsand.delino.io"
	// DefaultSTUNURL is the production STUN URL that callers and agents get.
	DefaultSTUNURL = "stun:ownsand.api.delino.io:3478"
	// DefaultSTUNAddr is the UDP listen address of the STUN service.
	DefaultSTUNAddr = "127.0.0.1:13478"
	// DefaultSTUNRateBytesPerSecond and DefaultSTUNRateBurstBytes are the
	// response byte budget per source address: about 25 binding responses
	// per second sustained with a burst of about 100.
	DefaultSTUNRateBytesPerSecond = 1024
	DefaultSTUNRateBurstBytes     = 4096
	// DefaultAccountsAPIOrigin is the production origin of the delino-accounts
	// API.
	DefaultAccountsAPIOrigin = "https://accounts.api.delino.io"
	// DefaultSnapshotPartSizeBytes is the snapshot export part size (64 MiB).
	DefaultSnapshotPartSizeBytes = 64 << 20
	// minNonceKeySize is the smallest accepted nonce key.
	minNonceKeySize = 32
)

// ClientAddressMode selects where the client address of a request comes
// from. The address keys the per-client rate limits. The mode is resolved
// once at startup by Load.
type ClientAddressMode string

const (
	// ClientAddressNone gives requests no client address, so the per-client
	// rate limits do not apply. It is the default: behind a reverse proxy
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

// R2 is the object storage configuration of snapshot exports. Enabled is
// false when no bucket is configured; exports then fail with
// failed_precondition.
type R2 struct {
	Enabled         bool
	Endpoint        string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

// Accounts is the delino-accounts client configuration of usage reporting.
// Enabled is false when the machine-to-machine credential is not configured
// (development and tests); usage is then still metered into the outbox,
// and the reporter stays idle.
type Accounts struct {
	Enabled       bool
	APIOrigin     string
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
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

	// NonceKey is nil when unset: the server then uses a random key per
	// process, which is valid only for a single instance.
	NonceKey         []byte
	NonceKeyPrevious []byte

	CORSOrigins []string
	// TrustProxyClientAddress uses the last X-Forwarded-For entry (the one
	// that the Caddy reverse proxy appends) as the client address for rate
	// limits.
	TrustProxyClientAddress bool
	// AllowDirectClientAddress uses the TCP peer address as the client
	// address when TrustProxyClientAddress is false. Set it only for direct
	// connections, such as development and tests.
	AllowDirectClientAddress bool
	// ClientAddressMode is the mode that the two settings above resolve to.
	ClientAddressMode ClientAddressMode

	// STUNURLs are the STUN server URLs for callers and agents.
	STUNURLs []string

	R2                    R2
	SnapshotPartSizeBytes int64

	// STUNAddr is the UDP listen address of STUN; STUNRateBytesPerSecond
	// and STUNRateBurstBytes budget response bytes per source address.
	STUNAddr               string
	STUNRateBytesPerSecond int
	STUNRateBurstBytes     int

	// Accounts configures usage reporting to delino-accounts.
	Accounts Accounts
	// MetricsAddr is the listen address of the internal Prometheus
	// listener; empty turns it off.
	MetricsAddr string
}

// Load reads the configuration through lookup, which has the signature of
// os.LookupEnv.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		Addr:                  DefaultAddr,
		LogLevel:              LogLevelInfo,
		RunMigrations:         true,
		LogtoAudience:         DefaultAudience,
		CORSOrigins:           []string{DefaultCORSOrigin},
		STUNURLs:              []string{DefaultSTUNURL},
		SnapshotPartSizeBytes: DefaultSnapshotPartSizeBytes,
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
	if v := get(EnvRunMigrations); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", EnvRunMigrations)
		}
		cfg.RunMigrations = b
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

	var err error
	if cfg.NonceKey, err = parseKey(EnvNonceKey, get(EnvNonceKey)); err != nil {
		return Config{}, err
	}
	if cfg.NonceKeyPrevious, err = parseKey(EnvNonceKeyPrevious, get(EnvNonceKeyPrevious)); err != nil {
		return Config{}, err
	}
	if cfg.NonceKeyPrevious != nil && cfg.NonceKey == nil {
		return Config{}, fmt.Errorf("%s needs %s", EnvNonceKeyPrevious, EnvNonceKey)
	}

	if v := get(EnvCORSOrigins); v != "" {
		cfg.CORSOrigins = nil
		for _, origin := range splitList(v) {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" ||
				// A bare "#" leaves u.Fragment empty, so check the raw value too. Browsers never send a fragment in Origin.
				u.Fragment != "" || strings.Contains(origin, "#") {
				return Config{}, fmt.Errorf("%s must list origins such as https://ownsand.delino.io", EnvCORSOrigins)
			}
			cfg.CORSOrigins = append(cfg.CORSOrigins, origin)
		}
	}
	if v := get(EnvTrustProxyClientAddress); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", EnvTrustProxyClientAddress)
		}
		cfg.TrustProxyClientAddress = b
	}
	if v := get(EnvAllowDirectClientAddress); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", EnvAllowDirectClientAddress)
		}
		cfg.AllowDirectClientAddress = b
	}
	cfg.ClientAddressMode = ResolveClientAddressMode(cfg.TrustProxyClientAddress, cfg.AllowDirectClientAddress)

	if v := get(EnvSTUNURLs); v != "" {
		cfg.STUNURLs = nil
		for _, u := range splitList(v) {
			if !validSTUNURL(u) {
				return Config{}, fmt.Errorf("%s must list stun: URLs with a host", EnvSTUNURLs)
			}
			cfg.STUNURLs = append(cfg.STUNURLs, u)
		}
	}
	cfg.R2 = R2{
		Endpoint:        get(EnvR2Endpoint),
		Bucket:          get(EnvR2Bucket),
		AccessKeyID:     get(EnvR2AccessKeyID),
		SecretAccessKey: get(EnvR2SecretAccessKey),
	}
	if cfg.R2.Bucket != "" || cfg.R2.Endpoint != "" || cfg.R2.AccessKeyID != "" || cfg.R2.SecretAccessKey != "" {
		if cfg.R2.Bucket == "" || cfg.R2.Endpoint == "" || cfg.R2.AccessKeyID == "" || cfg.R2.SecretAccessKey == "" {
			return Config{}, fmt.Errorf("%s, %s, %s and %s must be set together", EnvR2Endpoint, EnvR2Bucket, EnvR2AccessKeyID, EnvR2SecretAccessKey)
		}
		if err := validateURL(EnvR2Endpoint, cfg.R2.Endpoint); err != nil {
			return Config{}, err
		}
		cfg.R2.Enabled = true
	}
	if v := get(EnvSnapshotPartSizeBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		// Each part is one R2 object. A snapshot may have at most 10,000
		// parts, so the largest snapshot is the part size times 10,000: about
		// 48.8 GiB at 5 MiB and 625 GiB at the 64 MiB default. The agent
		// rejects a larger snapshot before it uploads any part.
		if err != nil || n < 5<<20 || n > 5<<30 {
			return Config{}, fmt.Errorf("%s must be 5 MiB to 5 GiB", EnvSnapshotPartSizeBytes)
		}
		cfg.SnapshotPartSizeBytes = n
	}
	cfg.STUNAddr = DefaultSTUNAddr
	if v := get(EnvSTUNAddr); v != "" {
		if err := listenaddr.Validate(EnvSTUNAddr, v); err != nil {
			return Config{}, err
		}
		cfg.STUNAddr = v
	}
	var err2 error
	if cfg.STUNRateBytesPerSecond, err2 = positiveInt(EnvSTUNRateBytesPerSecond, get(EnvSTUNRateBytesPerSecond), DefaultSTUNRateBytesPerSecond); err2 != nil {
		return Config{}, err2
	}
	if cfg.STUNRateBurstBytes, err2 = positiveInt(EnvSTUNRateBurstBytes, get(EnvSTUNRateBurstBytes), DefaultSTUNRateBurstBytes); err2 != nil {
		return Config{}, err2
	}

	cfg.Accounts = Accounts{
		APIOrigin:     DefaultAccountsAPIOrigin,
		TokenEndpoint: get(EnvLogtoTokenEndpoint),
		ClientID:      get(EnvAccountsM2MClientID),
		ClientSecret:  get(EnvAccountsM2MClientSecret),
	}
	if v := get(EnvAccountsAPIOrigin); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(v, "#") {
			return Config{}, fmt.Errorf("%s must be an origin such as https://accounts.api.delino.io", EnvAccountsAPIOrigin)
		}
		cfg.Accounts.APIOrigin = strings.TrimSuffix(v, "/")
	}
	if a := cfg.Accounts; a.TokenEndpoint != "" || a.ClientID != "" || a.ClientSecret != "" {
		if a.TokenEndpoint == "" || a.ClientID == "" || a.ClientSecret == "" {
			return Config{}, fmt.Errorf("%s, %s and %s must be set together", EnvLogtoTokenEndpoint, EnvAccountsM2MClientID, EnvAccountsM2MClientSecret)
		}
		if err := validateURL(EnvLogtoTokenEndpoint, a.TokenEndpoint); err != nil {
			return Config{}, err
		}
		cfg.Accounts.Enabled = true
	}
	if v := get(EnvMetricsAddr); v != "" {
		if err := listenaddr.Validate(EnvMetricsAddr, v); err != nil {
			return Config{}, err
		}
		cfg.MetricsAddr = v
	}
	return cfg, nil
}

func positiveInt(name, value string, def int) (int, error) {
	if value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
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

func parseKey(name, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		key, err = base64.StdEncoding.DecodeString(value)
	}
	if err != nil || len(key) < minNonceKeySize {
		return nil, fmt.Errorf("%s must be at least %d bytes in base64", name, minNonceKeySize)
	}
	return key, nil
}

func validateURL(name, value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s must be an http or https URL", name)
	}
	return nil
}

// validSTUNURL reports whether u is a stun: URL that the same parser as Pion
// WebRTC accepts and that names a host. A hostless or otherwise malformed URL
// would be published to agents and make their peer connection setup fail.
func validSTUNURL(u string) bool {
	if !strings.HasPrefix(u, "stun:") {
		return false
	}
	uri, err := pionstun.ParseURI(u)
	return err == nil && uri.Scheme == pionstun.SchemeTypeSTUN && uri.Host != ""
}
