// SPDX-License-Identifier: Apache-2.0
//
// Tests `scripts/protos.sh generate` in a throwaway Git repository with a fake
// `go` executable that stands in for `go run buf@<version>`. The tests check
// that a failing generator leaves every committed binding of both languages
// and every TypeScript target byte for byte unchanged, and that a successful
// run replaces the bindings and removes the stale files of both languages.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const repositoryRoot = fileURLToPath(new URL("../..", import.meta.url));
const protosScript = join(repositoryRoot, "scripts", "protos.sh");

// The script is bash and CI runs it on Linux only.
const skip = process.platform === "win32" && "scripts/protos.sh needs bash";

// Fake `go`. `go run <module>@<version> <buf args>` behaves like buf:
// `format` succeeds, and `generate --output <dir>` writes one new binding per
// language (the TypeScript template runs are the ones with --template; each
// writes under the schema path that --path names). The FAKE_GO_FAIL variable
// selects a failing generator: `go`, `ts` (every TypeScript target) or
// `ts-accounts` (only the delino/accounts target, which runs second).
const fakeGo = `#!/usr/bin/env bash
set -euo pipefail
shift 2
subcommand="$1"
shift
if [[ "$subcommand" == "format" ]]; then
  exit 0
fi
if [[ "$subcommand" != "generate" ]]; then
  echo "fake go: unexpected buf subcommand $subcommand" >&2
  exit 2
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
  if [[ "\${FAKE_GO_FAIL:-}" == "ts" ]] ||
    [[ "\${FAKE_GO_FAIL:-}" == "ts-accounts" && "$schemas" == "delino/accounts" ]]; then
    echo "fake go: TypeScript generator failure for $schemas" >&2
    exit 1
  fi
  mkdir -p "$out/$schemas/v1"
  echo "new ts $schemas" > "$out/$schemas/v1/new_pb.ts"
else
  if [[ "\${FAKE_GO_FAIL:-}" == "go" ]]; then
    echo "fake go: Go generator failure" >&2
    exit 1
  fi
  mkdir -p "$out/delino/ownsand/v1/v1connect"
  echo "new go" > "$out/delino/ownsand/v1/new.pb.go"
  echo "new go connect" > "$out/delino/ownsand/v1/v1connect/new.connect.go"
fi
`;

function write(file, content) {
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, content);
}

// TypeScript targets of the script: the schema directories under protos/,
// which are also the binding package directories.
const tsPackages = ["ownsand", "accounts", "tide"];

// Creates a repository with the real script, committed (old) bindings of both
// languages and every TypeScript target, and the fake go in a bin directory.
function fixture({ plugin = true } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "protos-generate-"));
  const repo = join(dir, "repo");
  const bin = join(dir, "bin");
  const scratch = join(dir, "tmp");
  mkdirSync(scratch);
  const env = isolateGitEnvironment({ ...process.env });
  env.PATH = `${bin}${delimiter}${env.PATH}`;
  env.TMPDIR = scratch;

  mkdirSync(repo);
  execFileSync("git", ["init", "--quiet", repo], { env });
  mkdirSync(join(repo, "scripts"));
  copyFileSync(protosScript, join(repo, "scripts", "protos.sh"));
  chmodSync(join(repo, "scripts", "protos.sh"), 0o755);
  write(join(repo, "protos", "buf.yaml"), "version: v2\n");
  write(join(repo, "protos", "delino", "ownsand", "v1", "a.proto"), "x\n");
  write(
    join(repo, "protos", "delino", "ownsand", "v1", "a.pb.go"),
    "old go a\n",
  );
  write(
    join(repo, "protos", "delino", "ownsand", "v1", "stale.pb.go"),
    "old go stale\n",
  );
  write(
    join(
      repo,
      "protos",
      "delino",
      "ownsand",
      "v1",
      "v1connect",
      "a.connect.go",
    ),
    "old go connect\n",
  );
  for (const pkg of tsPackages) {
    const dir = join(repo, "protos", "delino", pkg);
    write(join(dir, "buf.gen.yaml"), "version: v2\n");
    write(join(dir, "package.json"), "{}\n");
    write(join(dir, "v1", "a_pb.ts"), `old ts a ${pkg}\n`);
    write(join(dir, "v1", "stale_pb.ts"), `old ts stale ${pkg}\n`);
    // Files that are not bindings: generation must keep them.
    write(join(dir, "v1", "a.proto"), "x\n");
    write(join(dir, "dist", "v1", "a_pb.ts"), `built ${pkg}\n`);
    if (plugin === true || plugin === pkg) {
      const pluginFile = join(dir, "node_modules", ".bin", "protoc-gen-es");
      write(pluginFile, "#!/bin/sh\n");
      chmodSync(pluginFile, 0o755);
    }
  }
  write(join(bin, "go"), fakeGo);
  chmodSync(join(bin, "go"), 0o755);
  return { dir, repo, scratch, env };
}

