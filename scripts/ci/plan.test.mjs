// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";
import {
  GoReason,
  MAIN_PUSH_EVENT,
  MAIN_REF,
  isMainPush,
  main,
  plan,
  workspaceRoots,
} from "./plan.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));

// The exported planner functions and the fixture helpers run git in this
// process, so they inherit its environment. A developer's global or system Git
// configuration (commit.gpgsign, ...) and the GIT_* variables of an enclosing
// Git hook invocation (GIT_DIR, GIT_INDEX_FILE, ...) must not reach them;
// isolate it before any test runs.
isolateGitEnvironment(process.env);

// Environment for a spawned planner CLI: an isolated copy of the current
// environment, taken at spawn time. Load-time isolation alone cannot cover a
// GIT_* variable set after this module loads, and the production planner
// intentionally honors GIT_DIR. Drops inherited GITHUB_* variables: a
// push-to-main runner sets GITHUB_EVENT_NAME and GITHUB_REF, which would force
// go=true.
function plannerEnvironment(extra = {}) {
  return {
    ...isolateGitEnvironment(
      Object.fromEntries(
        Object.entries(process.env).filter(
          ([key]) => !key.startsWith("GITHUB_"),
        ),
      ),
    ),
    ...extra,
  };
}

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), "ci-plan-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) =>
    execFileSync("git", args, {
      cwd,
      encoding: "utf8",
      env: isolateGitEnvironment({ ...process.env }),
    }).trim();
  git("init", "--quiet");
  git("config", "user.name", "CI fixture");
  git("config", "user.email", "fixture@example.invalid");
  git("config", "core.hooksPath", ".no-hooks");
  const write = (path, content = "fixture\n") => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), content);
  };
  const commit = () => {
    git("add", ".");
    git("commit", "--quiet", "--allow-empty", "-m", "fixture");
    return git("rev-parse", "HEAD");
  };
  write("LICENSE");
  const base = commit();
  return { cwd, git, write, commit, base };
}

test("planner skips empty comparisons and non-validation paths", (t) => {
  const f = fixture(t);
  assert.deepEqual(plan(f.cwd, f.base, f.base), {
    quality: false,
    format: false,
    fixtures: false,
    go: false,
    e2e: false,
  });
  f.write("assets/image.svg");
  assert.deepEqual(plan(f.cwd, f.base, f.commit()), {
    quality: false,
    format: false,
    fixtures: false,
    go: false,
    e2e: false,
  });
});

test("documentation changes select formatting without fixtures", (t) => {
  const f = fixture(t);
  // Windows rejects control characters such as a line feed in filenames, so it
  // keeps the space and Unicode coverage; other platforms also cover a newline.
  f.write(
    process.platform === "win32"
      ? "docs/space name 한국어.md"
      : "docs/space name\n한국어.md",
  );
  assert.deepEqual(plan(f.cwd, f.base, f.commit()), {
    quality: true,
    format: true,
    fixtures: false,
    go: false,
    e2e: false,
  });
});

test("documentation and source paths select formatting in any extension case", (t) => {
  for (const path of [
    "notes.MD",
    "Component.TSX",
    "data.JSON",
    "docs/Guide.Md",
  ]) {
    const f = fixture(t);
    f.write(path);
    assert.deepEqual(plan(f.cwd, f.base, f.commit()), {
      quality: true,
      format: true,
      fixtures: false,
      go: false,
      e2e: false,
    });
  }
  // An unsupported extension stays unformatted whatever its case.
  const f = fixture(t);
  f.write("NOTES.TXT");
  assert.equal(plan(f.cwd, f.base, f.commit()).format, false);
});

test("every shared fixture input selects tests", async (t) => {
  for (const path of [
    ".github/workflows/CI.yml",
    ".nvmrc",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "rstack.config.ts",
    "lefthook.yml",
    "scripts/install-git-hooks.sh",
    "scripts/ci/plan.mjs",
    "scripts/ci/format-changed.test.mjs",
    "turbo.json",
    "apps/delino-accounts/turbo.json",
    "servers/accounts-api/turbo.json",
    "packages/delino-ui/turbo.json",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      const result = plan(f.cwd, f.base, f.commit());
      assert.equal(result.quality, true);
      assert.equal(result.fixtures, true);
    });
  }
});

test("only turbo.json files select Turbo configuration fixtures", (t) => {
  const f = fixture(t);
  f.write("apps/example/not-turbo.json");
  f.write("apps/example/turbo.json.example");
  f.write("apps/example/turbo.json/readme.txt");
  f.write("turbo.json/readme.txt");
  const result = plan(f.cwd, f.base, f.commit());
  assert.equal(result.fixtures, false);
});

