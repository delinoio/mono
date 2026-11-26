// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the license notices of `scripts/ci/release-binaries.sh build
// cross`: the module scan of each platform uses the cgo setting that the
// build of that platform uses, and the notices hold the licenses of every
// platform. The stubbed `go list` can return different modules for each cgo
// setting of a platform.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = fileURLToPath(new URL("../..", import.meta.url));
const script = join(root, "scripts/ci/release-binaries.sh");
const bashVersion = spawnSync(
  "bash",
  ["-c", 'echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"'],
  { encoding: "utf8" },
)
  .stdout?.trim()
  .split(" ")
  .map(Number) ?? [0, 0];
const hasJq = spawnSync("sh", ["-c", "command -v jq"]).status === 0;
function skipReason() {
  if (process.platform === "win32") return "the release scripts run on Linux";
  if (!hasJq) return "jq is not installed";
  if (bashVersion[0] < 4 || (bashVersion[0] === 4 && bashVersion[1] < 4)) {
    return `the release scripts need Bash 4.4 or newer, found ${bashVersion.join(".")}`;
  }
  return false;
}
const skip = skipReason();

function binary(name, dir, platforms, cgoPlatforms) {
  return {
    name,
    dir,
    kind: "binary",
    target: "github-release",
    version: "1.0.0",
    binary: name,
    platforms,
    cgoPlatforms,
    versionVariable: "",
    entitlements: "",
    licenseFiles: ["LICENSE"],
    manifestObjectKey: "",
    manifestPublicUrl: "",
  };
}

// `go list` answers with the module file of its platform and logs its cgo
// setting; `go build` writes a placeholder executable.
const goStub = `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  env)
    case "$2" in
      GOROOT) echo "$STUB_DIR/goroot" ;;
      GOHOSTOS) echo freebsd ;;
      GOHOSTARCH) echo amd64 ;;
    esac ;;
  build)
    out=""
    while (( $# > 0 )); do
      if [[ "$1" == -o ]]; then out="$2"; shift; fi
      shift
    done
    printf 'binary\\n' > "$out" ;;
  list)
    printf '%s %s-%s CGO_ENABLED=%s\\n' "\${*: -1}" "$GOOS" "$GOARCH" "$CGO_ENABLED" >> "$STUB_LOG"
    modules="$STUB_DIR/go-list-$GOOS-$GOARCH"
    if [[ -f "$modules-$CGO_ENABLED.tsv" ]]; then
      cat "$modules-$CGO_ENABLED.tsv"
    else
      cat "$modules.tsv"
    fi ;;
  *) echo "unexpected go $*" >&2; exit 1 ;;
esac
`;

function write(path, content) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

// Runs `build cross` for the binaries. `modules` maps a platform to the
// module names that its `go list` prints. Append -0 or -1 to a platform key
// to supply modules for a specific cgo setting. Every module has a license file.
function buildCross(t, binaries, modules) {
  const dir = mkdtempSync(join(tmpdir(), "release-binaries-notices-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  write(join(dir, "bin/go"), goStub);
  chmodSync(join(dir, "bin/go"), 0o755);
  write(join(dir, "goroot/LICENSE"), "Go license\n");
  const log = join(dir, "log");
  writeFileSync(log, "");
  const summary = join(dir, "summary");
  writeFileSync(summary, "");
  for (const [platform, names] of Object.entries(modules)) {
    let rows = "";
    for (const module of names) {
      write(join(dir, "modules", module, "LICENSE"), `${module} license\n`);
      rows += `example.com/${module}\tv1.0.0\t${join(dir, "modules", module)}\n`;
    }
    write(join(dir, `go-list-${platform}.tsv`), rows);
  }
  const out = join(dir, "out");
  const result = spawnSync("bash", [script, "build", "cross", out], {
    encoding: "utf8",
    cwd: root,
    env: {
      PATH: `${join(dir, "bin")}${delimiter}${process.env.PATH}`,
      HOME: dir,
      STUB_DIR: dir,
      STUB_LOG: log,
      BINARIES: JSON.stringify(binaries),
      RUNNER_TEMP: join(dir, "runner"),
      GITHUB_STEP_SUMMARY: summary,
    },
  });
  assert.equal(result.status, 0, result.stderr);
  const scans = readFileSync(log, "utf8").trimEnd().split("\n").sort();
  const notices = (name) =>
    readFileSync(
      join(out, "release", name, "THIRD-PARTY-LICENSES.txt"),
      "utf8",
    );
  return { scans, notices };
}

test(
  "build cross scans only the cgo platforms of a binary with cgo",
  { skip },
  (t) => {
    const agent = binary(
      "agent",
      "cmds/agent",
      ["linux-amd64", "darwin-arm64", "windows-arm64"],
      ["darwin-arm64"],
    );
    const cli = binary("cli", "cmds/cli", ["linux-arm64", "darwin-amd64"], []);
    const everyPlatform = {
      "linux-amd64": ["common", "linux-only"],
      "darwin-arm64-0": ["common", "darwin-arm64-nocgo-only"],
      "darwin-arm64-1": ["common", "darwin-arm64-cgo-only"],
      "windows-arm64": ["common", "windows-only"],
      "linux-arm64": ["common", "cli-linux"],
      "darwin-amd64": ["common", "cli-darwin"],
    };
    const { scans, notices } = buildCross(t, [agent, cli], everyPlatform);
    assert.deepEqual(scans, [
      "./cmds/agent darwin-arm64 CGO_ENABLED=1",
      "./cmds/agent linux-amd64 CGO_ENABLED=0",
      "./cmds/agent windows-arm64 CGO_ENABLED=0",
      "./cmds/cli darwin-amd64 CGO_ENABLED=0",
      "./cmds/cli linux-arm64 CGO_ENABLED=0",
    ]);
    // The notices keep the licenses of every platform of the binary.
    const agentNotices = notices("agent");
    for (const module of [
      "common",
      "linux-only",
      "darwin-arm64-cgo-only",
      "windows-only",
    ]) {
      assert.match(
        agentNotices,
        new RegExp(`example\\.com/${module} v1\\.0\\.0: LICENSE\\n`),
      );
    }
    assert.doesNotMatch(agentNotices, /darwin-arm64-nocgo-only/);
    assert.doesNotMatch(agentNotices, /cli-linux|cli-darwin/);
    assert.equal(
      agentNotices.match(/example\.com\/common v1\.0\.0: LICENSE\n/g).length,
      1,
    );
    const cliNotices = notices("cli");
    for (const module of ["common", "cli-linux", "cli-darwin"]) {
      assert.match(
        cliNotices,
        new RegExp(`example\\.com/${module} v1\\.0\\.0: LICENSE\\n`),
      );
    }
  },
);

test(
  "build cross scans every platform without cgo when no platform has cgo",
  { skip },
  (t) => {
    const agent = binary(
      "agent",
      "cmds/agent",
      ["linux-amd64", "darwin-arm64", "windows-arm64"],
      [],
    );
    const { scans } = buildCross(t, [agent], {
      "linux-amd64": ["a"],
      "darwin-arm64": ["a"],
      "windows-arm64": ["a"],
    });
    assert.deepEqual(scans, [
      "./cmds/agent darwin-arm64 CGO_ENABLED=0",
      "./cmds/agent linux-amd64 CGO_ENABLED=0",
      "./cmds/agent windows-arm64 CGO_ENABLED=0",
    ]);
  },
);
