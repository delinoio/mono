// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	return service.WindowsCommand(exe) + "\n"
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
	fmt.Fprintf(stdout, "Installed the %s service. Start it with:\n  sc.exe start %s\n", service.Name, service.Name)
	return exitOK
}