test("only root and direct workspace package turbo.json select fixtures", async (t) => {
  // Turbo reads a package configuration only directly inside a workspace
  // package, so a `turbo.json` at any other depth is not a fixture input.
  for (const path of [
    "docs/turbo.json",
    "apps/foo/bar/turbo.json",
    "packages/x/src/turbo.json",
    "apps/turbo.json",
    "cmds/example/turbo.json",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      const result = plan(f.cwd, f.base, f.commit());
      assert.equal(result.fixtures, false);
    });
  }
  // Only app packages select e2e suites, so a stray turbo.json elsewhere does
  // not.
  const f = fixture(t);
  f.write("docs/turbo.json");
  assert.equal(plan(f.cwd, f.base, f.commit()).e2e, false);
});

test("deleted and renamed turbo.json files select fixtures", (t) => {
  const f = fixture(t);
  f.write("packages/old-lib/turbo.json");
  f.write("apps/old-app/turbo.json");
  f.write("servers/old-server/turbo.json");
  const base = f.commit();
  f.git("rm", "--quiet", "packages/old-lib/turbo.json");
  // --no-renames reports a moved file as a deletion plus an addition.
  f.git("mv", "apps/old-app/turbo.json", "docs-only.txt");
  const result = plan(f.cwd, base, f.commit());
  assert.equal(result.quality, true);
  assert.equal(result.fixtures, true);

  const g = fixture(t);
  g.write("servers/old-server/turbo.json");
  const renameBase = g.commit();
  g.write("servers/new-server/readme.txt", "kept\n");
  g.git("mv", "servers/old-server/turbo.json", "servers/new-server/turbo.json");
  assert.equal(plan(g.cwd, renameBase, g.commit()).fixtures, true);

  // A deleted turbo.json at an unsupported depth stays unselected.
  const h = fixture(t);
  h.write("packages/x/src/turbo.json");
  const deleteBase = h.commit();
  h.git("rm", "--quiet", "packages/x/src/turbo.json");
  assert.equal(plan(h.cwd, deleteBase, h.commit()).fixtures, false);
});

test("workspace package manifests select fixtures", async (t) => {
  for (const path of [
    "apps/delino-accounts/package.json",
    "apps/delino-docs/package.json",
    "apps/delino-site/package.json",
    "servers/accounts-api/package.json",
    "packages/new-lib/package.json",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      const result = plan(f.cwd, f.base, f.commit());
      assert.equal(result.quality, true);
      assert.equal(result.fixtures, true);
    });
  }
});

test("deleted and renamed workspace manifests select fixtures", (t) => {
  const f = fixture(t);
  f.write("packages/old-lib/package.json");
  f.write("apps/old-app/package.json");
  const base = f.commit();
  f.git("rm", "--quiet", "packages/old-lib/package.json");
  // --no-renames reports a moved manifest as a deletion plus an addition.
  f.git("mv", "apps/old-app/package.json", "docs-only.txt");
  const result = plan(f.cwd, base, f.commit());
  assert.equal(result.quality, true);
  assert.equal(result.fixtures, true);

  const g = fixture(t);
  g.write("apps/old-app/package.json");
  const renameBase = g.commit();
  g.write("apps/new-app/readme.txt", "kept\n");
  g.git("mv", "apps/old-app/package.json", "apps/new-app/package.json");
  assert.equal(plan(g.cwd, renameBase, g.commit()).fixtures, true);
});

test("only direct workspace package manifests select fixtures", (t) => {
  const f = fixture(t);
  f.write("apps/delino-accounts/src/package.json");
  f.write("apps/delino-accounts/src/index.css");
  f.write("apps/package.json");
  f.write("apps/delino-accounts/package.json.example");
  f.write("cmds/example/package.json");
  f.write("docs/package.json");
  const result = plan(f.cwd, f.base, f.commit());
  assert.equal(result.fixtures, false);
});

const WORKSPACE_MANIFEST_NAME = "pnpm-workspace.yaml";
const WORKSPACE_PACKAGES_KEY = "packages:";
// A planner-supported workspace glob is exactly "<root>/*": one plain
// directory name followed by a single-level wildcard. Anything else (negation,
// "**", nested roots, relative prefixes, bare directories) is not understood by
// the planner's workspace manifest selector.
const WORKSPACE_GLOB_PATTERN = /^([^*/\s!]+)\/\*$/;

