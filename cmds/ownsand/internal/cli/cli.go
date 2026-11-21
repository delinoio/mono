// SPDX-License-Identifier: Apache-2.0

// Package cli implements the ownsand command-line interface on the Go SDK.
// The owning contract is docs/cmds-ownsand-cli-foundation.md.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand/internal/credstore"
	ownsand "github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/ownsand/validate"
)

// Environment variables of the CLI. OWNSAND_API_KEY and OWNSAND_ORG are the
// SDK variables.
const (
	EnvAPIURL        = "OWNSAND_API_URL"
	EnvTransport     = "OWNSAND_TRANSPORT"
	EnvConfigDir     = "OWNSAND_CONFIG_DIR"
	EnvLogtoEndpoint = "OWNSAND_LOGTO_ENDPOINT"
	EnvLogtoAppID    = "OWNSAND_LOGTO_APP_ID"
	EnvLogtoResource = "OWNSAND_LOGTO_RESOURCE"
	EnvLogtoScopes   = "OWNSAND_LOGTO_SCOPES"
	EnvNoColor       = "NO_COLOR"
)

// Exit statuses of the CLI. The exec statuses follow the contract.
const (
	ExitOK              = 0
	ExitError           = 1
	ExitUsage           = 2
	ExitExecTimedOut    = 124
	ExitSandboxEnded    = 125
	ExitCommandNotStart = 127
	ExitSignalBase      = 128
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
	case "text", "":
		return FormatText, nil
	case "json":
		return FormatJSON, nil
	}
	return FormatText, fmt.Errorf("unknown format %q (want text or json)", s)
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
	apiURL    string
	org       string
	format    string
	noColor   bool
	verbose   bool
	transport string
}

type app struct {
	env    Env
	g      globals
	fmt    Format
	color  bool
	logger *slog.Logger
	client *ownsand.Client
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
	a := &app{env: env}
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
			fmt.Fprintln(a.env.Stderr, "Run 'ownsand help' for usage.")
		}
		return ee.code
	}
	a.errorf("%s", describeError(err))
	return ExitError
}

// describeError formats an error for standard error.
func describeError(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return fmt.Sprintf("%s: %s", ce.Code(), ce.Message())
	}
	return err.Error()
}

func (a *app) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if a.color {
		fmt.Fprintf(a.env.Stderr, "%serror:%s %s\n", ansiRed, ansiReset, msg)
		return
	}
	fmt.Fprintf(a.env.Stderr, "error: %s\n", msg)
}

func (a *app) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if a.color {
		fmt.Fprintf(a.env.Stderr, "%swarning:%s %s\n", ansiYellow, ansiReset, msg)
		return
	}
	fmt.Fprintf(a.env.Stderr, "warning: %s\n", msg)
}

