// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
)

// runService runs under the Windows service control manager when started
// as a service, and interactively otherwise.
func runService(cfg config.Config) int {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return runInteractive(cfg)
	}
	return runUnderServiceManager(cfg, runAgentOutcome)
}

// runConfigError ends `run` when its configuration does not parse. Under the
// service control manager the process still connects to the manager and
// reports the usage exit code as a service-specific error, like every other
// nonzero exit of run: a service process that exits before it connects is
// reported only as a start timeout (error 1053, event 7009), its error text
// is lost because a service has no standard error, and the failure actions
// do not apply. The error goes to agent.log in the state directory that the
// environment names, or the default one, because the configuration that would
// name it did not parse.
func runConfigError(cfgErr error, stderr io.Writer) int {
	fmt.Fprintln(stderr, "ownsand-agent:", cfgErr)
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return exitUsage
	}
	return runUnderServiceManager(config.Config{StateDir: configErrorStateDir(os.LookupEnv)}, configErrorRun(cfgErr))
}

// configErrorStateDir returns the state directory for the log of a
// configuration error: the value of OWNSAND_AGENT_STATE_DIR when it is set,
// otherwise the default state directory of the host OS.
func configErrorStateDir(lookup config.Lookup) string {
	if v, ok := lookup(config.EnvStateDir); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return config.DefaultStateDir()
}

// configErrorRun returns the agent run of a service whose configuration did
// not parse: it logs the error and ends with the usage exit code.
func configErrorRun(cfgErr error) func(context.Context, config.Config) (int, bool) {
	return func(context.Context, config.Config) (int, bool) {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "ownsand-agent").Error("invalid configuration", "error", cfgErr.Error())
		return exitUsage, false
	}
}

// runUnderServiceManager runs the agent with run under the Windows service
// control manager.
func runUnderServiceManager(cfg config.Config, run func(context.Context, config.Config) (int, bool)) int {
	// A Windows service has no standard error; the agent writes its JSON
	// log to agent.log in the state directory. The file is opened with
	// delete sharing (see openServiceLog) so that a device removal can delete
	// it while the service runs, and it is closed when the service ends.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err == nil {
		if f, err := openServiceLog(filepath.Join(cfg.StateDir, "agent.log")); err == nil {
			prev := os.Stderr
			os.Stderr = f
			defer func() {
				os.Stderr = prev
				_ = f.Close()
			}()
		}
	}
	h := &handler{cfg: cfg, run: run}
	if err := svc.Run(service.Name, h); err != nil {
		return exitError
	}
	return h.code
}

type handler struct {
	cfg config.Config
	// run runs the agent; tests replace it.
	run func(context.Context, config.Config) (int, bool)
	// code is the public exit code of `run`, unchanged by the status that
	// the handler reports to the service control manager.
	code int
}

// Execute runs the agent as a service. It reports the exit status that
// service.WindowsExitStatus derives from the outcome, so that the failure
// actions of the service restart the agent after an installed update or an
// error and not after a stop request, a missing enrollment or a removal.
func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		code    int
		updated bool
	}
	done := make(chan outcome, 1)
	go func() {
		code, updated := h.run(ctx, h.cfg)
		done <- outcome{code, updated}
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case o := <-done:
			cancel()
			return h.finish(o.code, o.updated, false)
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				o := <-done
				return h.finish(o.code, o.updated, true)
			}
		}
	}
}

// finish records the public exit code and returns the status for the service
// control manager.
func (h *handler) finish(code int, updated, stopRequested bool) (bool, uint32) {
	h.code = code
	serviceSpecific, exit := service.WindowsExitStatus(code, updated, stopRequested)
	slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "ownsand-agent").Info("agent exits under the Windows service",
		"exit_code", code, "update_installed", updated, "stop_requested", stopRequested, "reported_exit_code", exit, "service_specific", serviceSpecific)
	return serviceSpecific, exit
}

func serviceDefinition(exe string) string {
	return service.WindowsDefinition(exe, serviceEnv())
}

// setServiceEnvironment writes the REG_MULTI_SZ Environment value of the
// service key: the service control manager passes it to the service process
// as its environment.
func setServiceEnvironment(entries []string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, service.WindowsServiceRegistryKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringsValue(service.WindowsEnvironmentValue, entries)
}

func serviceInstall(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: connect to the service manager (run as administrator):", err)
		return exitError
	}
	defer m.Disconnect()
	s, err := m.CreateService(service.Name, exe, mgr.Config{DisplayName: "OwnSand device agent", StartType: mgr.StartAutomatic}, "run")
	if err != nil {
		fmt.Fprintln(stderr, "ownsand-agent: create service:", err)
		return exitError
	}
	defer s.Close()
	// The failure actions make the service control manager restart the agent
	// after the service-specific exit statuses of service.WindowsExitStatus.
	if err := service.InstallWindowsRecovery(
		func() error {
			return s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: service.WindowsRestartDelayMillis * time.Millisecond}}, service.WindowsFailureResetSecs)
		},
		func() error { return s.SetRecoveryActionsOnNonCrashFailures(true) },
		s.Delete,
	); err != nil {
		slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "ownsand-agent").Error("service failure actions failed", "service", service.Name, "error", err.Error())
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	// A Windows service does not inherit the installing shell environment,
	// so the OWNSAND_AGENT_* variables go into the service Environment value
	// before the service can start. Only names and the count are logged:
	// values can hold secrets such as release keys.
	env := serviceEnv()
	if err := service.InstallWindowsEnvironment(env, setServiceEnvironment, s.Delete); err != nil {
		slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "ownsand-agent").Error("service environment write failed", "service", service.Name, "variable_count", len(env), "variables", environmentNames(env), "error", err.Error())
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	if len(env) > 0 {
		slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "ownsand-agent").Info("service environment written", "service", service.Name, "variable_count", len(env), "variables", environmentNames(env))
	}
	fmt.Fprintf(stdout, "Installed the %s service. Start it with:\n  sc.exe start %s\n", service.Name, service.Name)
	return exitOK
}
