// SPDX-License-Identifier: Apache-2.0
//
// Tests `scripts/protos.sh generate` and `check` with fake generators in a
// throwaway Git repository. The tests cover replacement, drift, and binding
// preservation when a generator or a file listing fails.
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
import { delimiter, dirname, join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const repositoryRoot = fileURLToPath(new URL("../..", import.meta.url));
const protosScript = join(repositoryRoot, "scripts", "protos.sh");

// The script is bash and CI runs it on Linux only.
const skip = process.platform === "win32" && "scripts/protos.sh needs bash";

// Fake `go`. `go run <module>@<version> <buf args>` behaves like buf:
// `lint` and `format` succeed, and `generate --output <dir>` writes bindings per
// language (the TypeScript template runs are the ones with --template; each
// writes under the schema path that --path names). The FAKE_GO_FAIL variable
// selects a failing generator: `go`, `ts` (every TypeScript target) or
// `ts-accounts` (only the delino/accounts target, which runs second).
const fakeGo = `#!/usr/bin/env bash
set -euo pipefail
shift 2
subcommand="$1"
shift
if [[ "$subcommand" == "format" || "$subcommand" == "lint" ]]; then
  exit 0
fi
if [[ "$subcommand" != "generate" ]]; then
  echo "fake go: unexpected buf subcommand $subcommand" >&2
  exit 2
fi
out=""
ts=0
schemas="go"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --template) ts=1; shift 2 ;;
    --path) schemas="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ "\${FAKE_GO_EMPTY_TARGET:-}" == "$schemas" ]]; then
  exit 0
fi
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

// Emit the first result from the real listing, then fail. Both phases use
// the same ordering so check can encounter identical partial lists.
const fakeFind = `#!/usr/bin/env bash
set -euo pipefail
export PATH="$FAKE_FIND_PATH"
phase=""
case "$PWD" in
  "$FAKE_PROTOS_DIR") phase="existing" ;;
  "$TMPDIR"/*) phase="staged" ;;
esac
target=""
case "$1" in
  .) target="go" ;;
  ./delino/accounts) target="delino/accounts" ;;
esac
if [[ -n "$phase" && -n "$target" && "\${@: -1}" != "-delete" ]] &&
  [[ "\${FAKE_FIND_FAIL:-}" == "$phase:$target" || "\${FAKE_FIND_FAIL:-}" == "both:$target" ]]; then
  files="$(find "$@" | LC_ALL=C sort)"
  printf '%s\\n' "\${files%%$'\\n'*}"
  exit 7
fi
exec find "$@"
`;

function write(file, content) {
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, content);
}

// TypeScript targets of the script: the schema directories under protos/,
// which are also the binding package directories.
const tsPackages = ["ownsand", "accounts", "sandrun"];

// Creates a repository with the real script, committed (old) bindings of both
// languages and every TypeScript target, and the fake go in a bin directory.
function fixture({ parent = tmpdir(), plugin = true } = {}) {
  const dir = mkdtempSync(join(parent, "protos-generate-"));
  const repo = join(dir, "repo");
  const bin = join(dir, "bin");
  const scratch = join(dir, "tmp");
  mkdirSync(scratch);
  const env = isolateGitEnvironment({ ...process.env });
  env.FAKE_FIND_PATH = env.PATH;
  env.FAKE_PROTOS_DIR = join(repo, "protos");
  env.PATH = `${bin}${delimiter}${env.PATH}`;
  env.TMPDIR = scratch;
  delete env.FAKE_GO_FAIL;
  delete env.FAKE_FIND_FAIL;
  delete env.FAKE_GO_EMPTY_TARGET;

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
  write(join(bin, "find"), fakeFind);
  chmodSync(join(bin, "find"), 0o755);
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
    const dir = join(repo, "protos", "delino", pkg);
    add(
      dir,
      (file) =>
        !dirname(relative(dir, file)).split(sep).includes("node_modules"),
    );
  }
  return result;
}

function run({ repo, env }, command, extraEnv = {}) {
  return spawnSync(join(repo, "scripts", "protos.sh"), [command], {
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
  "the snapshot excludes only package-local node_modules directories",
  { skip },
  (t) => {
    const parent = mkdtempSync(join(tmpdir(), "protos-generate-parent-"));
    t.after(() => rmSync(parent, { recursive: true, force: true }));
    const fixtureParent = join(parent, "node_modules");
    mkdirSync(fixtureParent);
    const f = fixture({ parent: fixtureParent });
    const packagePath = join("protos", "delino", "accounts");
    const dependency = join(packagePath, "node_modules", "dependency.txt");
    const notes = join(packagePath, "node_modules-notes", "notes.txt");
    write(join(f.repo, dependency), "dependency\n");
    write(join(f.repo, notes), "keep these notes\n");

    const snapshot = bindings(f.repo);
    assert.equal(snapshot.has(dependency), false);
    assert.equal(snapshot.get(notes), "keep these notes\n");
  },
);

test(
  "a failing Go generator preserves the Go and TypeScript bindings",
  { skip },
  (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    assertPreserved(
      f,
      () => run(f, "generate", { FAKE_GO_FAIL: "go" }),
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
      () => run(f, "generate", { FAKE_GO_FAIL: "ts" }),
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
      () => run(f, "generate"),
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
      () => run(f, "generate", { FAKE_GO_FAIL: "ts-accounts" }),
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
      () => run(f, "generate"),
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
    const result = run(f, "generate");
    assert.equal(result.status, 0, result.stderr);
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
          join("protos", "delino", "sandrun", "v1", "new_pb.ts"),
          "new ts delino/sandrun\n",
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

for (const { target, label, binding, extra } of [
  {
    target: "go",
    label: "Go bindings",
    binding: "protos/delino/ownsand/v1/new.pb.go",
    extra: "zz_extra.pb.go",
  },
  {
    target: "delino/accounts",
    label: "TypeScript bindings in protos/delino/accounts",
    binding: "protos/delino/accounts/v1/new_pb.ts",
    extra: "zz_extra_pb.ts",
  },
]) {
  for (const phase of ["existing", "staged"]) {
    test(
      `generate preserves every binding when listing ${phase} ${label} fails`,
      { skip },
      (t) => {
        const f = fixture();
        t.after(() => rmSync(f.dir, { recursive: true, force: true }));
        assertPreserved(
          f,
          () => run(f, "generate", { FAKE_FIND_FAIL: `${phase}:${target}` }),
          new RegExp(`cannot list ${phase} ${label}`),
        );
      },
    );
  }

  test(
    `generate preserves every binding when the generator stages no ${label}`,
    { skip },
    (t) => {
      const f = fixture();
      t.after(() => rmSync(f.dir, { recursive: true, force: true }));
      assertPreserved(
        f,
        () => run(f, "generate", { FAKE_GO_EMPTY_TARGET: target }),
        new RegExp(`no staged ${label}`),
      );
    },
  );

  test(`check rejects matching partial lists of ${label}`, { skip }, (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    const generated = run(f, "generate");
    assert.equal(generated.status, 0, generated.stderr);
    write(join(f.repo, dirname(binding), extra), "extra binding\n");
    const checked = run(f, "check", { FAKE_FIND_FAIL: `both:${target}` });
    assert.equal(checked.status, 1, checked.stdout);
    assert.match(checked.stderr, new RegExp(`cannot list existing ${label}`));
    assert.deepEqual(readdirSync(f.scratch), []);
  });

  test(`check compares the paths and bytes of ${label}`, { skip }, (t) => {
    const f = fixture();
    t.after(() => rmSync(f.dir, { recursive: true, force: true }));
    const generated = run(f, "generate");
    assert.equal(generated.status, 0, generated.stderr);
    const identical = run(f, "check");
    assert.equal(identical.status, 0, identical.stderr);

    const extraPath = join(f.repo, dirname(binding), extra);
    write(extraPath, "extra binding\n");
    const pathsDiffer = run(f, "check");
    assert.equal(pathsDiffer.status, 1, pathsDiffer.stdout);
    assert.match(
      pathsDiffer.stderr,
      new RegExp(`the set of committed ${label} differs`),
    );
    rmSync(extraPath);

    write(join(f.repo, binding), "changed content\n");
    const bytesDiffer = run(f, "check");
    assert.equal(bytesDiffer.status, 1, bytesDiffer.stdout);
    assert.ok(
      bytesDiffer.stderr.includes(
        `committed binding differs from the generated output: ${binding}`,
      ),
      bytesDiffer.stderr,
    );
    assert.deepEqual(readdirSync(f.scratch), []);
  });
}
