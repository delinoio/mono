// SPDX-License-Identifier: Apache-2.0

// Package cli implements the tide command-line interface and starts the
// tide mcp MCP server, on the tide Go SDK. The owning contract is
// docs/cmds-tide-cli-foundation.md.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/go/tide"
)

// Version is the version of the binary. Release builds set it with
// -ldflags "-X github.com/delinoio/mono/cmds/tide/internal/cli.Version=<v>".
var Version = "dev"

// Environment variables of the CLI.
const (
	EnvAPIKey        = "TIDE_API_KEY"
	EnvAPIURL        = "TIDE_API_URL"
	EnvOrganization  = "TIDE_ORG"
	EnvConfigDir     = "TIDE_CONFIG_DIR"
	EnvLogtoEndpoint = "TIDE_LOGTO_ENDPOINT"
	EnvLogtoAppID    = "TIDE_LOGTO_APP_ID"
	EnvLogtoResource = "TIDE_LOGTO_RESOURCE"
	EnvLogtoScopes   = "TIDE_LOGTO_SCOPES"
	EnvNoColor       = "NO_COLOR"
)

// Exit statuses. The exec statuses follow ownsand sandbox exec.
const (
	ExitOK              = 0
	ExitError           = 1
	ExitUsage           = 2
	ExitExecTimedOut    = 124
	ExitSandboxEnded    = 125
	ExitCommandNotStart = 127
	ExitSignalBase      = 128
	ExitInterrupted     = 130
	ExitDataPath        = 255
)

// Format is the output format.
type Format int

const (
	FormatText Format = iota
	FormatJSON
)

func parseFormat(s string) (Format, error) {
	switch s {
	case "text", "table", "":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	}
	return FormatText, fmt.Errorf("unknown output format %q (want text or json)", s)
}

// Env is the process environment of one CLI run.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads environment variables; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// HTTPClient overrides the HTTP client of the SDK and of Logto (tests).
	HTTPClient *http.Client
	// LoginPollInterval overrides the 5-second device flow polling (tests).
	LoginPollInterval time.Duration
	// Getwd overrides os.Getwd (tests).
	Getwd func() (string, error)
}

// exitError ends a run with a status and an optional message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, args ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, args...)}
}

// globals are the flags that every command accepts.
type globals struct {
	apiURL  string
	org     string
	output  string
	noColor bool
	verbose bool
}

type app struct {
	env    Env
	g      globals
	fmt    Format
	color  bool
	logger *slog.Logger
	client *tide.Client
	// tokens is the Logto token source of client, or nil with an API key.
	// Every caller in one invocation must use it: a second source would
	// refresh with a refresh token that the first one already rotated.
	tokens tide.TokenSource
}

func (a *app) getenv(k string) string {
	v, _ := a.env.LookupEnv(k)
	return v
}

// Run runs one CLI invocation and returns the exit status.
func Run(ctx context.Context, env Env) int {
	if env.LookupEnv == nil {
		env.LookupEnv = os.LookupEnv
	}
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Getwd == nil {
		env.Getwd = os.Getwd
	}
	_, noColor := env.LookupEnv(EnvNoColor)
	a := &app{env: env, color: !noColor, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := a.run(ctx, env.Args)
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			a.errorf("%s", ee.msg)
		}
		if ee.code == ExitUsage {
			fmt.Fprintln(a.env.Stderr, "Run 'tide help' for usage.")
		}
		return ee.code
	}
	a.errorf("%s", describeError(err))
	return ExitError
}

// describeError formats an error for standard error: the Connect code and
// the message.
func describeError(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Sprintf("%s: %s", ce.Code(), ce.Message())
	}
	return err.Error()
}

func (a *app) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.env.Stderr, "%s %s\n", a.paint(ansiRed, "error:"), msg)
}

func (a *app) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.env.Stderr, "%s %s\n", a.paint(ansiYellow, "warning:"), msg)
}

func (a *app) registerGlobals(fs *flag.FlagSet) {
	fs.StringVar(&a.g.apiURL, "api-url", a.g.apiURL, "API origin (env "+EnvAPIURL+")")
	fs.StringVar(&a.g.org, "org", a.g.org, "organization ID (env "+EnvOrganization+"; default: the organization of 'tide org use')")
	fs.StringVar(&a.g.output, "output", a.g.output, "output format: text or json")
	fs.BoolVar(&a.g.noColor, "no-color", a.g.noColor, "disable ANSI color (env "+EnvNoColor+")")
	fs.BoolVar(&a.g.verbose, "verbose", a.g.verbose, "write structured debug logs to standard error")
}

func (a *app) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	a.registerGlobals(fs)
	return fs
}

// parse parses fs and applies the global settings. Flags may follow
// positional arguments; "--" ends flag parsing.
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	return a.parseUntil(fs, args, -1)
}