// Returns the workspace roots listed in the `packages:` block of a
// pnpm-workspace.yaml text. Accepts the valid YAML spellings of a list entry
// (single or double quotes, trailing comments). Fails closed: an entry that is
// not a "<root>/*" glob throws instead of being skipped, so the cross-check
// below cannot pass while the manifest and `workspaceRoots` differ.
function workspaceGlobRoots(manifest) {
  const lines = manifest.split("\n");
  const start = lines.findIndex(
    (line) => line.trimEnd() === WORKSPACE_PACKAGES_KEY,
  );
  assert.notEqual(
    start,
    -1,
    `${WORKSPACE_MANIFEST_NAME} has no packages block`,
  );
  const roots = [];
  for (const line of lines.slice(start + 1)) {
    // Blank and comment-only lines, at any indentation, do not end the block.
    if (/^\s*(#.*)?$/.test(line)) {
      continue;
    }
    if (/^\S/.test(line)) {
      break;
    }
    const entryMatch = line.match(/^\s*-\s+(.+)$/);
    if (!entryMatch) {
      continue;
    }
    let entry = entryMatch[1].replace(/\s+#.*$/, "").trim();
    const quoted = entry.match(/^(['"])(.*)\1$/);
    if (quoted) {
      entry = quoted[2];
    }
    const globMatch = entry.match(WORKSPACE_GLOB_PATTERN);
    if (!globMatch) {
      throw new Error(
        `unsupported ${WORKSPACE_MANIFEST_NAME} workspace entry: ${entry}`,
      );
    }
    roots.push(globMatch[1]);
  }
  if (roots.length === 0) {
    throw new Error(`${WORKSPACE_MANIFEST_NAME} packages block has no entries`);
  }
  return roots;
}

function sortedWorkspaceRootsMatch(manifest) {
  assert.deepEqual(
    [...workspaceRoots].sort(),
    workspaceGlobRoots(manifest).sort(),
  );
}

test("workspace manifest roots match pnpm-workspace.yaml", () => {
  sortedWorkspaceRootsMatch(
    readFileSync(join(root, WORKSPACE_MANIFEST_NAME), "utf8"),
  );
});

test("workspace glob parser accepts quotes and trailing comments", () => {
  assert.deepEqual(
    workspaceGlobRoots(
      "packages:\n  - apps/*\n  - 'crates/*'\n  - \"tools/*\"\n  - libs/*  # note\n",
    ),
    ["apps", "crates", "tools", "libs"],
  );
});

test("workspace glob parser fails closed on unsupported entries", async (t) => {
  for (const entry of ["packages/**", "'!excluded/*'", "apps/a", "./apps/*"]) {
    await t.test(entry, () => {
      assert.throws(
        () => workspaceGlobRoots(`packages:\n  - apps/*\n  - ${entry}\n`),
        (error) =>
          error.message.includes(entry.replaceAll("'", "")) &&
          error.message.includes(WORKSPACE_MANIFEST_NAME),
      );
    });
  }
});

test("workspace glob parser reads only the packages block", () => {
  assert.deepEqual(
    workspaceGlobRoots(
      "packages:\n  - apps/*\nonlyBuiltDependencies:\n  - esbuild\n",
    ),
    ["apps"],
  );
});

test("workspace glob parser skips comment and blank lines inside the packages block", () => {
  assert.deepEqual(
    workspaceGlobRoots(
      "packages:\n  - apps/*\n# column-zero comment\n\n    # indented comment\n  - crates/*\nonlyBuiltDependencies:\n# trailing comment\n  - esbuild\n",
    ),
    ["apps", "crates"],
  );
});

test("workspace roots cross-check detects a root after a column-zero comment", () => {
  const current = readFileSync(join(root, WORKSPACE_MANIFEST_NAME), "utf8");
  assert.throws(
    () =>
      sortedWorkspaceRootsMatch(
        `${current.trimEnd()}\n# additional workspace roots\n  - crates/*\n`,
      ),
    assert.AssertionError,
  );
});

test("workspace glob parser rejects an empty packages block", () => {
  assert.throws(() => workspaceGlobRoots("packages:\n"));
});

test("workspace roots cross-check detects extra roots in any spelling", async (t) => {
  const current = readFileSync(join(root, WORKSPACE_MANIFEST_NAME), "utf8");
  for (const entry of [
    "crates/*",
    "'crates/*'",
    '"crates/*"',
    "crates/*  # note",
  ]) {
    await t.test(entry, () => {
      assert.throws(
        () => sortedWorkspaceRootsMatch(`${current.trimEnd()}\n  - ${entry}\n`),
        assert.AssertionError,
      );
    });
  }
});

test("Go sources and module files select Go checks", async (t) => {
  for (const path of [
    "servers/example/main.go",
    "servers/example/go.mod",
    "cmds/example/go.sum",
    "go.mod",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      assert.equal(plan(f.cwd, f.base, f.commit()).go, true);
    });
  }
  const f = fixture(t);
  f.write("servers/example/README.md");
  f.write("servers/example/go.mod.example");
  assert.equal(plan(f.cwd, f.base, f.commit()).go, false);
});

test("app paths and shared workspace inputs select e2e suites", async (t) => {
  for (const path of [
    "apps/delino-accounts/src/App.tsx",
    "apps/delino-docs/docs/guide/introduction.md",
    "apps/delino-site/e2e.config.ts",
    ".github/workflows/CI.yml",
    ".nvmrc",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "turbo.json",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      assert.equal(plan(f.cwd, f.base, f.commit()).e2e, true);
    });
  }
  const f = fixture(t);
  f.write("apps/AGENTS.md");
  f.write("docs/apps-delino-site-foundation.md");
  f.write("servers/accounts-api/main.go");
  assert.equal(plan(f.cwd, f.base, f.commit()).e2e, false);
});

test("deleted Go files still select Go checks", (t) => {
  const f = fixture(t);
  f.write("servers/example/old.go");
  const base = f.commit();
  f.git("rm", "--quiet", "servers/example/old.go");
  assert.equal(plan(f.cwd, base, f.commit()).go, true);
});

test("deleted inputs and renamed paths still select affected fixtures", (t) => {
  const f = fixture(t);
  f.write("scripts/ci/old.mjs");
  f.write("old.txt");
  const base = f.commit();
  f.git("rm", "scripts/ci/old.mjs");
  f.git("mv", "old.txt", "new.md");
  assert.deepEqual(plan(f.cwd, base, f.commit()), {
    quality: true,
    format: true,
    fixtures: true,
    go: false,
    e2e: false,
  });
});

test("deleted documentation has no remaining formatting input", (t) => {
  const f = fixture(t);
  f.write("docs/old.md");
  const base = f.commit();
  f.git("rm", "docs/old.md");
  assert.deepEqual(plan(f.cwd, base, f.commit()), {
    quality: false,
    format: false,
    fixtures: false,
    go: false,
    e2e: false,
  });
});

test("planner rejects invalid comparisons instead of emitting a skip plan", (t) => {
  const f = fixture(t);
  for (const base of [undefined, "main", "0".repeat(40), "a".repeat(40)]) {
    assert.throws(() => plan(f.cwd, base, f.base));
  }
});

test("planner publishes booleans and records exact comparison and checkout", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  const output = join(f.cwd, "output");
  const summary = join(f.cwd, "summary");
  main(
    {
      CI_BASE: f.base,
      CI_HEAD: head,
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: summary,
    },
    f.cwd,
  );
  assert.equal(
    readFileSync(output, "utf8"),
    "quality=true\nformat=true\nfixtures=false\ngo=false\ne2e=false\n",
  );
  const record = JSON.parse(
    readFileSync(summary, "utf8").slice("CI plan: ".length),
  );
  assert.equal(record.base, f.base);
  assert.equal(record.head, head);
  assert.equal(record.validationRevision, head);
});

test("planner accepts more than 1 MiB of changed paths", (t) => {
  const f = fixture(t);
  // Build the head tree directly in the object database so that no files are
  // written to disk. The .txt names keep the formatter selection empty.
  const blob = execFileSync("git", ["hash-object", "-w", "--stdin"], {
    cwd: f.cwd,
    encoding: "utf8",
    input: "",
  }).trim();
  const entries = Array.from(
    { length: 5300 },
    (_, index) =>
      `100644 ${blob}\tbulk/${String(index).padStart(5, "0")}-${"x".repeat(190)}.txt\n`,
  ).join("");
  f.git("read-tree", f.base);
  execFileSync("git", ["update-index", "--index-info"], {
    cwd: f.cwd,
    input: entries,
  });
  const tree = f.git("write-tree");
  const head = f.git("commit-tree", tree, "-p", f.base, "-m", "bulk");
  // Guard the fixture so it cannot silently shrink below Node's 1 MiB default.
  const listed = execFileSync(
    "git",
    ["diff", "--name-only", "--no-renames", "-z", f.base, head, "--"],
    { cwd: f.cwd, maxBuffer: 32 * 1024 * 1024 },
  );
  assert.ok(listed.length > 1024 * 1024);
  assert.deepEqual(plan(f.cwd, f.base, head), {
    quality: false,
    format: false,
    fixtures: false,
    go: false,
    e2e: false,
  });
});

test("planner CLI runs through physical and symlinked script paths", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  const links = mkdtempSync(join(tmpdir(), "ci-plan-link-"));
  t.after(() => rmSync(links, { recursive: true, force: true }));
  symlinkSync(
    join(root, "scripts/ci"),
    join(links, "ci"),
    process.platform === "win32" ? "junction" : "dir",
  );
  for (const script of [
    join(root, "scripts/ci/plan.mjs"),
    join(links, "ci/plan.mjs"),
  ]) {
    const output = join(f.cwd, "cli-output");
    const summary = join(f.cwd, "cli-summary");
    rmSync(output, { force: true });
    const result = spawnSync(process.execPath, [script], {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: summary,
      }),
    });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /"event":"ci_plan"/, script);
    assert.equal(
      readFileSync(output, "utf8"),
      "quality=true\nformat=true\nfixtures=false\ngo=false\ne2e=false\n",
      script,
    );
  }
});

