// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

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
	// A Windows service has no standard error; the agent writes its JSON
	// log to agent.log in the state directory.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(cfg.StateDir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			os.Stderr = f
		}
	}
	h := &handler{cfg: cfg}
	if err := svc.Run(service.Name, h); err != nil {
		return exitError
	}
	return h.code
}

type handler struct {
	cfg  config.Config
	code int
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runAgent(ctx, h.cfg) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case code := <-done:
			cancel()
			h.code = code
			return false, uint32(code)
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.code = <-done
				return false, uint32(h.code)
			}
		}
	}
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
