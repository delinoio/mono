// SPDX-License-Identifier: Apache-2.0

// Package version holds the bridge version, which the build sets with
// -ldflags "-X". The bridge version is the version of
// servers/sandcrew-api/package.json plus the commit, because sandcrew-api
// embeds the bridge binaries of the same commit
// (docs/cmds-sandcrew-bridge-foundation.md#runtime-and-language).
package version

// Version and Commit are set at build time.
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

// String returns "<version>+<commit>".
func String() string {
	return Version + "+" + Commit
}