const eventMatrix = [
  { name: "push to main", event: "push", ref: "refs/heads/main", main: true },
  { name: "push to another branch", event: "push", ref: "refs/heads/dev" },
  {
    name: "push to a main-like branch",
    event: "push",
    ref: "refs/heads/main2",
  },
  { name: "pull request", event: "pull_request", ref: "refs/pull/1/merge" },
  { name: "manual dispatch", event: "workflow_dispatch", ref: MAIN_REF },
  { name: "main tag-like ref on push", event: "push", ref: "refs/tags/main" },
  { name: "missing event context" },
];

test("main-push event constants and detection are exact", () => {
  assert.equal(MAIN_PUSH_EVENT, "push");
  assert.equal(MAIN_REF, "refs/heads/main");
  for (const row of eventMatrix) {
    assert.equal(
      isMainPush({ GITHUB_EVENT_NAME: row.event, GITHUB_REF: row.ref }),
      row.main === true,
      row.name,
    );
  }
});

for (const row of eventMatrix) {
  test(`planner go output for ${row.name} with a documentation-only range`, (t) => {
    const f = fixture(t);
    f.write("docs/guide.md");
    const head = f.commit();
    const output = join(f.cwd, "output");
    const summary = join(f.cwd, "summary");
    const logged = [];
    t.mock.method(console, "log", (line) => logged.push(line));
    main(
      {
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: summary,
        GITHUB_EVENT_NAME: row.event,
        GITHUB_REF: row.ref,
      },
      f.cwd,
    );
    // Only the go selection depends on the event; every other output still
    // follows the comparison.
    assert.equal(
      readFileSync(output, "utf8"),
      `quality=true\nformat=true\nfixtures=false\ngo=${row.main === true}\ne2e=false\n`,
    );
    const record = JSON.parse(logged.join(""));
    assert.equal(record.go, row.main === true);
    assert.equal(record.goReason, row.main ? GoReason.MainPush : GoReason.Diff);
    assert.equal(
      JSON.parse(readFileSync(summary, "utf8").slice("CI plan: ".length))
        .goReason,
      record.goReason,
    );
  });
}

