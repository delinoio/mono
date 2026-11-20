// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

// resetHost removes host resources that a runtime created outside the
// state directory. The process runtime creates none.
func resetHost(config.Config, *store.Store) error {
	return nil
}
