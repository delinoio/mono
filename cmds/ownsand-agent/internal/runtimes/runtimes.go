// SPDX-License-Identifier: Apache-2.0

// Package runtimes builds the sandbox runtime that the configuration
// selects.
package runtimes

import (
	"fmt"
	"log/slog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
)

// New returns the runtime of cfg.
func New(cfg config.Config, st *store.Store, logger *slog.Logger) (vmruntime.Runtime, error) {
	switch cfg.Runtime {
	case config.RuntimeKindProcess:
		return processrt.New(cfg.GuestBinary, logger)
	case config.RuntimeKindFirecracker:
		return newFirecracker(cfg, st, logger)
	}
	return nil, fmt.Errorf("%w: no sandbox runtime for this host; set %s", vmruntime.ErrUnsupported, config.EnvRuntime)
}
