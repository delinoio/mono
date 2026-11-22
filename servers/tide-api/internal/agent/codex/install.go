// SPDX-License-Identifier: FSL-1.1-ALv2

package codex

import (
	"sort"
	"strings"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
)

// CodexVersion is the pinned version of the `@openai/codex` npm package
// that tide installs when a guest has no `codex`. The self-hosted guide
// installs `@openai/codex@alpha`, and whether OpenAI supports the stable
// line (`latest`) with the Agents API is NOT DOCUMENTED, so tide pins the
// newest `alpha` version (recorded decision). Renovate follows the `alpha`
// dist-tag through the custom manager in renovate.json.
//
// renovate: datasource=npm depName=@openai/codex
const CodexVersion = "0.173.0-alpha.4"

// NodeVersion is the pinned Node.js 24 LTS version that tide installs
// when a guest has no Node.js 16 or later (`engines.node` of
// @openai/codex). Renovate updates it within the 24 line; the checksums
// below must be updated in the same change (TestNodeChecksums fails until
// they are).
//
// renovate: datasource=node-version depName=node
const NodeVersion = "24.22.3"

// nodeChecksumsVersion is the Node.js version of nodeSHA256.
const nodeChecksumsVersion = "24.22.3"

// nodeSHA256 holds the SHA-256 of the official archives of
// nodeChecksumsVersion from https://nodejs.org/dist/v24.22.3/SHASUMS256.txt,
// by <os>-<arch>. tide uses the .tar.gz archives, which need no xz in the
// guest.
var nodeSHA256 = map[string]string{
	"linux-x64":    "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2",
	"linux-arm64":  "261af2af250dfd7e9f25cf151d1b2713a0adf8f10a9ad116c4947f68627affe8",
	"darwin-x64":   "977614af0504c81045e7fd85596dbca0e5f951b96681516dadafedcca8c447f7",
	"darwin-arm64": "ff6e17d4874b76d272212b908cc020d6e815fba5a6c7eaaa06add725840307fd",
}

// nodeDir is the guest directory of the Node.js that tide installs,
// relative to $HOME; its bin directory is on PATH of the install and start
// scripts, before the guest's own PATH.
const nodeDir = ".local/share/tide/node-v" + NodeVersion

// nodeBin is the bin directory of nodeDir.
const nodeBin = nodeDir + "/bin"

// Exit codes of InstallScript.
var installCodes = map[int64]string{
	10: "the guest has neither curl nor wget, which the Node.js download needs",
	11: "the guest has no tar, which the Node.js archive needs",
	12: "the Node.js download or extraction failed",
	13: "the Node.js archive does not match its pinned SHA-256",
	14: "npm could not install @openai/codex@" + CodexVersion,
	15: "codex is missing after the installation",
	16: "the guest platform has no official Node.js archive (Linux and macOS on x64 or arm64 only)",
	17: "the guest uses musl libc, for which Node.js publishes no official archive; use a template with Node.js 16 or later",
	18: "the OpenAI Codex adapter supports Linux and macOS guests only",
}

// InstallScript returns the guest script of Prepare. A guest with a
// working `codex` keeps it at its installed version (the version policy of
// the contract). Otherwise, when the guest has no Node.js 16 or later, the
// script downloads the official Node.js archive of NodeVersion for the
// machine, verifies its pinned SHA-256 and extracts it to $HOME/<nodeDir>;
// then `npm install -g @openai/codex@<CodexVersion>` installs codex with
// the npm prefix $HOME/.local, so no root access is needed.
func InstallScript(guest tidev1.GuestOs) string {
	if agent.IsWindows(guest) {
		return "exit 18\n"
	}
	var cases strings.Builder
	keys := make([]string, 0, len(nodeSHA256))
	for k := range nodeSHA256 {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cases.WriteString("  " + k + ") sum=" + nodeSHA256[k] + " ;;\n")
	}
	return agent.ScriptPrelude(guest, nodeBin) + `export NPM_CONFIG_PREFIX="$HOME/.local"
if command -v codex >/dev/null 2>&1 && codex --version >/dev/null 2>&1; then exit 0; fi
node_ok() {
  command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1 || return 1
  major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null) || return 1
  [ "${major:-0}" -ge 16 ] 2>/dev/null
}
if ! node_ok; then
  case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) exit 16 ;;
  esac
  case "$(uname -m)" in
  x86_64 | amd64) arch=x64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) exit 16 ;;
  esac
  if [ "$os" = linux ] && { [ -e /lib/ld-musl-x86_64.so.1 ] || [ -e /lib/ld-musl-aarch64.so.1 ]; }; then exit 17; fi
  case "$os-$arch" in
` + cases.String() + `  *) exit 16 ;;
  esac
  command -v tar >/dev/null 2>&1 || exit 11
  file="node-v` + NodeVersion + `-$os-$arch.tar.gz"
  url="https://nodejs.org/dist/v` + NodeVersion + `/$file"
  tmp=$(mktemp -d) || exit 12
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 -o "$tmp/$file" "$url" || { rm -rf "$tmp"; exit 12; }
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$tmp/$file" "$url" || { rm -rf "$tmp"; exit 12; }
  else
    rm -rf "$tmp"; exit 10
  fi
  if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d ' ' -f 1); else got=$(shasum -a 256 "$tmp/$file" | cut -d ' ' -f 1); fi
  [ "$got" = "$sum" ] || { rm -rf "$tmp"; exit 13; }
  dest="$HOME/` + nodeDir + `"
  rm -rf "$dest.partial" && mkdir -p "$dest.partial" && tar xzf "$tmp/$file" -C "$dest.partial" --strip-components 1 || { rm -rf "$tmp"; exit 12; }
  rm -rf "$tmp" "$dest" && mv "$dest.partial" "$dest" || exit 12
  node_ok || exit 12
fi
npm install -g "@openai/codex@` + CodexVersion + `" >/dev/null 2>&1 || exit 14
command -v codex >/dev/null 2>&1 || exit 15
`
}
