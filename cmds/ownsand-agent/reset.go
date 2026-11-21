// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"log/slog"
	"os"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

// resetHost removes host resources that the Firecracker runtime creates
// outside the state directory: the sandbox tap devices and the nftables
// table (Linux only; a no-op elsewhere).
func resetHost(cfg config.Config, _ *store.Store) error {
	if cfg.Runtime == config.RuntimeKindFirecracker {
		netpolicy.CleanupHost(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
	return nil
}
