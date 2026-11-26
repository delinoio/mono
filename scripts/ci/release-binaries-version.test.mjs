// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the host executable version check of `build cross` in
// `scripts/ci/release-binaries.sh`: the host executable must exit with 0 and
// print exactly the planned version on standard output. The fixture stubs `go`
// so that the "built" executable prints the text that each case sets.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
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

const version = "1.2.3";
const binary = {
  name: "ownsand-agent",
  dir: "cmds/ownsand-agent",
  kind: "binary",
  target: "github-release",
  version,
  binary: "ownsand-agent",
  platforms: ["linux-amd64"],
  cgoPlatforms: [],
  versionVariable:
    "github.com/delinoio/mono/cmds/ownsand-agent/internal/version.Version",
  entitlements: "",
  licenseFiles: ["LICENSE"],
  manifestObjectKey: "",
  manifestPublicUrl: "",
};

// `go build` copies the executable stub to its `-o` path (for the release
// manifest tool too, which the fixture never runs). `go list` lists no module.
const goStub = `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  env)
    case "$2" in
      GOROOT) echo "$STUB_DIR/goroot" ;;
      GOHOSTOS) echo linux ;;
      GOHOSTARCH) echo amd64 ;;
    esac ;;
  build)
    out=""
    while (( $# > 0 )); do
      if [[ "$1" == -o ]]; then out="$2"; shift; fi
      shift
    done
    cp "$STUB_DIR/executable" "$out"; chmod 755 "$out" ;;
  list) ;;
  *) echo "unexpected go $*" >&2; exit 1 ;;
esac
`;

// Read stdout from a file so the test case can provide NUL bytes.
const executableStub = `#!/usr/bin/env bash
cat "$STUB_DIR/stdout"
printf '%s' "\${STUB_VERSION_STDERR-}" >&2
exit "\${STUB_VERSION_STATUS:-0}"
`;

function runBuild(t, stdout, env = {}) {
  const dir = mkdtempSync(join(tmpdir(), "release-binaries-version-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "bin"));
  mkdirSync(join(dir, "goroot"));
  mkdirSync(join(dir, "runner"));
  writeFileSync(join(dir, "goroot/LICENSE"), "Go license\n");
  writeFileSync(join(dir, "bin/go"), goStub);
  writeFileSync(join(dir, "executable"), executableStub);
  writeFileSync(join(dir, "stdout"), stdout);
  writeFileSync(join(dir, "summary"), "");
  chmodSync(join(dir, "bin/go"), 0o755);
  const result = spawnSync(
    "bash",
    [script, "build", "cross", join(dir, "out")],
    {
      encoding: "utf8",
      cwd: root,
      env: {
        PATH: `${join(dir, "bin")}${delimiter}${process.env.PATH}`,
        HOME: dir,
        TMPDIR: join(dir, "runner"),
        STUB_DIR: dir,
        BINARIES: JSON.stringify([binary]),
        RUNNER_TEMP: join(dir, "runner"),
        GITHUB_STEP_SUMMARY: join(dir, "summary"),
        ...env,
      },
    },
  );
  assert.deepEqual(readdirSync(join(dir, "runner")), []);
  return result;
}

const asset = "ownsand-agent-linux-amd64";

test(
  "the host executable that prints exactly the version passes",
  { skip },
  (t) => {
    const result = runBuild(t, `${version}\n`);
    assert.equal(result.status, 0, result.stderr);
  },
);

test(
  "the host executable that prints the version without a newline passes",
  { skip },
  (t) => {
    const result = runBuild(t, version);
    assert.equal(result.status, 0, result.stderr);
  },
);

test(
  "standard error output does not fail an exact version on standard output",
  { skip },
  (t) => {
    const result = runBuild(t, `${version}\n`, {
      STUB_VERSION_STDERR: "a diagnostic\n",
    });
    assert.equal(result.status, 0, result.stderr);
  },
);

for (const [label, printed] of [
  ["a longer version that contains it", `11.2.3\n`],
  ["a prefix before it", `v${version}\n`],
  ["text around it", `ownsand-agent ${version} (linux)\n`],
  ["two trailing newlines", `${version}\n\n`],
  ["a leading NUL byte", `\0${version}\n`],
  ["an embedded NUL byte", "1.2\0.3\n"],
  ["a trailing NUL byte", `${version}\n\0`],
  ["nothing", ""],
]) {
  test(`the host executable that prints ${label} fails`, { skip }, (t) => {
    const result = runBuild(t, printed);
    assert.notEqual(result.status, 0);
    assert.ok(
      result.stderr.includes(`${asset} version must print exactly ${version}`),
      result.stderr,
    );
  });
}

test(
  "the host executable that prints the version on standard error only fails",
  { skip },
  (t) => {
    const result = runBuild(t, "", { STUB_VERSION_STDERR: `${version}\n` });
    assert.notEqual(result.status, 0);
    assert.ok(
      result.stderr.includes(`${asset} version must print exactly ${version}`),
      result.stderr,
    );
  },
);

test(
  "the host executable that exits with a nonzero status fails even with the version",
  { skip },
  (t) => {
    const result = runBuild(t, `${version}\n`, {
      STUB_VERSION_STDERR: "boom\n",
      STUB_VERSION_STATUS: "3",
    });
    assert.notEqual(result.status, 0);
    assert.ok(
      result.stderr.includes(
        `${asset} version exited with status 3 (standard error: boom )`,
      ),
      result.stderr,
    );
  },
);
