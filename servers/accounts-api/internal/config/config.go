// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config loads and validates the accounts-api configuration
// from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/internal/listenaddr"
	"github.com/delinoio/mono/servers/internal/weborigin"
)

// Environment variable names read by the server.
const (
	EnvAddr                    = "ADDR"
	EnvLogLevel                = "LOG_LEVEL"
	EnvDatabaseURL             = "DATABASE_URL"
	EnvRunMigrations           = "RUN_MIGRATIONS"
	EnvLogtoIssuer             = "LOGTO_ISSUER"
	EnvLogtoAudience           = "LOGTO_AUDIENCE"
	EnvLogtoManagementEndpoint = "LOGTO_MANAGEMENT_ENDPOINT"
	EnvLogtoM2MClientID        = "LOGTO_M2M_CLIENT_ID"
	EnvLogtoM2MClientSecret    = "LOGTO_M2M_CLIENT_SECRET"
	EnvCORSOrigins             = "CORS_ORIGINS"
	EnvWebOrigin               = "WEB_ORIGIN"
	EnvResendAPIKey            = "RESEND_API_KEY"
	EnvEmailFrom               = "EMAIL_FROM"
	EnvMetricsAddr             = "METRICS_ADDR"
	EnvPaddleEnvironment       = "PADDLE_ENVIRONMENT"
	EnvPaddleAPIKey            = "PADDLE_API_KEY"
	EnvPaddleWebhookSecret     = "PADDLE_WEBHOOK_SECRET"
	EnvPaddlePriceID           = "PADDLE_SUBSCRIPTION_PRICE_ID"
	EnvPaddleTopUpProductID    = "PADDLE_TOP_UP_PRODUCT_ID"
	EnvUsageServiceClients     = "USAGE_SERVICE_CLIENTS"
)

// DefaultAudience is the Logto API resource indicator of accounts-api.
const DefaultAudience = "https://accounts.api.delino.io"

// DefaultWebOrigin is the production origin of the delino-accounts web app. It
// is the default browser origin for CORS and the origin of invitation links.
const DefaultWebOrigin = "https://accounts.delino.io"

// DefaultAddr listens on loopback only so that a local run is never exposed
// to the network by accident.
const DefaultAddr = "127.0.0.1:18100"

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

// Config is the validated server configuration. The fields that hold
// secrets (DatabaseURL, LogtoM2MClientSecret, ResendAPIKey, PaddleAPIKey and
// PaddleWebhookSecret) must never be logged.
type Config struct {
	Addr     string
	LogLevel LogLevel

	// DatabaseURL is the Postgres connection URL. Secret.
	DatabaseURL string
	// RunMigrations applies the embedded goose migrations at startup.
	RunMigrations bool

	// LogtoIssuer is the Logto issuer, `<tenant endpoint>/oidc`, without a
	// trailing slash.
	LogtoIssuer string
	// LogtoAudience is the expected `aud` of access tokens.
	LogtoAudience string
	// LogtoManagementEndpoint is the default Logto endpoint of the tenant
	// (`https://<tenant-id>.logto.app`), without a trailing slash. The
	// Management API lives under `/api` there, and its token comes from
	// `/oidc/token`.
	LogtoManagementEndpoint string
	LogtoM2MClientID        string
	// LogtoM2MClientSecret is the secret of the Management API
	// machine-to-machine application. Secret.
	LogtoM2MClientSecret string

	// CORSOrigins are the browser origins that may call the API.
	CORSOrigins []string
	// WebOrigin is the origin of the web app in invitation and billing email
	// links.
	WebOrigin string

	// ResendAPIKey is the Resend API key. Secret.
	ResendAPIKey string
	// EmailFrom is the sender address of billing emails.
	EmailFrom string

	// MetricsAddr is the `host:port` of the internal Prometheus listener;
	// empty turns the listener off.
	MetricsAddr string

	// PaddleEnvironment selects the Paddle API: sandbox or production.
	PaddleEnvironment paddle.Environment
	// PaddleAPIKey is the Paddle API key of that environment. Secret.
	PaddleAPIKey string
	// PaddleWebhookSecret is the secret of the Paddle notification
	// destination. Secret.
	PaddleWebhookSecret string
	// PaddleSubscriptionPriceID is the price of the $10/month subscription.
	PaddleSubscriptionPriceID string
	// PaddleTopUpProductID is the product of top-up charges.
	PaddleTopUpProductID string

	// UsageServiceClients maps the Logto application ID of each
	// machine-to-machine client of UsageService to the services that it may
	// use. A client without an entry may call no UsageService RPC.
	UsageServiceClients map[string][]accountsv1.ServiceId
}