// registerGlobals adds the global flags to fs.
func (a *app) registerGlobals(fs *flag.FlagSet) {
	fs.StringVar(&a.g.apiURL, "api-url", a.g.apiURL, "API origin (env "+EnvAPIURL+")")
	fs.StringVar(&a.g.org, "org", a.g.org, "organization ID (env "+ownsand.EnvOrganization+")")
	fs.StringVar(&a.g.format, "format", a.g.format, "output format: text or json")
	fs.BoolVar(&a.g.noColor, "no-color", a.g.noColor, "disable ANSI color (env "+EnvNoColor+")")
	fs.BoolVar(&a.g.verbose, "verbose", a.g.verbose, "write structured logs to standard error")
	fs.StringVar(&a.g.transport, "transport", a.g.transport, "data path transport: auto, peer-to-peer or relay (env "+EnvTransport+")")
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
// arguments (-1: never), so that `sandbox exec <id> <command> -flag` keeps
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
		// fs.Parse consumes a "--" terminator; detect it by the length.
		consumedDash := len(args) > len(rest) && len(args)-len(rest) >= 1 && args[len(args)-len(rest)-1] == "--"
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

func (a *app) printFlags(fs *flag.FlagSet) {
	fmt.Fprintf(a.env.Stdout, "Usage: ownsand %s [flags]\n\nFlags:\n", fs.Name())
	fs.SetOutput(a.env.Stdout)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

func (a *app) applyGlobals() error {
	f, err := parseFormat(a.g.format)
	if err != nil {
		return usageErr("%v", err)
	}
	a.fmt = f
	_, noColorEnv := a.env.LookupEnv(EnvNoColor)
	a.color = !a.g.noColor && !noColorEnv
	if a.g.verbose {
		a.logger = slog.New(newLogHandler(a.env.Stderr, a.color))
	} else {
		a.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return nil
}

func (a *app) run(ctx context.Context, args []string) error {
	a.g.format = "text"
	root := a.newFlags("ownsand")
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
	case "login":
		return a.login(ctx, rest[1:])
	case "logout":
		return a.logout(rest[1:])
	}
	subs, ok := commandTable[group]
	if !ok {
		return usageErr("unknown command %q", group)
	}
	if len(rest) < 2 {
		return usageErr("ownsand %s needs a subcommand: %s", group, strings.Join(subNames(subs), ", "))
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

// commandTable lists every command of the CLI contract.
var commandTable map[string]map[string]commandFunc

func init() {
	commandTable = map[string]map[string]commandFunc{
		"org": {
			"list": (*app).orgList, "create": (*app).orgCreate, "get": (*app).orgGet,
			"delete": (*app).orgDelete, "transfer": (*app).orgTransfer,
		},
		"member": {
			"list": (*app).memberList, "invite": (*app).memberInvite, "invitations": (*app).memberInvitations,
			"revoke-invitation": (*app).memberRevokeInvitation, "accept": (*app).memberAccept,
			"set-role": (*app).memberSetRole, "remove": (*app).memberRemove,
		},
		"device": {
			"list": (*app).deviceList, "get": (*app).deviceGet, "set-limits": (*app).deviceSetLimits, "remove": (*app).deviceRemove,
		},
		"enrollment-token": {
			"create": (*app).enrollmentCreate, "list": (*app).enrollmentList, "revoke": (*app).enrollmentRevoke,
		},
		"api-key": {
			"create": (*app).apiKeyCreate, "list": (*app).apiKeyList, "revoke": (*app).apiKeyRevoke,
		},
		"agent-version": {
			"get": (*app).agentVersionGet, "set": (*app).agentVersionSet,
		},
		"sandbox": {
			"create": (*app).sandboxCreate, "get": (*app).sandboxGet, "list": (*app).sandboxList,
			"delete": (*app).sandboxDelete, "pause": (*app).sandboxPause, "resume": (*app).sandboxResume,
			"exec": (*app).sandboxExec,
		},
		"file": {
			"read": (*app).fileRead, "write": (*app).fileWrite,
		},
		"snapshot": {
			"create": (*app).snapshotCreate, "get": (*app).snapshotGet, "list": (*app).snapshotList,
			"delete": (*app).snapshotDelete, "export": (*app).snapshotExport,
		},
		"template": {
			"create": (*app).templateCreate, "list": (*app).templateList, "delete": (*app).templateDelete,
		},
	}
}

func (a *app) usage(w io.Writer) {
	fmt.Fprint(w, `Usage: ownsand [global flags] <command> <subcommand> [flags] [arguments]

Commands:
  login                      sign in with the Logto device flow
  logout                     delete the stored tokens
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
  --api-url URL        API origin (env OWNSAND_API_URL, default https://ownsand.api.delino.io)
  --org ID             organization (env OWNSAND_ORG)
  --format text|json   output format (default text)
  --transport MODE     data path: auto, peer-to-peer or relay (env OWNSAND_TRANSPORT)
  --no-color           disable ANSI color (env NO_COLOR)
  --verbose            write structured logs to standard error

Authentication: OWNSAND_API_KEY selects an organization API key; otherwise
the CLI uses the tokens that 'ownsand login' stored.
`)
}

// store returns the credential store.
func (a *app) store() (credstore.Store, error) {
	if dir := a.getenv(EnvConfigDir); dir != "" {
		return credstore.Store{Dir: dir}, nil
	}
	dir, err := credstore.DefaultDir()
	if err != nil {
		return credstore.Store{}, err
	}
	return credstore.Store{Dir: dir}, nil
}

func (a *app) apiURL() string {
	if a.g.apiURL != "" {
		return a.g.apiURL
	}
	if v := a.getenv(EnvAPIURL); v != "" {
		return v
	}
	return ownsand.DefaultBaseURL
}

// orgID returns the organization from --org or OWNSAND_ORG.
func (a *app) orgID() string {
	if a.g.org != "" {
		return a.g.org
	}
	return a.getenv(ownsand.EnvOrganization)
}

// requireOrg fails before any request when no organization is set.
func (a *app) requireOrg() (string, error) {
	org := a.orgID()
	if org == "" {
		return "", usageErr("this command needs an organization: pass --org or set %s", ownsand.EnvOrganization)
	}
	if err := validate.UUIDv7("organization_id", org); err != nil {
		return "", usageErr("--org: %v", err)
	}
	return org, nil
}

// sdk returns the SDK client. OWNSAND_API_KEY takes precedence over stored
// Logto tokens.
func (a *app) sdk() (*ownsand.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	transportName := a.g.transport
	if transportName == "" {
		transportName = a.getenv(EnvTransport)
	}
	tr, err := ownsand.ParseTransport(transportName)
	if err != nil {
		return nil, usageErr("%v", err)
	}
	opts := ownsand.Options{
		BaseURL:        a.apiURL(),
		OrganizationID: a.orgID(),
		HTTPClient:     a.env.HTTPClient,
		Logger:         a.logger,
		Transport:      tr,
		LookupEnv:      a.env.LookupEnv,
	}
	if key := a.getenv(ownsand.EnvAPIKey); key != "" {
		opts.APIKey = key
		a.logger.Debug("ownsand credential selected", "kind", "api_key")
	} else {
		src, err := a.tokenSource()
		if err != nil {
			return nil, err
		}
		opts.TokenSource = src
		a.logger.Debug("ownsand credential selected", "kind", "logto")
	}
	c, err := ownsand.New(opts)
	if err != nil {
		return nil, err
	}
	a.client = c
	return c, nil
}
