package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Exit statuses of the CLI itself. sandbox exec has its own rules.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitDataPath = 255
)

// Environment variables of the CLI.
const (
	envOrigin    = "OWNSAND_API_ORIGIN"
	envNoColor   = "NO_COLOR"
	envConfigDir = "OWNSAND_CONFIG_DIR"
)

// outputFormat is the --format value.
type outputFormat int

const (
	formatText outputFormat = iota
	formatJSON
)

type stdio struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

// globals are the flags that every command accepts.
type globals struct {
	org       string
	format    string
	origin    string
	transport string
	noColor   bool
	verbose   bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.org, "org", g.org, "organization ID (default $OWNSAND_ORG)")
	fs.StringVar(&g.format, "format", g.format, "output format: text or json")
	fs.StringVar(&g.origin, "api-origin", g.origin, "API origin (default $OWNSAND_API_ORIGIN or "+ownsand.DefaultOrigin+")")
	fs.StringVar(&g.transport, "transport", g.transport, "data path transport: auto, relay or peer-to-peer (for tests)")
	fs.BoolVar(&g.noColor, "no-color", g.noColor, "turn off ANSI color (also $NO_COLOR)")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "write structured logs to standard error")
}

// app is one CLI invocation.
type app struct {
	ctx    context.Context
	io     *stdio
	getenv func(string) string
	g      globals
	fmt    outputFormat
	color  bool
	log    *slog.Logger
	// clk overrides the SDK clock in tests.
	clk clock.Clock
	// now is the wall clock of credential expiry; tests replace it.
	now func() time.Time
}

// testClock lets tests run the SDK on the fake server's clock.
var testClock clock.Clock

// usageError is a command-line usage error (exit status 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// exitCodeError ends the command with a specific exit status after the
// command already reported its outcome.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func run(ctx context.Context, args []string, sio *stdio, getenv func(string) string) int {
	a := &app{ctx: ctx, io: sio, getenv: getenv, g: globals{format: "text", transport: "auto"}, clk: testClock, now: time.Now}
	fs := flag.NewFlagSet("ownsand", flag.ContinueOnError)
	fs.SetOutput(sio.err)
	a.g.register(fs)
	fs.Usage = func() { fmt.Fprint(sio.err, topUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(sio.err, topUsage)
		return exitUsage
	}
	err := a.dispatch(rest)
	return a.finish(err)
}

func (a *app) finish(err error) int {
	if err == nil {
		return exitOK
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		a.errorf("%s", ue.msg)
		return exitUsage
	}
	if errors.Is(err, ownsand.ErrInvalidArgument) {
		a.errorf("%v", err)
		return exitUsage
	}
	a.errorf("%v", err)
	return exitError
}

// setup applies the global flags after the command's flags were parsed.
func (a *app) setup() error {
	switch a.g.format {
	case "text":
		a.fmt = formatText
	case "json":
		a.fmt = formatJSON
	default:
		return usagef("--format must be text or json")
	}
	a.color = !a.g.noColor && a.getenv(envNoColor) == ""
	level := slog.LevelError + 4
	if a.g.verbose {
		level = slog.LevelDebug
	}
	a.log = slog.New(slog.NewTextHandler(a.io.err, &slog.HandlerOptions{Level: level}))
	return nil
}

const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiRed   = "\x1b[31m"
	ansiYel   = "\x1b[33m"
	ansiCyan  = "\x1b[36m"
)

func (a *app) paint(code, s string) string {
	if !a.color {
		return s
	}
	return code + s + ansiReset
}

func (a *app) errorf(format string, args ...any) {
	fmt.Fprintf(a.io.err, "%s %s\n", a.paint(ansiRed+ansiBold, "error:"), fmt.Sprintf(format, args...))
}

func (a *app) warnf(format string, args ...any) {
	fmt.Fprintf(a.io.err, "%s %s\n", a.paint(ansiYel+ansiBold, "warning:"), fmt.Sprintf(format, args...))
}

var marshal = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}

// printMessages prints proto messages: one JSON object (or array for a
// list) in JSON mode, or field lines in text mode.
func (a *app) printMessages(list bool, nextPage string, msgs ...proto.Message) error {
	return a.printTo(a.io.out, list, nextPage, msgs...)
}

func (a *app) printTo(w io.Writer, list bool, nextPage string, msgs ...proto.Message) error {
	if a.fmt == formatJSON {
		items := make([]json.RawMessage, 0, len(msgs))
		for _, m := range msgs {
			b, err := marshal.Marshal(m)
			if err != nil {
				return err
			}
			items = append(items, b)
		}
		var v any
		switch {
		case list:
			obj := map[string]any{"items": items}
			if nextPage != "" {
				obj["next_page_token"] = nextPage
			}
			v = obj
		case len(items) == 1:
			v = items[0]
		default:
			v = items
		}
		return json.NewEncoder(w).Encode(v)
	}
	for i, m := range msgs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := a.printText(w, m); err != nil {
			return err
		}
	}
	if list && len(msgs) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	if nextPage != "" {
		fmt.Fprintf(w, "\n%s %s\n", a.paint(ansiCyan, "next page token:"), nextPage)
	}
	return nil
}

// printText prints the top-level fields of m, one per line, in field order
// of their JSON names.
func (a *app) printText(w io.Writer, m proto.Message) error {
	b, err := marshal.Marshal(m)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := string(fields[k])
		if s, err := unquote(fields[k]); err == nil {
			v = s
		}
		fmt.Fprintf(w, "%s %s\n", a.paint(ansiCyan, k+":"), v)
	}
	return nil
}

func unquote(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}

// printSecret prints a new secret once: in text mode the secret alone on
// standard output and the other fields on standard error, in JSON mode one
// object with both on standard output.
func (a *app) printSecret(secret, key string, m proto.Message) error {
	if a.fmt == formatJSON {
		b, err := marshal.Marshal(m)
		if err != nil {
			return err
		}
		return json.NewEncoder(a.io.out).Encode(map[string]any{"secret": secret, key: json.RawMessage(b)})
	}
	if err := a.printTo(a.io.err, false, "", m); err != nil {
		return err
	}
	fmt.Fprintln(a.io.err, a.paint(ansiYel, "The secret is printed once on standard output; store it now."))
	_, err := fmt.Fprintln(a.io.out, secret)
	return err
}

// parseKV parses repeated key=value flags.
type kvFlag map[string]string

func (k kvFlag) String() string { return "" }

func (k kvFlag) Set(v string) error {
	key, val, ok := strings.Cut(v, "=")
	if !ok || key == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	k[key] = val
	return nil
}
