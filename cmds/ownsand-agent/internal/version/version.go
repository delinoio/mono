// SPDX-License-Identifier: Apache-2.0

// Package version holds the running agent version. Compare versions with
// github.com/delinoio/mono/internal/semver.
package version

// Version is the running agent version. Release builds set it with
// -ldflags "-X github.com/delinoio/mono/cmds/ownsand-agent/internal/version.Version=<version>".
var Version = "0.0.0-dev"