test("Go changes select go for every event and keep the diff reason", (t) => {
  const f = fixture(t);
  f.write("servers/example/main.go");
  const head = f.commit();
  for (const row of eventMatrix) {
    const output = join(
      f.cwd,
      `output-${row.event}-${row.ref}`.replaceAll("/", "_"),
    );
    const logged = [];
    t.mock.method(console, "log", (line) => logged.push(line));
    main(
      {
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
        GITHUB_EVENT_NAME: row.event,
        GITHUB_REF: row.ref,
      },
      f.cwd,
    );
    assert.match(readFileSync(output, "utf8"), /\ngo=true\n/, row.name);
    assert.equal(JSON.parse(logged.at(-1)).goReason, GoReason.Diff, row.name);
    t.mock.restoreAll();
  }
});

test("main push keeps an empty comparison selecting only Go", (t) => {
  const f = fixture(t);
  const output = join(f.cwd, "output");
  t.mock.method(console, "log", () => {});
  main(
    {
      CI_BASE: f.base,
      CI_HEAD: f.base,
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
      GITHUB_EVENT_NAME: MAIN_PUSH_EVENT,
      GITHUB_REF: MAIN_REF,
    },
    f.cwd,
  );
  assert.equal(
    readFileSync(output, "utf8"),
    "quality=false\nformat=false\nfixtures=false\ngo=true\ne2e=false\n",
  );
});

test("planner CLI forces go for a main push and keeps pull request selection", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  for (const [event, ref, go] of [
    [MAIN_PUSH_EVENT, MAIN_REF, "true"],
    ["pull_request", "refs/pull/1/merge", "false"],
    ["workflow_dispatch", MAIN_REF, "false"],
  ]) {
    const output = join(f.cwd, `cli-${event}`);
    const result = spawnSync(
      process.execPath,
      [join(root, "scripts/ci/plan.mjs")],
      {
        cwd: f.cwd,
        encoding: "utf8",
        env: plannerEnvironment({
          CI_BASE: f.base,
          CI_HEAD: head,
          GITHUB_OUTPUT: output,
          GITHUB_STEP_SUMMARY: join(f.cwd, "cli-summary"),
          GITHUB_EVENT_NAME: event,
          GITHUB_REF: ref,
        }),
      },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(output, "utf8"),
      `quality=true\nformat=true\nfixtures=false\ngo=${go}\ne2e=false\n`,
      event,
    );
  }
});

