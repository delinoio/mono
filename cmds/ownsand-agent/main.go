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
  image build    Build the macOS or Windows base image locally.
  reset --yes    Delete all local sandboxes, snapshots, images and the device key.
  service print  Print the service definition of this host OS.
  service install
                 Install the service definition of this host OS.
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
	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
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
	c := colors{on: !*noColor && os.Getenv("NO_COLOR") == ""}
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, version.Version)
		return exitOK
	case "enroll":
		return enroll(cfg, *apiURL, *tokenFile, *name, stdin, stdout, stderr, c)
	case "run":
		if service.Manager(*manager) == service.ManagerLaunchd {
			return runLaunchd(cfg)
		}
		return runService(cfg)
	case "status":
		return status(cfg, stdout, stderr, c)
	case "reset":
		return reset(cfg, *yes, stdout, stderr)
	case "image build":
		fmt.Fprintln(stderr, "ownsand-agent: image build is not implemented in this version: the macOS (Apple IPSW) and Windows (ISO) base image builds are open technical work, and Linux base images are not built locally")
		return exitError
	case "service print":
		return servicePrint(stdout, stderr)
	case "service install":
		return serviceInstall(stdout, stderr)
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
	logger := newLogger(cfg)
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		logger.Error("open state directory failed", "error", err)
		return exitError, false
	}
	id, err := st.LoadIdentity()
	if err != nil || id.DeviceID == "" {
		logger.Error("the device is not enrolled; run `ownsand-agent enroll`")
		return exitNotEnrolled, false
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
		logger.Error("control plane client failed", "error", err)
		return exitError, false
	}
	rt, err := runtimes.New(cfg, st, logger)
	if err != nil {
		logger.Error("sandbox runtime unavailable", "runtime", cfg.Runtime.String(), "error", err)
		return exitError, false
	}
	exe, _ := os.Executable()
	a, err := agent.New(ctx, agent.Options{
		Store: st, Identity: id, Control: cp, Runtime: rt, Logger: logger,
		NewDataPath: newDataPath,
		Update: update.Options{
			ManifestURL: cfg.ReleaseManifestURL,
			Keys:        cfg.ReleasePublicKeys,
			Platform:    sysinfo.Platform(),
			Running:     version.Version,
			Installer:   update.ExecutableInstaller{Path: exe},
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
	if err != nil || id.DeviceID == "" {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Enrollment:"), c.bad("not enrolled"))
		return exitNotEnrolled
	}
	fmt.Fprintf(stdout, "%s %s (device %s, organization %s)\n", c.bold("Enrollment:"), c.ok("enrolled"), id.DeviceID, id.OrganizationID)
	if a, err := st.LoadAgent(); err == nil {
		fmt.Fprintf(stdout, "%s %s\n", c.bold("Control plane:"), a.APIURL)
		if a.Removal.NoticeReceived {
			fmt.Fprintf(stdout, "%s %s\n", c.bold("Removal:"), c.bad("in progress"))
		}
	}
	var s agent.Status
	if err := st.LoadStatus(&s); err != nil {
		fmt.Fprintf(stdout, "%s unknown (the agent has not run)\n", c.bold("Connection:"))
	} else {
		conn := c.bad("disconnected")
		if s.Connected {
			conn = c.ok("connected")
		}
		fmt.Fprintf(stdout, "%s %s (status of %s)\n", c.bold("Connection:"), conn, s.UpdatedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(stdout, "%s %s, runtime %s\n", c.bold("Agent:"), s.AgentVersion, s.Runtime)
		fmt.Fprintf(stdout, "%s %s %s\n", c.bold("Update:"), s.UpdateState, s.UpdateTarget)
	}
	var sandboxes []string
	_ = st.LoadSandboxes(func(b []byte) error {
		sandboxes = append(sandboxes, string(b))
		return nil
	})
	fmt.Fprintf(stdout, "%s %d records\n", c.bold("Sandboxes:"), len(sandboxes))
	entries, _ := os.ReadDir(st.Path(store.SnapshotsDir))
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Strings(snaps)
	fmt.Fprintf(stdout, "%s %d\n", c.bold("Snapshots:"), len(snaps))
	return exitOK
}

func reset(cfg config.Config, yes bool, stdout, stderr io.Writer) int {
	if !yes {
		fmt.Fprintln(stderr, "ownsand-agent: reset deletes every local sandbox, snapshot, image and the device key; stop the service and pass --yes")
		return exitUsage
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	// A failed host clean-up does not stop the deletion of the local data, so
	// that one reset removes everything that it can; it still makes the reset
	// fail, and running reset again deletes the remaining host resources.
	code := exitOK
	if err := resetHost(cfg, st); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: reset host resources:", err)
		fmt.Fprintln(stderr, "ownsand-agent: run reset again, as an operator with CAP_NET_ADMIN, to delete the remaining host resources")
		code = exitError
	}
	if err := st.Reset(); err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	fmt.Fprintln(stdout, "Local agent data deleted from", filepath.Clean(cfg.StateDir))
	return code
}

func servicePrint(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	fmt.Fprint(stdout, serviceDefinition(exe))
	return exitOK
}
