// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"log/slog"
	"os"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

// cleanupHost is the host network clean-up of reset; tests replace it so
// that they never touch the network of the machine that runs them.
var cleanupHost = netpolicy.CleanupHost

// resetHost removes host resources that the Firecracker runtime creates
// outside the state directory: the sandbox tap devices and the nftables
// table (Linux only; a no-op elsewhere). It returns the joined failures of
// the resources that it could not delete; a resource that is already absent
// is not a failure.
func resetHost(cfg config.Config, _ *store.Store) error {
	if cfg.Runtime != config.RuntimeKindFirecracker {
		return nil
	}
	return cleanupHost(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}