test("planner CLI ignores GIT_* variables set after the suite loaded", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  // Set after module load, so only the explicit environment copy handed to the
  // spawned planner can keep it out of the child. The production planner
  // honors GIT_DIR, so a leaked value would fail the comparison.
  const hostile = {
    GIT_DIR: join(f.cwd, "no-such-git-dir"),
    GIT_INDEX_FILE: join(f.cwd, "no-such-index"),
    GIT_CONFIG_GLOBAL: join(f.cwd, "no-such-config"),
  };
  const saved = Object.fromEntries(
    Object.keys(hostile).map((key) => [key, process.env[key]]),
  );
  t.after(() => {
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = value;
      }
    }
  });
  Object.assign(process.env, hostile);
  const output = join(f.cwd, "cli-output");
  const result = spawnSync(
    process.execPath,
    [join(root, "scripts/ci/plan.mjs")],
    {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: join(f.cwd, "cli-summary"),
      }),
    },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.equal(
    readFileSync(output, "utf8"),
    "quality=true\nformat=true\nfixtures=false\ngo=false\ne2e=false\n",
  );
});

// Read-only text check: the single `go` planner output must keep driving both
// the Go job and the aggregate, so the planner's main-push selection is
// enforced by ci-result without a workflow edit.
test("CI workflow reads the planner go output for the Go job and the aggregate", () => {
  const workflow = readFileSync(join(root, ".github/workflows/CI.yml"), "utf8");
  assert.match(workflow, /^\s+go: \$\{\{ steps\.plan\.outputs\.go \}\}$/m);
  assert.match(workflow, /^\s+if: needs\.changes\.outputs\.go == 'true'$/m);
  assert.match(
    workflow,
    /^\s+GO_PLANNED: \$\{\{ needs\.changes\.outputs\.go \}\}$/m,
  );
  assert.match(
    workflow,
    /\.go\.result == "skipped" and \$go_planned == "false"/,
  );
});

test("planner skips a non-UTF-8 gitlink instead of failing", (t) => {
  const raw = Buffer.concat([
    Buffer.from([0x63, 0x61, 0x66, 0xe9]),
    Buffer.from(".md"),
  ]);
  const f = fixture(t);
  try {
    // A gitlink is a directory in the worktree and has no blob to format.
    mkdirSync(Buffer.concat([Buffer.from(`${f.cwd}/`), raw]));
  } catch {
    return t.skip("file system cannot hold a non-UTF-8 name");
  }
  execFileSync("git", ["update-index", "-z", "--add", "--index-info"], {
    cwd: f.cwd,
    env: isolateGitEnvironment({ ...process.env }),
    input: Buffer.concat([
      Buffer.from(`160000 ${"1".repeat(40)}\t`),
      raw,
      Buffer.alloc(1),
    ]),
  });
  const listing = execFileSync("git", ["ls-files", "-z"], {
    cwd: f.cwd,
    encoding: "buffer",
  });
  if (!listing.includes(raw))
    return t.skip("file system cannot hold a non-UTF-8 name");
  f.git("commit", "--quiet", "-m", "add a non-UTF-8 gitlink");
  const head = f.git("rev-parse", "HEAD");
  assert.equal(plan(f.cwd, f.base, head).format, false);
  const output = join(f.cwd, "cli-output");
  const result = spawnSync(
    process.execPath,
    [join(root, "scripts/ci/plan.mjs")],
    {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: join(f.cwd, "cli-summary"),
      }),
    },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /"event":"ci_plan"/u);
});

// A path name that is not valid UTF-8 (the Latin-1 name `caf\xe9<suffix>`).
// Linux file systems hold any byte; APFS and Windows reject or normalize it,
// so the tests below attempt the write and skip when the exact bytes are not
// listed back by Git.
const latin1Name = (suffix, prefix = "") =>
  Buffer.concat([
    Buffer.from(prefix),
    Buffer.from([0x63, 0x61, 0x66, 0xe9]),
    Buffer.from(suffix),
  ]);
const unsupportedFileSystem = "file system cannot hold a non-UTF-8 name";

// Writes each raw name (a Buffer, possibly under a valid directory prefix) in
// the fixture. Returns false when the file system cannot hold them.
function writeRawNames(f, ...names) {
  try {
    for (const raw of names) {
      const slash = raw.lastIndexOf(0x2f);
      if (slash !== -1) {
        mkdirSync(join(f.cwd, raw.subarray(0, slash).toString()), {
          recursive: true,
        });
      }
      writeFileSync(Buffer.concat([Buffer.from(`${f.cwd}/`), raw]), "x\n");
    }
  } catch {
    return false;
  }
  const listing = execFileSync("git", ["ls-files", "--others", "-z"], {
    cwd: f.cwd,
    encoding: "buffer",
  });
  return names.every((raw) => listing.includes(raw));
}