// Paddle catalog ID formats (https://developer.paddle.com/api-reference/about/paddle-ids).
var (
	paddlePriceID   = regexp.MustCompile(`^pri_[a-z0-9]{26}$`)
	paddleProductID = regexp.MustCompile(`^pro_[a-z0-9]{26}$`)
)

// Load reads the configuration through lookup, which has the signature of
// os.LookupEnv. Unset or empty values select the documented defaults, and a
// required variable that is unset or empty is an error. Error messages name
// the variable but never contain its value when the value can be a secret.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		Addr:          DefaultAddr,
		LogLevel:      LogLevelInfo,
		RunMigrations: true,
		LogtoAudience: DefaultAudience,
		CORSOrigins:   []string{DefaultWebOrigin},
		WebOrigin:     DefaultWebOrigin,
	}

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

	required := func(name string) (string, error) {
		value := lookupNonEmpty(lookup, name)
		if value == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return value, nil
	}
	var err error

	if cfg.DatabaseURL, err = required(EnvDatabaseURL); err != nil {
		return Config{}, err
	}
	if value := lookupNonEmpty(lookup, EnvRunMigrations); value != "" {
		run, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be true or false", EnvRunMigrations)
		}
		cfg.RunMigrations = run
	}

	if cfg.LogtoIssuer, err = required(EnvLogtoIssuer); err != nil {
		return Config{}, err
	}
	if cfg.LogtoIssuer, err = httpURL(EnvLogtoIssuer, cfg.LogtoIssuer); err != nil {
		return Config{}, err
	}
	if value := lookupNonEmpty(lookup, EnvLogtoAudience); value != "" {
		cfg.LogtoAudience = value
	}
	if cfg.LogtoManagementEndpoint, err = required(EnvLogtoManagementEndpoint); err != nil {
		return Config{}, err
	}
	if cfg.LogtoManagementEndpoint, err = httpURL(EnvLogtoManagementEndpoint, cfg.LogtoManagementEndpoint); err != nil {
		return Config{}, err
	}
	if cfg.LogtoM2MClientID, err = required(EnvLogtoM2MClientID); err != nil {
		return Config{}, err
	}
	if cfg.LogtoM2MClientSecret, err = required(EnvLogtoM2MClientSecret); err != nil {
		return Config{}, err
	}

	if value := lookupNonEmpty(lookup, EnvCORSOrigins); value != "" {
		cfg.CORSOrigins = nil
		for _, item := range strings.Split(value, ",") {
			item = strings.Trim(item, asciiWhiteSpace)
			if item == "" {
				continue
			}
			origin, err := parseOrigin(EnvCORSOrigins, item)
			if err != nil {
				return Config{}, err
			}
			cfg.CORSOrigins = append(cfg.CORSOrigins, origin)
		}
		if len(cfg.CORSOrigins) == 0 {
			return Config{}, fmt.Errorf("%s must list at least one origin", EnvCORSOrigins)
		}
	}
	if value := lookupNonEmpty(lookup, EnvWebOrigin); value != "" {
		if cfg.WebOrigin, err = parseOrigin(EnvWebOrigin, value); err != nil {
			return Config{}, err
		}
	}

	if cfg.ResendAPIKey, err = required(EnvResendAPIKey); err != nil {
		return Config{}, err
	}
	if cfg.EmailFrom, err = required(EnvEmailFrom); err != nil {
		return Config{}, err
	}
	if _, err := mail.ParseAddress(cfg.EmailFrom); err != nil {
		return Config{}, fmt.Errorf("%s must be an email address such as billing@delino.io or Delino <billing@delino.io>", EnvEmailFrom)
	}

	if value := lookupNonEmpty(lookup, EnvMetricsAddr); value != "" {
		if err := listenaddr.Validate(EnvMetricsAddr, value); err != nil {
			return Config{}, err
		}
		cfg.MetricsAddr = value
	}

	environment, err := required(EnvPaddleEnvironment)
	if err != nil {
		return Config{}, err
	}
	var ok bool
	if cfg.PaddleEnvironment, ok = paddle.ParseEnvironment(environment); !ok {
		return Config{}, fmt.Errorf("%s must be sandbox or production", EnvPaddleEnvironment)
	}
	if cfg.PaddleAPIKey, err = required(EnvPaddleAPIKey); err != nil {
		return Config{}, err
	}
	if cfg.PaddleWebhookSecret, err = required(EnvPaddleWebhookSecret); err != nil {
		return Config{}, err
	}
	if cfg.PaddleSubscriptionPriceID, err = required(EnvPaddlePriceID); err != nil {
		return Config{}, err
	}
	if !paddlePriceID.MatchString(cfg.PaddleSubscriptionPriceID) {
		return Config{}, fmt.Errorf("%s must be a Paddle price ID (pri_...)", EnvPaddlePriceID)
	}
	if cfg.PaddleTopUpProductID, err = required(EnvPaddleTopUpProductID); err != nil {
		return Config{}, err
	}
	if !paddleProductID.MatchString(cfg.PaddleTopUpProductID) {
		return Config{}, fmt.Errorf("%s must be a Paddle product ID (pro_...)", EnvPaddleTopUpProductID)
	}

	if value := lookupNonEmpty(lookup, EnvUsageServiceClients); value != "" {
		if cfg.UsageServiceClients, err = parseUsageServiceClients(value); err != nil {
			return Config{}, err
		}
	}

	return cfg, nil
}

