// SPDX-License-Identifier: Apache-2.0
//
// Tests that `scripts/protos.sh generate`, `check` and `breaking` remove their
// scratch directory on success and on failure, and keep the exit status of
// the failing command, when the temporary directory path contains a space and
// an apostrophe. The script runs in a throwaway Git repository with a fake
// `go` executable that stands in for `go run buf@<version>`. The breaking
// cases also check that the script passes the archived base schema to Buf.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const repositoryRoot = fileURLToPath(new URL("../..", import.meta.url));
const protosScript = join(repositoryRoot, "scripts", "protos.sh");

// The script is bash and CI runs it on Linux only.
const skip = process.platform === "win32" && "scripts/protos.sh needs bash";

// Exit statuses of the fake buf subcommands that FAKE_GO_FAIL selects. The
// tests expect the script to exit with exactly these statuses, so a cleanup
// trap that changes the exit status is detected.
const generateFailureStatus = 7;
const breakingFailureStatus = 9;
const comparisonFixtureErrorStatus = 10;
const schemaPath = "delino/ownsand/v1/fixture.proto";
const baseSchema =
  'syntax = "proto3";\nmessage Fixture { string value = 1; }\n';
const headSchema = 'syntax = "proto3";\nmessage Fixture { int32 value = 1; }\n';

// Fake `go`. `lint` and `format` succeed. `breaking` checks the comparison
// tree before returning success or a simulated Buf failure. `generate` writes
// the same bindings that the fixture commits, so `check` finds no drift.
// FAKE_GO_FAIL selects the failing subcommand.
const fakeGo = `#!/usr/bin/env bash
set -euo pipefail
shift 2
subcommand="$1"
shift
if [[ "$subcommand" == "generate" && "\${FAKE_GO_FAIL:-}" == "generate" ]]; then
  echo "fake go: generate failure" >&2
  exit ${generateFailureStatus}
fi
comparison_fixture_error() {
  echo "fake go: comparison fixture: $1" >&2
  exit ${comparisonFixtureErrorStatus}
}
if [[ "$subcommand" == "breaking" ]]; then
  while [[ $# -gt 0 && "$1" != "--against" ]]; do
    shift
  done
  if [[ $# -lt 2 || -z "$2" || "$2" == --* ]]; then
    comparison_fixture_error "missing --against value"
  fi
  against="$(cd -- "$2" 2>/dev/null && pwd -P)" ||
    comparison_fixture_error "--against is not a directory"
  scratch="$(cd -- "$TMPDIR" && pwd -P)"
  if [[ "$against" != "$scratch/"*/protos || ! -f "$against/buf.yaml" ]]; then
    comparison_fixture_error "--against must point to the extracted protos tree under TMPDIR"
  fi
  if ! cmp -s "$FAKE_GO_BASE_SCHEMA" "$against/${schemaPath}"; then
    comparison_fixture_error "archived schema differs from the base schema"
  fi
  if [[ "\${FAKE_GO_FAIL:-}" == "breaking" ]]; then
    echo "fake go: breaking failure" >&2
    exit ${breakingFailureStatus}
  fi
  exit 0
fi
if [[ "$subcommand" != "generate" ]]; then
  exit 0
fi
out=""
ts=0
schemas=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --template) ts=1; shift 2 ;;
    --path) schemas="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ "$ts" -eq 1 ]]; then
  mkdir -p "$out/$schemas/v1"
  echo "ts $schemas" > "$out/$schemas/v1/gen_pb.ts"
else
  mkdir -p "$out/delino/ownsand/v1"
  echo "go" > "$out/delino/ownsand/v1/gen.pb.go"
fi
`;

// Paths with a space and an apostrophe, the characters that broke the former
// single-quoted trap text.
const scratchNames = ["it's a tmp dir", "with space"];

function write(file, content) {
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, content);
}

function commitFixture(repo, env, message) {
  execFileSync("git", ["add", "."], { cwd: repo, env });
  execFileSync(
    "git",
    [
      "-c",
      "user.name=Fixture",
      "-c",
      "user.email=fixture@example.com",
      "commit",
      "--quiet",
      "-m",
      message,
    ],
    { cwd: repo, env },
  );
  return execFileSync("git", ["rev-parse", "HEAD"], {
    cwd: repo,
    env,
    encoding: "utf8",
  }).trim();
}