// parseUntil is parse that stops reading flags after stopAfter positional
// arguments (-1: never), so that `session exec <id> <command> -flag` keeps
// the command's own flags.
func (a *app) parseUntil(fs *flag.FlagSet, args []string, stopAfter int) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				a.printFlags(fs)
				return nil, &exitError{code: ExitOK}
			}
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		consumedDash := endedWithTerminator(fs, args[:len(args)-len(rest)])
		if len(rest) == 0 || consumedDash {
			if consumedDash && len(positional) > 0 && stopAfter >= 0 {
				positional = append(positional, "--")
			}
			positional = append(positional, rest...)
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
		if stopAfter >= 0 && len(positional) >= stopAfter {
			positional = append(positional, args...)
			break
		}
	}
	if err := a.applyGlobals(); err != nil {
		return nil, err
	}
	return positional, nil
}

// endedWithTerminator reports whether the tokens that fs.Parse consumed
// end with a "--" terminator (and not with "--" as a flag value).
func endedWithTerminator(fs *flag.FlagSet, consumed []string) bool {
	for i := 0; i < len(consumed); i++ {
		tok := consumed[i]
		if tok == "--" {
			return true
		}
		name := strings.TrimPrefix(strings.TrimPrefix(tok, "-"), "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
		}
		i++
	}
	return false
}

const rootFlagSetName = "tide"

func (a *app) printFlags(fs *flag.FlagSet) {
	if fs.Name() == rootFlagSetName {
		a.usage(a.env.Stdout)
		return
	}
	fmt.Fprintf(a.env.Stdout, "Usage: tide %s [flags]\n\nFlags:\n", fs.Name())
	fs.SetOutput(a.env.Stdout)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

func (a *app) applyGlobals() error {
	f, err := parseFormat(a.g.output)
	if err != nil {
		return usageErr("%v", err)
	}
	a.fmt = f
	_, noColorEnv := a.env.LookupEnv(EnvNoColor)
	a.color = !a.g.noColor && !noColorEnv
	if a.g.verbose {
		a.logger = slog.New(newLogHandler(a.env.Stderr, a.color, slog.LevelDebug))
	} else {
		a.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return nil
}

func (a *app) run(ctx context.Context, args []string) error {
	a.g.output = "text"
	root := a.newFlags(rootFlagSetName)
	rest, err := a.parseUntil(root, args, 1)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		a.usage(a.env.Stderr)
		return &exitError{code: ExitUsage}
	}
	group := rest[0]
	switch group {
	case "help", "-h", "--help":
		a.usage(a.env.Stdout)
		return nil
	case "version":
		fmt.Fprintln(a.env.Stdout, "tide "+Version)
		return nil
	}
	if cmd, ok := singleCommands[group]; ok {
		return cmd(a, ctx, group, rest[1:])
	}
	subs, ok := commandTable[group]
	if !ok {
		return usageErr("unknown command %q", group)
	}
	if len(rest) < 2 {
		return usageErr("tide %s needs a subcommand: %s", group, strings.Join(subNames(subs), ", "))
	}
	cmd, ok := subs[rest[1]]
	if !ok {
		return usageErr("unknown command %q %q; subcommands: %s", group, rest[1], strings.Join(subNames(subs), ", "))
	}
	return cmd(a, ctx, group+" "+rest[1], rest[2:])
}

type commandFunc func(a *app, ctx context.Context, name string, args []string) error

func subNames(m map[string]commandFunc) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

var (
	singleCommands map[string]commandFunc
	commandTable   map[string]map[string]commandFunc
)

func init() {
	singleCommands = map[string]commandFunc{
		"login":  (*app).login,
		"logout": (*app).logout,
		"status": (*app).status,
		"mcp":    (*app).mcp,
	}
	commandTable = map[string]map[string]commandFunc{
		"org": {
			"list": (*app).orgList, "create": (*app).orgCreate, "use": (*app).orgUse, "delete": (*app).orgDelete,
		},
		"member": {
			"list": (*app).memberList, "invite": (*app).memberInvite, "revoke-invite": (*app).memberRevokeInvite,
			"set-role": (*app).memberSetRole, "remove": (*app).memberRemove, "leave": (*app).memberLeave,
		},
		"ownsand": {
			"link": (*app).ownsandLink, "show": (*app).ownsandShow, "unlink": (*app).ownsandUnlink,
		},
		"github": {
			"install-url": (*app).githubInstallURL, "list": (*app).githubList, "runner-groups": (*app).githubRunnerGroups,
			"set-runner-group": (*app).githubSetRunnerGroup, "unlink": (*app).githubUnlink,
		},
		"integration": {
			"create": (*app).integrationCreate, "list": (*app).integrationList, "get": (*app).integrationGet,
			"update": (*app).integrationUpdate, "delete": (*app).integrationDelete,
		},
		"profile": {
			"create": (*app).profileCreate, "list": (*app).profileList, "get": (*app).profileGet,
			"update": (*app).profileUpdate, "delete": (*app).profileDelete,
		},
		"request": {
			"list": (*app).requestList, "get": (*app).requestGet, "cancel": (*app).requestCancel,
		},
		"runner": {
			"list": (*app).runnerList, "get": (*app).runnerGet, "delete": (*app).runnerDelete,
		},
		"api-key": {
			"create": (*app).apiKeyCreate, "list": (*app).apiKeyList, "revoke": (*app).apiKeyRevoke,
		},
		"session": {
			"profiles": (*app).sessionProfiles, "create": (*app).sessionCreate, "wait": (*app).sessionWait,
			"list": (*app).sessionList, "get": (*app).sessionGet, "exec": (*app).sessionExec,
			"attach": (*app).sessionAttach, "sync": (*app).sessionSync, "download": (*app).sessionDownload,
			"delete": (*app).sessionDelete,
		},
		"org-settings": {
			"get": (*app).orgSettingsGet, "set-dev-session-limit": (*app).orgSettingsSetLimit,
			"clear-dev-session-limit": (*app).orgSettingsClearLimit,
		},
	}
}

func (a *app) usage(w io.Writer) {
	fmt.Fprint(w, `Usage: tide [global flags] <command> [subcommand] [flags] [arguments]

Commands:
  login                      sign in with the Logto device flow
  logout                     delete the stored tokens
  status                     show the queue, the runners and the device pool
  mcp                        serve the MCP server on standard input and output
  version                    print the version
`)
	groups := make([]string, 0, len(commandTable))
	for g := range commandTable {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Fprintf(w, "  %-26s %s\n", g, strings.Join(subNames(commandTable[g]), ", "))
	}
	fmt.Fprint(w, `
Global flags:
  --api-url URL        API origin (env TIDE_API_URL, default https://tide.api.delino.io)
  --org ID             organization (env TIDE_ORG, default: 'tide org use')
  --output text|json   output format (default text)
  --no-color           disable ANSI color (env NO_COLOR)
  --verbose            write structured debug logs to standard error

Authentication: TIDE_API_KEY selects a personal API key, which works only
for 'tide session' and 'tide mcp'; otherwise the CLI uses the tokens that
'tide login' stored. Credentials are read from standard input or from the
file of a --*-file flag, never from command-line arguments.
`)
}

// store returns the credential store.
func (a *app) store() (credstore.Store, error) {
	dir, err := a.configDir()
	if err != nil {
		return credstore.Store{}, err
	}
	return credstore.Store{Dir: dir, Program: "tide"}, nil
}

func (a *app) configDir() (string, error) {
	if dir := a.getenv(EnvConfigDir); dir != "" {
		return dir, nil
	}
	return credstore.DefaultDir("tide")
}

func (a *app) apiURL() string {
	if a.g.apiURL != "" {
		return a.g.apiURL
	}
	if v := a.getenv(EnvAPIURL); v != "" {
		return v
	}
	return tide.DefaultBaseURL
}

// configFile is the CLI settings file next to the credentials. It holds no
// secret.
const configFile = "config.json"

type settings struct {
	OrganizationID string `json:"organization_id,omitempty"`
}

func (a *app) loadSettings() (settings, error) {
	dir, err := a.configDir()
	if err != nil {
		return settings{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return settings{}, nil
	}
	if err != nil {
		return settings{}, err
	}
	var s settings
	if err := json.Unmarshal(b, &s); err != nil {
		return settings{}, fmt.Errorf("the settings file %s is not valid JSON; run 'tide org use' again", filepath.Join(dir, configFile))
	}
	return s, nil
}

func (a *app) saveSettings(s settings) error {
	dir, err := a.configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, configFile))
}

