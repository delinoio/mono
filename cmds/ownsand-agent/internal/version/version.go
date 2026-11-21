// SPDX-License-Identifier: Apache-2.0

// Package version holds the agent version. The comparison of release
// versions lives in github.com/delinoio/mono/go/ownsand/version.
package version

// Version is the running agent version. Release builds set it with
// -ldflags "-X github.com/delinoio/mono/cmds/ownsand-agent/internal/version.Version=<version>".
var Version = "0.0.0-dev"