// clientID reports whether value can be a Logto application ID: 1 to 128
// visible ASCII characters other than the separators `,` and `=`. The
// server compares it with the `client_id` claim unchanged.
func clientID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c <= ' ' || c > '~' || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

// parseUsageServiceClients reads `<application ID>=<service>` entries
// separated by commas, such as `abc123=ownsand`. The service is the stable
// lowercase ServiceId identifier. A client that may use several services
// has one entry for each.
func parseUsageServiceClients(value string) (map[string][]accountsv1.ServiceId, error) {
	out := map[string][]accountsv1.ServiceId{}
	for _, item := range strings.Split(value, ",") {
		item = strings.Trim(item, asciiWhiteSpace)
		if item == "" {
			continue
		}
		client, service, ok := strings.Cut(item, "=")
		client, service = strings.Trim(client, asciiWhiteSpace), strings.Trim(service, asciiWhiteSpace)
		if !ok || !clientID(client) {
			return nil, fmt.Errorf("%s must hold entries such as <Logto application ID>=ownsand", EnvUsageServiceClients)
		}
		id, ok := serviceFromKey(service)
		if !ok {
			return nil, fmt.Errorf("%s names an unknown service %q", EnvUsageServiceClients, service)
		}
		if !slices.Contains(out[client], id) {
			out[client] = append(out[client], id)
		}
	}
	return out, nil
}

// serviceFromKey returns the ServiceId of a stable lowercase identifier.
func serviceFromKey(key string) (accountsv1.ServiceId, bool) {
	// Reject anything outside a-z, 0-9 and "_" before the lookup:
	// strings.ToUpper is Unicode-aware and folds characters such as U+017F
	// (long s) to ASCII letters, so the lookup alone would accept "ownſand".
	// billing.ServiceFromKey applies the same rule; the packages keep
	// separate copies because config cannot import billing.
	if !stableKey(key) {
		return accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED, false
	}
	v, ok := accountsv1.ServiceId_value["SERVICE_ID_"+strings.ToUpper(key)]
	if !ok || v == 0 {
		return accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED, false
	}
	return accountsv1.ServiceId(v), true
}

// stableKey reports whether key is non-empty and holds only the bytes a-z,
// 0-9 and "_" that stable identifiers use.
func stableKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// httpURL checks that value is an absolute http or https URL with a host name
// (a port alone such as https://:443 has none), without a query
// or fragment and returns it without a trailing slash. A port, when present,
// must fit in 16 bits: url.Parse accepts any digit string, so a value such as
// https://127.0.0.1:65536 would otherwise only fail on the first request.
func httpURL(name, value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.Contains(value, "#") || strings.Contains(value, "?") {
		return "", fmt.Errorf("%s must be an http or https URL", name)
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return "", fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	return strings.TrimRight(value, "/"), nil
}

// parseOrigin checks that value is a browser origin: an http or https scheme
// and a host name with an optional port, without a path, query or fragment.
// It returns the origin in the form that a browser sends in the Origin header
// (lower-case host, no default port), so the CORS middleware can match the
// header by exact string comparison.
func parseOrigin(name, value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.Contains(value, "#") || strings.Contains(value, "?") {
		return "", fmt.Errorf("%s must hold origins such as https://accounts.delino.io", name)
	}
	origin, ok := weborigin.Canonical(u)
	if !ok {
		return "", fmt.Errorf("%s must hold origins such as https://accounts.delino.io", name)
	}
	return origin, nil
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