function listFiles(dir) {
  const files = [];
  const walk = (current) => {
    for (const entry of readdirSync(current)) {
      const path = join(current, entry);
      if (statSync(path).isDirectory()) {
        walk(path);
      } else {
        files.push(path);
      }
    }
  };
  walk(dir);
  return files.sort();
}

// Maps every generated file of both languages to its content.
function bindings(repo) {
  const result = new Map();
  const add = (dir, accept) => {
    for (const file of listFiles(dir)) {
      if (accept(file)) {
        result.set(relative(repo, file), readFileSync(file, "utf8"));
      }
    }
  };
  add(join(repo, "protos"), (f) => /\.(pb|connect)\.go$/.test(f));
  for (const pkg of tsPackages) {
    add(
      join(repo, "protos", "delino", pkg),
      (f) => !f.includes("node_modules"),
    );
  }
  return result;
}

function generate({ repo, env }, extraEnv = {}) {
  return spawnSync(join(repo, "scripts", "protos.sh"), ["generate"], {
    cwd: repo,
    env: { ...env, ...extraEnv },
    encoding: "utf8",
  });
}

function assertPreserved(f, result, diagnostic) {
  const before = bindings(f.repo);
  const run = result();
  assert.notEqual(run.status, 0, run.stdout);
  assert.match(run.stderr, diagnostic);
  assert.deepEqual(bindings(f.repo), before);
  assert.equal(before.size, 21);
  assert.deepEqual(readdirSync(f.scratch), []);
}

test(
  "a failing Go generator preserves the Go and TypeScript bindings",
  { skip },
  (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => generate(f, { FAKE_GO_FAIL: "go" }),
      /Go generator failure/,
    );
  },
);

test(
  "a failing TypeScript generator preserves the Go and TypeScript bindings",
  { skip },
  (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => generate(f, { FAKE_GO_FAIL: "ts" }),
      /TypeScript generator failure/,
    );
  },
);

test(
  "a missing protoc-gen-es preserves the Go and TypeScript bindings",
  { skip },
  (t) => {
    const f = fixture({ plugin: false });
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => generate(f),
      /protoc-gen-es is missing; run pnpm install/,
    );
  },
);

test(
  "a failing second TypeScript target preserves every binding",
  { skip },
  (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => generate(f, { FAKE_GO_FAIL: "ts-accounts" }),
      /TypeScript generator failure for delino\/accounts/,
    );
  },
);

test(
  "a missing protoc-gen-es of the accounts package preserves every binding",
  { skip },
  (t) => {
    const f = fixture({ plugin: "ownsand" });
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => generate(f),
      /delino\/accounts\/node_modules\/\.bin\/protoc-gen-es is missing/,
    );
  },
);

test(
  "a successful run replaces the bindings and removes stale files",
  { skip },
  (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    const run = generate(f);
    assert.equal(run.status, 0, run.stderr);
    const kept = tsPackages.flatMap((pkg) => {
      const dir = join("protos", "delino", pkg);
      return [
        [join(dir, "buf.gen.yaml"), "version: v2\n"],
        [join(dir, "package.json"), "{}\n"],
        [join(dir, "v1", "a.proto"), "x\n"],
        [join(dir, "dist", "v1", "a_pb.ts"), `built ${pkg}\n`],
      ];
    });
    assert.deepEqual(
      [...bindings(f.repo)].sort(),
      [
        ...kept,
        [
          join("protos", "delino", "ownsand", "v1", "new_pb.ts"),
          "new ts delino/ownsand\n",
        ],
        [
          join("protos", "delino", "accounts", "v1", "new_pb.ts"),
          "new ts delino/accounts\n",
        ],
        [
          join("protos", "delino", "tide", "v1", "new_pb.ts"),
          "new ts delino/tide\n",
        ],
        [join("protos", "delino", "ownsand", "v1", "new.pb.go"), "new go\n"],
        [
          join(
            "protos",
            "delino",
            "ownsand",
            "v1",
            "v1connect",
            "new.connect.go",
          ),
          "new go connect\n",
        ],
      ].sort(),
    );
    assert.deepEqual(readdirSync(f.scratch), []);
  },
);
