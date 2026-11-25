// SPDX-License-Identifier: FSL-1.1-ALv2

// Command ownsand-agent is the OwnSand device agent. See
// docs/cmds-ownsand-agent-foundation.md.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtimes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/version"
)

// Exit codes.
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitNotEnrolled = service.ExitNotEnrolled
)

const usage = `Usage: ownsand-agent <command> [flags]

Commands:
  enroll         Enroll the device with a one-time enrollment token (stdin or --token-file).
  run            Run the agent service.
  status         Print the enrollment, connection, sandbox and update state.
  image build    Build the macOS or Windows base image locally (needs root
                 or an Administrator): --os macos|windows (default: the host
                 OS), --ipsw <file> (macOS; default: the latest supported
                 restore image), --iso <file> and --edition <index|name>
                 (Windows), --disk-gib, --cpus, --memory-mib, --guest-binary.
  reset --yes    Delete all local sandboxes, snapshots, images and the device key.
  service print  Print the service definition of this host OS.
  service install
                 Install the service definition of this host OS.
  service migrate
                 Move an existing Windows service to the recovery launcher.
  version        Print the agent version.

Flags common to every command:
  --state-dir    State directory (env OWNSAND_AGENT_STATE_DIR).
  --no-color     Turn off ANSI color (also NO_COLOR).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// colors prints ANSI colors unless --no-color or NO_COLOR is set.
type colors struct{ on bool }

func (c colors) wrap(code, s string) string {
	if !c.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (c colors) ok(s string) string   { return c.wrap("32", s) }
func (c colors) bad(s string) string  { return c.wrap("31", s) }
func (c colors) bold(s string) string { return c.wrap("1", s) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	if cmd == "image" || cmd == "service" {
		if len(rest) == 0 {
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
	}
	launch := cmd == "service launch" && len(rest) != 0 && rest[0] == "run"
	if launch {
		cmd, rest = "run", rest[1:]
	}
	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		if cmd == "run" {
			// The Windows service must report the error to the service
			// control manager (see runConfigError).
			return runConfigError(err, stderr)
		}
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitUsage
	}
	fs := flag.NewFlagSet("ownsand-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "state directory")
	noColor := fs.Bool("no-color", false, "turn off ANSI color")
	apiURL := fs.String("api-url", "", "control plane origin (enroll; env "+config.EnvAPIURL+")")
	tokenFile := fs.String("token-file", "", "file that holds the enrollment token (enroll)")
	name := fs.String("name", "", "device name (enroll; default: host name)")
	yes := fs.Bool("yes", false, "confirm the reset")
	var ib imageBuildFlags
	fs.StringVar(&ib.os, "os", "", "guest OS of the image (image build: macos or windows)")
	fs.StringVar(&ib.ipsw, "ipsw", "", "macOS restore image (image build)")
	fs.StringVar(&ib.iso, "iso", "", "Windows installation ISO (image build)")
	fs.StringVar(&ib.edition, "edition", "", "Windows image index or name in the ISO (image build)")
	fs.Int64Var(&ib.diskGiB, "disk-gib", 0, "image disk size in GiB (image build)")
	fs.IntVar(&ib.cpus, "cpus", 0, "vCPUs of the build VM (image build)")
	fs.Int64Var(&ib.memoryMiB, "memory-mib", 0, "memory of the build VM in MiB (image build)")
	fs.StringVar(&ib.guestBinary, "guest-binary", "", "guest helper executable for the guest OS (image build)")
	// Hidden flag (not in the usage text): the launchd daemon definition
	// passes it so that `run` maps its exit codes to the launchd KeepAlive
	// predicate. See service.LaunchdExitCode.
	manager := fs.String(strings.TrimPrefix(service.ManagerFlag, "--"), "", "service manager that starts run (launchd)")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	switch service.Manager(*manager) {
	case "", service.ManagerLaunchd:
	default:
		fmt.Fprintf(stderr, "ownsand-agent: unknown service manager %q\n", *manager)
		return exitUsage
	}
	// The service definition carries the state directory only when the flag
	// is set: a directory that comes from the environment or the host default
	// keeps the behavior of the environment-only service definition. The
	// service runs in another working directory, so the path is made absolute.
	var explicitStateDir string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "state-dir" {
			explicitStateDir = cfg.StateDir
		}
	})
	if explicitStateDir != "" && (cmd == "service print" || cmd == "service install") {
		abs, err := filepath.Abs(explicitStateDir)
		if err != nil {
			fmt.Fprintln(stderr, "ownsand-agent:", err)
			return exitUsage
		}
		explicitStateDir = abs
	}
	c := colors{on: !*noColor && os.Getenv("NO_COLOR") == ""}
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, version.Version)
		return exitOK
	case "enroll":
		return enroll(cfg, *apiURL, *tokenFile, *name, stdin, stdout, stderr, c)
	case "run":
		if launch {
			return runServiceLauncher(cfg, rest, stderr)
		}
		if service.Manager(*manager) == service.ManagerLaunchd {
			return runLaunchd(cfg)
		}
		return runService(cfg)
	case "status":
		return status(cfg, stdout, stderr, c)
	case "reset":
		return reset(cfg, *yes, stdout, stderr)
	case "image build":
		return imageBuild(cfg, ib, stdout, stderr, c)
	case "service print":
		return servicePrint(explicitStateDir, stdout, stderr)
	case "service install":
		return serviceInstall(explicitStateDir, stdout, stderr)
	case "service migrate", "service prepare":
		return serviceLauncherSetup(cmd == "service migrate", stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return exitUsage
}

func newLogger(cfg config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})).With("component", "ownsand-agent")
}

func enroll(cfg config.Config, apiURL, tokenFile, name string, stdin io.Reader, stdout, stderr io.Writer, c colors) int {
	if apiURL == "" {
		apiURL = cfg.APIURLOverride
	}
	if apiURL == "" {
		apiURL = config.DefaultAPIURL
	}
	var raw []byte
	var err error
	if tokenFile != "" {
		raw, err = os.ReadFile(tokenFile)
	} else {
		raw, err = bufio.NewReader(stdin).ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: read enrollment token:", err)
		return exitError
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		fmt.Fprintln(stderr, "ownsand-agent: the enrollment token is empty; pass it on standard input or with --token-file")
		return exitUsage
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	cp, err := cpclient.New(apiURL, nil)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	id, err := agent.Enroll(context.Background(), st, cp, token, name, newLogger(cfg))
	if err != nil {
		fmt.Fprintln(stderr, c.bad("enrollment failed:"), err)
		return exitError
	}
	fmt.Fprintf(stdout, "%s device %s in organization %s\n", c.ok("Enrolled"), id.DeviceID, id.OrganizationID)
	return exitOK
}

// runAgent runs the agent until ctx ends and returns the exit code.
func runAgent(ctx context.Context, cfg config.Config) int {
	code, _ := runAgentOutcome(ctx, cfg)
	return code
}

// runAgentOutcome runs the agent until ctx ends and returns the exit code
// and whether the agent installed an update and stops so that the service
// manager starts the new executable.
func runAgentOutcome(ctx context.Context, cfg config.Config) (code int, updated bool) {
	return runAgentWithInstaller(ctx, cfg, nil)
}

func runAgentWithInstaller(ctx context.Context, cfg config.Config, installer update.Installer) (code int, updated bool) {
	logger := newLogger(cfg)
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		logger.Error("open state directory failed", "error", err)
		return exitError, false
	}
	id, err := st.LoadIdentity()
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && id.DeviceID == ""):
		logger.Error("the device is not enrolled; run `ownsand-agent enroll`")
		return exitNotEnrolled, false
	case err != nil:
		// An unreadable or undecodable identity file is an error, not a
		// missing enrollment: exit code 3 would stop the service manager
		// from restarting the agent.
		logger.Error("read device identity failed", "error", err)
		return exitError, false
	}
	agentState, err := st.LoadAgent()
	if err != nil {
		logger.Error("read agent state failed", "error", err)
		return exitError, false
	}
	apiURL := agentState.APIURL
	if cfg.APIURLOverride != "" {
		apiURL = cfg.APIURLOverride
	}
	cp, err := cpclient.New(apiURL, nil)
	if err != nil {
		if cfg.APIURLOverride != "" {
			logger = logger.With("setting", config.EnvAPIURL)
		}
		logger.Error("control plane client failed", "error", err)
		return exitError, false
	}
	rt, err := runtimes.New(cfg, st, logger)
	if err != nil {
		logger.Error("sandbox runtime unavailable", "runtime", cfg.Runtime.String(), "error", err)
		return exitError, false
	}
	if installer == nil {
		exe, _ := os.Executable()
		installer = update.ExecutableInstaller{Path: exe}
	}
	a, err := agent.New(ctx, agent.Options{
		Store: st, Identity: id, Control: cp, Runtime: rt, Logger: logger,
		NewDataPath: newDataPath,
		Update: update.Options{
			ManifestURL: cfg.ReleaseManifestURL,
			Keys:        cfg.ReleasePublicKeys,
			Platform:    sysinfo.Platform(),
			Running:     version.Version,
			Installer:   installer,
		},
	})
	if err != nil {
		rt.Close()
		logger.Error("agent start failed", "error", err)
		return exitError, false
	}
	logger.Info("agent starting", "agent_version", version.Version, "device_id", id.DeviceID, "runtime", cfg.Runtime.String())
	switch err := a.Run(ctx); {
	case errors.Is(err, agent.ErrRemoved):
		return exitNotEnrolled, false
	case errors.Is(err, agent.ErrRestart):
		return exitOK, true
	case err == nil:
		return exitOK, false
	default:
		logger.Error("agent stopped", "error", err)
		return exitError, false
	}
}

func runInteractive(cfg config.Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAgent(ctx, cfg)
}

// runLaunchd runs the agent under the launchd daemon definition and maps the
// exit code to the launchd KeepAlive predicate (SuccessfulExit=false): an
// installed update exits nonzero so that launchd starts the new executable,
// and a missing enrollment or a finished removal exits 0 so that launchd does
// not restart the agent.
func runLaunchd(cfg config.Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, updated := runAgentOutcome(ctx, cfg)
	mapped := service.LaunchdExitCode(code, updated)
	newLogger(cfg).Info("agent exits under launchd", "exit_code", code, "update_installed", updated, "launchd_exit_code", mapped)
	return mapped
}

func status(cfg config.Config, stdout, stderr io.Writer, c colors) int {
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	id, err := st.LoadIdentity()
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && id.DeviceID == ""):
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Enrollment:"), c.bad("not enrolled"))
		return exitNotEnrolled
	case err != nil:
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	fmt.Fprintf(stdout, "%s %s (device %s, organization %s)\n", c.bold("Enrollment:"), c.ok("enrolled"), id.DeviceID, id.OrganizationID)
	a, agentErr := st.LoadAgent()
	var s agent.Status
	statusErr := st.LoadStatus(&s)
	if statusErr == nil && s.APIURL != "" {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Control plane:"), s.APIURL)
	} else if agentErr == nil {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Control plane:"), a.APIURL)
	}
	if agentErr == nil && a.Removal.NoticeReceived {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Removal:"), c.bad("in progress"))
	}
	// Only an absent status.json means the agent has not run. A file that
	// cannot be read or decoded is an error: the report is skipped, the other
	// sections still print, and the command exits with exitError.
	code := exitOK
	switch {
	case errors.Is(statusErr, store.ErrNotFound):
		fmt.Fprintf(stdout, "%s unknown (the agent has not run)\n", c.bold("Connection:"))
	case statusErr != nil:
		fmt.Fprintln(stderr, "ownsand-agent: read status.json:", statusErr)
		code = exitError
	default:
		conn := c.bad("disconnected")
		if s.Connected {
			conn = c.ok("connected")
		}
		fmt.Fprintf(stdout, "%s %s (status of %s)\n", c.bold("Connection:"), conn, s.UpdatedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(stdout, "%s %s, runtime %s\n", c.bold("Agent:"), s.AgentVersion, s.Runtime)
		fmt.Fprintf(stdout, "%s %s %s\n", c.bold("Update:"), s.UpdateState, s.UpdateTarget)
		printCapabilities(stdout, s, c)
	}
	// A failed inventory read is an error, never a smaller count: the partial
	// result is discarded, the line shows the count as unavailable, the
	// diagnostic names the inventory on standard error, and the command exits
	// with exitError after both inventory lines have printed.
	var sandboxes []string
	if err := st.LoadSandboxes(func(b []byte) error {
		sandboxes = append(sandboxes, string(b))
		return nil
	}); err != nil {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Sandboxes:"), c.bad(fmt.Sprintf("unavailable (%v)", err)))
		fmt.Fprintln(stderr, "ownsand-agent: read sandbox records:", err)
		code = exitError
	} else {
		fmt.Fprintf(stdout, "%s %d records\n", c.bold("Sandboxes:"), len(sandboxes))
	}
	entries, err := os.ReadDir(st.Path(store.SnapshotsDir))
	if err != nil {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Snapshots:"), c.bad("unavailable"))
		fmt.Fprintln(stderr, "ownsand-agent: read snapshots:", err)
		return exitError
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Strings(snaps)
	fmt.Fprintf(stdout, "%s %d\n", c.bold("Snapshots:"), len(snaps))
	return code
}

// printCapabilities prints the capabilities of the last capability report
// in the status file: the guest OSes, the built base images and every
// hypervisor check with its result and detail.
func printCapabilities(stdout io.Writer, s agent.Status, c colors) {
	if s.CapabilitiesAt.IsZero() {
		fmt.Fprintf(stdout, "%s unknown (no capability report yet)\n", c.bold("Capabilities:"))
		return
	}
	none := func(v []string) string {
		if len(v) == 0 {
			return "none"
		}
		return strings.Join(v, ", ")
	}
	fmt.Fprintf(stdout, "%s guest OSes %s; base images %s (report of %s)\n", c.bold("Capabilities:"),
		none(s.GuestOSes), none(s.BaseImages), s.CapabilitiesAt.Format("2006-01-02 15:04:05"))
	for _, ch := range s.Checks {
		result := c.ok("passed")
		if !ch.Passed {
			result = c.bad("failed")
		}
		fmt.Fprintf(stdout, "  %s: %s", ch.ID, result)
		if ch.Detail != "" {
			fmt.Fprintf(stdout, " (%s)", ch.Detail)
		}
		fmt.Fprintln(stdout)
	}
}

func reset(cfg config.Config, yes bool, stdout, stderr io.Writer) int {
	if !yes {
		fmt.Fprintln(stderr, "ownsand-agent: reset deletes every local sandbox, snapshot, image and the device key; stop the service and pass --yes")
		return exitUsage
	}
	// A failed host clean-up does not stop the deletion of the local data, so
	// that one reset removes everything that it can; it still makes the reset
	// fail, and running reset again deletes the remaining host resources.
	code := exitOK
	if err := resetHost(cfg); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: reset host resources:", err)
		fmt.Fprintln(stderr, "ownsand-agent: run reset again, as an operator with CAP_NET_ADMIN, to delete the remaining host resources")
		code = exitError
	}
	// ResetDir, not store.Open: reset must work on a damaged state directory
	// (for example a regular file where a runtime subdirectory belongs), and
	// Open would fail on it before it deleted anything.
	if err := store.ResetDir(cfg.StateDir); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		if errors.Is(err, store.ErrEnrollmentBusy) {
			fmt.Fprintln(stderr, "ownsand-agent: run reset again after enrollment or the other reset ends")
		}
		return exitError
	}
	fmt.Fprintln(stdout, "Local agent data deleted from", filepath.Clean(cfg.StateDir))
	return code
}

func servicePrint(stateDir string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	def, err := serviceDefinition(exe, stateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: render service definition:", err)
		return exitError
	}
	fmt.Fprint(stdout, def)
	return exitOK
}