// Creates a committed repository with the real script, the bindings that the
// fake go generates, a fake protoc-gen-es per TypeScript target and the fake
// go in a bin directory. `scratch` is the TMPDIR of every script run.
function fixture(scratchName) {
  const dir = mkdtempSync(join(tmpdir(), "protos-scratch-"));
  const repo = join(dir, "repo");
  const bin = join(dir, "bin");
  const scratch = join(dir, scratchName);
  mkdirSync(scratch);
  const env = isolateGitEnvironment({ ...process.env });
  env.PATH = `${bin}${delimiter}${env.PATH}`;
  env.TMPDIR = scratch;
  delete env.FAKE_GO_FAIL;

  mkdirSync(repo);
  execFileSync("git", ["init", "--quiet", repo], { env });
  mkdirSync(join(repo, "scripts"));
  copyFileSync(protosScript, join(repo, "scripts", "protos.sh"));
  chmodSync(join(repo, "scripts", "protos.sh"), 0o755);
  write(join(repo, "protos", "buf.yaml"), "version: v2\n");
  write(join(repo, "protos", schemaPath), baseSchema);
  write(join(repo, "protos", "delino", "ownsand", "v1", "gen.pb.go"), "go\n");
  for (const pkg of ["ownsand", "accounts", "sandrun"]) {
    const pkgDir = join(repo, "protos", "delino", pkg);
    write(join(pkgDir, "buf.gen.yaml"), "version: v2\n");
    write(join(pkgDir, "v1", "gen_pb.ts"), `ts delino/${pkg}\n`);
    const plugin = join(pkgDir, "node_modules", ".bin", "protoc-gen-es");
    write(plugin, "#!/bin/sh\n");
    chmodSync(plugin, 0o755);
  }
  write(join(bin, "go"), fakeGo);
  chmodSync(join(bin, "go"), 0o755);
  const baseSha = commitFixture(repo, env, "fixture base");
  return { dir, repo, scratch, env, baseSha };
}

function breakingFixture(scratchName) {
  const f = fixture(scratchName);
  f.env.FAKE_GO_BASE_SCHEMA = join(f.dir, "expected-base.proto");
  write(f.env.FAKE_GO_BASE_SCHEMA, baseSchema);
  write(join(f.repo, "protos", schemaPath), headSchema);
  commitFixture(f.repo, f.env, "change fixture schema");
  return f;
}

function run({ repo, env }, args, extraEnv = {}) {
  return spawnSync(join(repo, "scripts", "protos.sh"), args, {
    cwd: repo,
    env: { ...env, ...extraEnv },
    encoding: "utf8",
  });
}

// Runs the script and asserts the exit status, a diagnostic on stderr and an
// empty scratch directory, so the script left no temporary directory behind.
function assertRun(f, args, extraEnv, { status, stderr }) {
  const result = run(f, args, extraEnv);
  assert.equal(result.status, status, `${result.stdout}${result.stderr}`);
  assert.match(result.stderr, stderr);
  assert.doesNotMatch(result.stderr, /unexpected EOF|syntax error/);
  assert.deepEqual(readdirSync(f.scratch), []);
}

for (const scratchName of scratchNames) {
  const label = `TMPDIR "${scratchName}"`;

  test(
    `generate removes its scratch directory on success (${label})`,
    { skip },
    (t) => {
      const f = fixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(f, ["generate"], {}, { status: 0, stderr: /^$/ });
    },
  );

  test(
    `generate removes its scratch directory and keeps the exit status on failure (${label})`,
    { skip },
    (t) => {
      const f = fixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(
        f,
        ["generate"],
        { FAKE_GO_FAIL: "generate" },
        { status: generateFailureStatus, stderr: /fake go: generate failure/ },
      );
    },
  );

  test(
    `check removes its scratch directory on success (${label})`,
    { skip },
    (t) => {
      const f = fixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(f, ["check"], {}, { status: 0, stderr: /^$/ });
    },
  );

  test(
    `check removes its scratch directory and keeps the exit status when generation fails (${label})`,
    { skip },
    (t) => {
      const f = fixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(
        f,
        ["check"],
        { FAKE_GO_FAIL: "generate" },
        { status: generateFailureStatus, stderr: /fake go: generate failure/ },
      );
    },
  );

  test(
    `check removes its scratch directory and keeps the exit status on binding drift (${label})`,
    { skip },
    (t) => {
      const f = fixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      write(
        join(f.repo, "protos", "delino", "ownsand", "v1", "gen.pb.go"),
        "drifted\n",
      );
      assertRun(
        f,
        ["check"],
        {},
        {
          status: 1,
          stderr: /committed binding differs from the generated output/,
        },
      );
    },
  );

  test(
    `breaking removes its scratch directory on success (${label})`,
    { skip },
    (t) => {
      const f = breakingFixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(f, ["breaking", f.baseSha], {}, { status: 0, stderr: /^$/ });
    },
  );

  test(
    `breaking removes its scratch directory and keeps the exit status on failure (${label})`,
    { skip },
    (t) => {
      const f = breakingFixture(scratchName);
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertRun(
        f,
        ["breaking", f.baseSha],
        { FAKE_GO_FAIL: "breaking" },
        { status: breakingFailureStatus, stderr: /fake go: breaking failure/ },
      );
    },
  );
}
