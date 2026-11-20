// Package cli implements the ownsand-agent commands: enroll, run, status,
// reset and image build.
package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	goruntime "runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/hostinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Env holds the process environment of a command, so tests can replace it.
type Env struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Version string
	// NewRuntime overrides runtime.New (tests).
	NewRuntime func(runtime.Config) (runtime.Runtime, error)
	// IsTerminal reports whether stdin is a terminal.
	IsTerminal func() bool
	// Configure lets the caller adjust the agent configuration (the data
	// path layer installs itself here).
	Configure func(*agent.Config)
}

// Main runs a command and returns the exit code.
func Main(env Env) int {
	if env.NewRuntime == nil {
		env.NewRuntime = runtime.New
	}
	global := flag.NewFlagSet("ownsand-agent", flag.ContinueOnError)
	global.SetOutput(env.Stderr)
	stateDir := global.String("state-dir", state.DefaultDir(), "agent state directory")
	noColor := global.Bool("no-color", false, "disable ANSI color in command output")
	global.Usage = func() { usage(env.Stderr) }
	if err := global.Parse(env.Args); err != nil {
		return 2
	}
	p := &Printer{w: env.Stdout, color: !*noColor && env.Getenv("NO_COLOR") == ""}
	pe := &Printer{w: env.Stderr, color: p.color}
	args := global.Args()
	if len(args) == 0 {
		usage(env.Stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "enroll":
		err = enroll(env, p, *stateDir, args[1:])
	case "run":
		err = run(env, *stateDir, args[1:])
	case "status":
		err = status(env, p, *stateDir, args[1:])
	case "reset":
		err = reset(env, p, *stateDir, args[1:])
	case "image":
		err = image(args[1:])
	case "version":
		_, _ = fmt.Fprintln(env.Stdout, env.Version)
	case "help", "-h", "--help":
		usage(env.Stdout)
	default:
		pe.Error(fmt.Sprintf("unknown command %q", args[0]))
		usage(env.Stderr)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			pe.Error(err.Error())
			return 2
		}
		pe.Error(err.Error())
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: ownsand-agent [--state-dir DIR] [--no-color] <command> [flags]

Commands:
  enroll [--origin URL] [--token-file FILE]   Enroll the device. The token is read from
                                              standard input unless --token-file is set.
  run                                         Run the service (started by the service manager).
  status                                      Print the enrollment, connection, capability,
                                              sandbox and update state.
  reset --yes                                 Delete all local sandboxes, snapshots, images
                                              and the device key without the control plane.
  image build [--iso FILE] [--ipsw FILE]      Build the macOS or Windows base image locally.
  version                                     Print the agent version.

Color is on by default; --no-color or NO_COLOR turns it off.
`)
}

// Printer writes CLI output with optional ANSI color.
type Printer struct {
	w     io.Writer
	color bool
}

func (p *Printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Success prints a green line.
func (p *Printer) Success(s string) { _, _ = fmt.Fprintln(p.w, p.paint("32", s)) }

// Warn prints a yellow line.
func (p *Printer) Warn(s string) { _, _ = fmt.Fprintln(p.w, p.paint("33", "warning: "+s)) }

// Error prints a red line.
func (p *Printer) Error(s string) { _, _ = fmt.Fprintln(p.w, p.paint("31", "error: "+s)) }

// Field prints a bold label and a value.
func (p *Printer) Field(label, value string) {
	_, _ = fmt.Fprintf(p.w, "%s %s\n", p.paint("1", label+":"), value)
}

// Heading prints a cyan heading.
func (p *Printer) Heading(s string) { _, _ = fmt.Fprintln(p.w, p.paint("36;1", s)) }

func enroll(env Env, p *Printer, dirPath string, args []string) error {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	origin := flags.String("origin", state.DefaultOrigin, "control plane origin")
	tokenFile := flags.String("token-file", "", "file that holds the enrollment token (default: standard input)")
	if err := flags.Parse(args); err != nil {
		return usageError{"invalid enroll flags"}
	}
	if flags.NArg() > 0 {
		// The token is never accepted as an argument: other local users can
		// read process arguments.
		return usageError{"enroll takes no arguments; pass the token on standard input or with --token-file"}
	}
	dir, err := state.Open(dirPath)
	if err != nil {
		return err
	}
	id, err := dir.LoadIdentity()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read identity: %w", err)
	}
	if id.Enrolled() {
		return fmt.Errorf("the device is already enrolled as %s; run `ownsand-agent reset` first", id.DeviceID)
	}
	token, err := readToken(env, *tokenFile)
	if err != nil {
		return err
	}
	// Reuse the key of a pending enrollment for the same origin, so a retry
	// after a lost Enroll response returns the same device.
	if id == nil || id.Origin != *origin || len(id.PrivateKey) != ed25519.PrivateKeySize {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		id = &state.Identity{Origin: *origin, PrivateKey: priv}
		if err := dir.SaveIdentity(id); err != nil {
			return fmt.Errorf("save the device key: %w", err)
		}
	}
	rt, err := env.NewRuntime(runtime.Config{StateDir: dirPath, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	defer rt.Close()
	rc := rt.Capabilities(context.Background())
	cp, err := controlplane.New(*origin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := cp.Agent.Enroll(ctx, connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: token,
		PublicKey:       id.PublicKey(),
		Capabilities: &ownsandv1.DeviceCapabilities{
			HostOs:           hostinfo.HostOS(),
			Arch:             hostinfo.Arch(),
			AgentVersion:     env.Version,
			LogicalCpuCount:  hostinfo.LogicalCPUs(),
			MemoryMib:        hostinfo.MemoryMiB(),
			FreeDiskBytes:    hostinfo.FreeDiskBytes(dirPath),
			GuestOses:        rc.GuestOSes,
			BuiltBaseImages:  rc.BuiltBaseImages,
			HypervisorChecks: rc.HypervisorChecks,
		},
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			return errors.New("enrollment failed: the token is expired, revoked or already used")
		}
		return fmt.Errorf("enrollment failed: %w", err)
	}
	id.DeviceID = res.Msg.GetDeviceId()
	id.OrganizationID = res.Msg.GetOrganizationId()
	id.EnrolledAt = time.Now().UTC()
	if err := dir.SaveIdentity(id); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	p.Success("Device enrolled.")
	p.Field("Device ID", id.DeviceID)
	p.Field("Organization ID", id.OrganizationID)
	p.Field("Control plane", id.Origin)
	return nil
}

func readToken(env Env, path string) (string, error) {
	var raw []byte
	var err error
	if path != "" {
		raw, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
	} else {
		if env.IsTerminal != nil && env.IsTerminal() {
			_, _ = fmt.Fprint(env.Stderr, "Enrollment token: ")
		}
		line, err := bufio.NewReader(env.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read token: %w", err)
		}
		raw = []byte(line)
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, "ose_") {
		return "", errors.New("the enrollment token must start with ose_")
	}
	return token, nil
}

func run(env Env, dirPath string, args []string) error {
	if len(args) > 0 {
		return usageError{"run takes no arguments"}
	}
	// The service writes JSON logs without color to standard output, which
	// journald (Linux), the unified log (macOS) or the service wrapper
	// (Windows) collects.
	log := slog.New(slog.NewJSONHandler(env.Stdout, nil))
	dir, err := state.Open(dirPath)
	if err != nil {
		return err
	}
	id, err := dir.LoadIdentity()
	if err != nil || !id.Enrolled() {
		return errors.New("the device is not enrolled; run `ownsand-agent enroll` first")
	}
	rt, err := env.NewRuntime(runtime.Config{StateDir: dirPath, Logger: log})
	if err != nil {
		return fmt.Errorf("start runtime: %w", err)
	}
	defer rt.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := agent.Config{Dir: dir, Identity: id, Version: env.Version, Runtime: rt, Logger: log}
	if env.Configure != nil {
		env.Configure(&cfg)
	}
	a, err := agent.New(ctx, cfg)
	if err != nil {
		return err
	}
	err = a.Run(ctx)
	switch {
	case errors.Is(err, agent.ErrRemoved):
		log.Info("agent stopped after device removal")
		return nil
	case errors.Is(err, agent.ErrRestartForUpdate):
		log.Info("agent exiting to start the installed update")
		return nil
	}
	return err
}

func status(env Env, p *Printer, dirPath string, args []string) error {
	if len(args) > 0 {
		return usageError{"status takes no arguments"}
	}
	dir := &state.Dir{Path: dirPath}
	id, err := dir.LoadIdentity()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read identity: %w", err)
	}
	p.Heading("Enrollment")
	if !id.Enrolled() {
		p.Field("State", p.paint("33", "not enrolled"))
		return nil
	}
	p.Field("State", p.paint("32", "enrolled"))
	p.Field("Device ID", id.DeviceID)
	p.Field("Organization ID", id.OrganizationID)
	p.Field("Control plane", id.Origin)
	if rem, _ := dir.LoadRemoval(); rem != nil {
		p.Warn("a removal notice was received; the agent finishes the removal at its next start")
	}
	st, err := dir.LoadStatus()
	if err != nil {
		return err
	}
	p.Heading("Connection")
	if st == nil {
		p.Field("State", "unknown (the service has not run yet)")
		return nil
	}
	conn := string(st.Connection)
	if time.Since(st.UpdatedAt) > time.Minute {
		conn += " (stale: the service last wrote its status at " + st.UpdatedAt.Local().Format(time.RFC3339) + ")"
	}
	color := "33"
	if st.Connection == state.ConnectionConnected {
		color = "32"
	}
	p.Field("State", p.paint(color, conn))
	if !st.LastConnectedAt.IsZero() {
		p.Field("Last connected", st.LastConnectedAt.Local().Format(time.RFC3339))
	}
	if st.LastError != "" {
		p.Field("Last error", st.LastError)
	}
	p.Heading("Capabilities")
	if c := st.Capabilities; c != nil {
		p.Field("Host", c.HostOS+" "+c.Arch)
		p.Field("CPUs", fmt.Sprint(c.LogicalCPUs))
		p.Field("Memory", fmt.Sprintf("%d MiB", c.MemoryMiB))
		p.Field("Free disk", fmt.Sprintf("%.1f GiB", float64(c.FreeDiskBytes)/(1<<30)))
	}
	p.Field("Guest OSes", joinEnums(st.GuestOSes))
	p.Field("Built base images", joinEnums(st.BuiltBaseImages))
	keys := make([]string, 0, len(st.HypervisorChecks))
	for k := range st.HypervisorChecks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.Field("Check "+k, st.HypervisorChecks[k])
	}
	if l := st.Limits; l != nil {
		p.Field("Device limits", fmt.Sprintf("%d vCPU, %d MiB, %d GiB", l.GetVcpuCount(), l.GetMemoryMib(), l.GetDiskGib()))
	}
	p.Heading("Sandboxes")
	if len(st.SandboxStates) == 0 {
		p.Field("Total", "0")
	}
	states := make([]string, 0, len(st.SandboxStates))
	for k := range st.SandboxStates {
		states = append(states, k)
	}
	sort.Strings(states)
	for _, k := range states {
		p.Field(strings.TrimPrefix(k, "SANDBOX_STATE_"), fmt.Sprint(st.SandboxStates[k]))
	}
	p.Heading("Update")
	p.Field("Agent version", st.AgentVersion)
	p.Field("Update state", strings.TrimPrefix(st.UpdateState.String(), "AGENT_UPDATE_STATE_"))
	if st.TargetVersion != "" {
		p.Field("Target version", st.TargetVersion)
	}
	return nil
}

func joinEnums[T fmt.Stringer](v []T) string {
	if len(v) == 0 {
		return "none"
	}
	out := make([]string, len(v))
	for i, x := range v {
		s := x.String()
		if i := strings.LastIndex(s, "_"); i >= 0 {
			s = s[i+1:]
		}
		out[i] = s
	}
	return strings.Join(out, ", ")
}

func reset(env Env, p *Printer, dirPath string, args []string) error {
	flags := flag.NewFlagSet("reset", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	yes := flags.Bool("yes", false, "confirm the deletion")
	if err := flags.Parse(args); err != nil {
		return usageError{"invalid reset flags"}
	}
	if !*yes {
		return usageError{"reset deletes every local sandbox, snapshot, image and the device key; pass --yes to confirm"}
	}
	dir, err := state.Open(dirPath)
	if err != nil {
		return err
	}
	rt, err := env.NewRuntime(runtime.Config{StateDir: dirPath, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	if err := rt.DeleteAll(context.Background()); err != nil {
		_ = rt.Close()
		return fmt.Errorf("delete VMs: %w", err)
	}
	_ = rt.Close()
	if err := dir.Reset(); err != nil {
		return fmt.Errorf("delete local state: %w", err)
	}
	p.Success("Local sandboxes, snapshots, images and the device key were deleted.")
	return nil
}

func image(args []string) error {
	if len(args) == 0 || args[0] != "build" {
		return usageError{"usage: ownsand-agent image build [--iso FILE] [--ipsw FILE]"}
	}
	switch goruntime.GOOS {
	case "linux":
		return errors.New("image build is not supported on Linux: the LINUX base image is built and signed by the OwnSand release process, and Linux hosts run only Linux guests; image build builds macOS (from an IPSW) and Windows (from an ISO) base images on those hosts")
	default:
		return connect.NewError(connect.CodeUnimplemented, errors.New("image build is not implemented in this agent version: macOS and Windows guests are not supported yet"))
	}
}