// orgID returns the organization of --org, TIDE_ORG or 'tide org use'.
func (a *app) orgID() (string, error) {
	if a.g.org != "" {
		return a.g.org, nil
	}
	if v := a.getenv(EnvOrganization); v != "" {
		return v, nil
	}
	s, err := a.loadSettings()
	if err != nil {
		return "", err
	}
	return s.OrganizationID, nil
}

// requireOrg fails before any request when no organization is selected.
func (a *app) requireOrg() (string, error) {
	org, err := a.orgID()
	if err != nil {
		return "", err
	}
	if org == "" {
		return "", usageErr("this command needs an organization: pass --org, set %s or run 'tide org use <organization-id>'", EnvOrganization)
	}
	return org, nil
}

// sdk returns the SDK client. TIDE_API_KEY takes precedence over the
// stored Logto tokens.
func (a *app) sdk() (*tide.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	opts := tide.Options{BaseURL: a.apiURL(), HTTPClient: a.env.HTTPClient, Logger: a.logger}
	if key := a.getenv(EnvAPIKey); key != "" {
		opts.APIKey = key
		a.logger.Debug("tide credential selected", "kind", "api_key")
	} else {
		src, err := a.tokenSource()
		if err != nil {
			return nil, err
		}
		opts.TokenSource = src
		a.tokens = src
		a.logger.Debug("tide credential selected", "kind", "logto")
	}
	c, err := tide.New(opts)
	if err != nil {
		return nil, usageErr("%v", err)
	}
	a.client = c
	return c, nil
}

// orgSetup prepares a command that needs an organization and a client.
func (a *app) orgSetup() (string, *tide.Client, error) {
	org, err := a.requireOrg()
	if err != nil {
		return "", nil, err
	}
	c, err := a.sdk()
	if err != nil {
		return "", nil, err
	}
	return org, c, nil
}