test("planner defers the verdict for a non-UTF-8 path and skips unsupported ones", (t) => {
  const f = fixture(t);
  if (!writeRawNames(f, latin1Name(".txt"), latin1Name(".ts")))
    return t.skip(unsupportedFileSystem);
  // The unsupported extension alone selects nothing.
  rmSync(Buffer.concat([Buffer.from(`${f.cwd}/`), latin1Name(".ts")]));
  const unsupported = plan(f.cwd, f.base, f.commit());
  assert.equal(unsupported.format, false);
  // A formattable name selects formatting: the planner cannot evaluate the
  // shared exclusions without Rstack, so the `quality` job's formatter gives
  // the verdict. It neither throws nor reports a missing path.
  assert.ok(writeRawNames(f, latin1Name(".ts")));
  const head = f.commit();
  assert.equal(plan(f.cwd, f.base, head).format, true);
});

test("planner selects formatting for non-UTF-8 names whether or not Rstack excludes them", (t) => {
  for (const raw of [latin1Name(".ts", "vendor/"), latin1Name(".ts", "src_")]) {
    const f = fixture(t);
    if (!writeRawNames(f, raw)) return t.skip(unsupportedFileSystem);
    assert.deepEqual(plan(f.cwd, f.base, f.commit()), {
      quality: true,
      format: true,
      fixtures: false,
      go: false,
      e2e: false,
    });
  }
});

test("planner without installed dependencies publishes format=true for non-UTF-8 names", (t) => {
  // Copy the planner next to no `node_modules` so Rstack cannot be resolved,
  // as in the `changes` job, which installs nothing.
  const bare = mkdtempSync(join(tmpdir(), "ci-plan-bare-"));
  t.after(() => rmSync(bare, { recursive: true, force: true }));
  for (const file of ["plan.mjs", "format-changed.mjs"]) {
    copyFileSync(join(root, "scripts/ci", file), join(bare, file));
  }
  for (const raw of [latin1Name(".ts", "vendor/"), latin1Name(".ts", "src_")]) {
    const f = fixture(t);
    if (!writeRawNames(f, raw)) return t.skip(unsupportedFileSystem);
    const head = f.commit();
    const output = join(f.cwd, "cli-output");
    const summary = join(f.cwd, "cli-summary");
    const result = spawnSync(process.execPath, [join(bare, "plan.mjs")], {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: summary,
      }),
    });
    assert.equal(result.status, 0, result.stderr);
    const records = result.stdout.split("\n").filter(Boolean);
    assert.equal(records.length, 1, result.stdout);
    assert.equal(JSON.parse(records[0]).event, "ci_plan");
    assert.match(readFileSync(output, "utf8"), /^format=true$/mu);
    assert.match(readFileSync(summary, "utf8"), /^CI plan: /u);
  }
});

// Runs the planner CLI with `extra` as its comparison and output variables.
function runPlannerCli(f, extra) {
  const output = join(f.cwd, "cli-output");
  const summary = join(f.cwd, "cli-summary");
  const result = spawnSync(
    process.execPath,
    [join(root, "scripts/ci/plan.mjs")],
    {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: summary,
        ...extra,
      }),
    },
  );
  return { result, output, summary };
}

// Git's own `fatal:` line can precede the record, as it does for the
// formatter, so parse the last stderr line.
function lastStderrRecord(result) {
  const lines = result.stderr.split("\n").filter(Boolean);
  return JSON.parse(lines.at(-1));
}

test("planner CLI reports a missing comparison revision as a ci_format_error record", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  // Well-formed 40-hex SHA that names no object: `git cat-file -e` fails.
  const { result, output, summary } = runPlannerCli(f, {
    CI_BASE: "de".repeat(20),
    CI_HEAD: head,
  });
  assert.equal(result.status, 2, result.stderr);
  assert.equal(result.stdout, "", "no ci_plan record on failure");
  const record = lastStderrRecord(result);
  assert.equal(record.event, "ci_format_error");
  assert.equal(typeof record.message, "string");
  assert.doesNotMatch(result.stderr, /\n\s+at /u, "no stack trace");
  assert.equal(existsSync(output), false, "no outputs on failure");
  assert.equal(existsSync(summary), false, "no summary on failure");
});

test("planner CLI reports an unset or malformed comparison as a ci_format_error record", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  for (const comparison of [
    { CI_HEAD: head },
    { CI_BASE: "not-a-sha", CI_HEAD: head },
    { CI_BASE: "0".repeat(40), CI_HEAD: head },
  ]) {
    const { result, output, summary } = runPlannerCli(f, comparison);
    const label = JSON.stringify(comparison);
    assert.equal(result.status, 2, `${label}: ${result.stderr}`);
    assert.equal(result.stdout, "", label);
    const lines = result.stderr.split("\n").filter(Boolean);
    assert.equal(lines.length, 1, `${label}: ${result.stderr}`);
    const record = JSON.parse(lines[0]);
    assert.equal(record.event, "ci_format_error", label);
    assert.match(record.message, /nonzero 40-character comparison SHA/u, label);
    assert.equal(existsSync(output), false, label);
    assert.equal(existsSync(summary), false, label);
  }
});

test("planner CLI fails with one ci_format_error and publishes nothing when a publication target is unusable", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  const comparison = { CI_BASE: f.base, CI_HEAD: head };
  const directory = join(f.cwd, "a-directory");
  mkdirSync(directory);
  const cases = [
    // `undefined` overrides the default and spawnSync leaves the variable out.
    { env: { GITHUB_OUTPUT: undefined }, message: /GITHUB_OUTPUT/u },
    {
      env: { GITHUB_STEP_SUMMARY: undefined },
      message: /GITHUB_STEP_SUMMARY/u,
    },
    { env: { GITHUB_OUTPUT: directory }, message: /EISDIR|directory/iu },
  ];
  for (const { env, message } of cases) {
    const label = JSON.stringify(Object.keys(env));
    const { result, output, summary } = runPlannerCli(f, {
      ...comparison,
      ...env,
    });
    assert.equal(result.status, 2, `${label}: ${result.stderr}`);
    assert.equal(result.stdout, "", `${label}: no ci_plan record`);
    const lines = result.stderr.split("\n").filter(Boolean);
    assert.equal(lines.length, 1, `${label}: ${result.stderr}`);
    const record = JSON.parse(lines[0]);
    assert.equal(record.event, "ci_format_error", label);
    assert.match(record.message, message, label);
    if (!("GITHUB_OUTPUT" in env && env.GITHUB_OUTPUT !== undefined)) {
      assert.equal(existsSync(output), false, `${label}: no outputs`);
    }
    assert.equal(existsSync(summary), false, `${label}: no summary`);
  }
});

test("planner CLI still exits 0 with one ci_plan record and no error record on success", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  const { result, output } = runPlannerCli(f, {
    CI_BASE: f.base,
    CI_HEAD: head,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  assert.equal(result.stdout.split("\n").filter(Boolean).length, 1);
  assert.match(result.stdout, /"event":"ci_plan"/u);
  assert.equal(
    readFileSync(output, "utf8"),
    "quality=true\nformat=true\nfixtures=false\ngo=false\ne2e=false\n",
  );
});

test("planner CLI reports a non-UTF-8 checkout path accurately", (t) => {
  if (process.platform === "win32") return t.skip(unsupportedFileSystem);
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  // Move the checkout to `<parent>/caf\xe9`; the test process cannot address
  // it with a string, so the planner is started through `/bin/sh` with a glob.
  const parent = mkdtempSync(join(tmpdir(), "ci-plan-root-"));
  t.after(() => rmSync(parent, { recursive: true, force: true }));
  try {
    renameSync(
      f.cwd,
      Buffer.concat([Buffer.from(`${parent}/`), latin1Name("")]),
    );
  } catch {
    return t.skip(unsupportedFileSystem);
  }
  const env = plannerEnvironment({
    CI_BASE: f.base,
    CI_HEAD: head,
    GITHUB_OUTPUT: join(parent, "output"),
    GITHUB_STEP_SUMMARY: join(parent, "summary"),
  });
  const result = spawnSync(
    "/bin/sh",
    [
      "-c",
      'cd -- caf* && exec "$@"',
      "sh",
      process.execPath,
      join(root, "scripts/ci/plan.mjs"),
    ],
    { cwd: parent, env, encoding: "utf8" },
  );
  assert.equal(result.status, 2, result.stderr);
  assert.equal(result.stdout, "");
  const lines = result.stderr.split("\n").filter(Boolean);
  assert.equal(lines.length, 1, result.stderr);
  const record = JSON.parse(lines[0]);
  assert.equal(record.event, "ci_format_error");
  assert.match(record.message, /checkout path is not valid UTF-8/u);
  assert.match(record.message, /caf\\xe9/u);
  assert.match(record.message, /Move or rename the checkout/u);
  assert.doesNotMatch(record.message, /ENOENT/u);
  assert.equal(existsSync(join(parent, "output")), false);
  assert.equal(existsSync(join(parent, "summary")), false);
});
