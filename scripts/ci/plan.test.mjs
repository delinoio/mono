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
  MAIN_PUSH_EVENT,
  MAIN_REF,
  OutputReason,
  SCHEDULE_EVENT,
  isMainPush,
  isScheduledMain,
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
// go, fixtures and e2e.
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
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
  });
  f.write("assets/image.svg");
  assert.deepEqual(plan(f.cwd, f.base, f.commit()), {
    quality: false,
    format: false,
    fixtures: false,
    go: false,
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
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
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
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
      rust: false,
      e2e: false,
      workspace: false,
      workflows: false,
    });
  }
  // An unsupported extension stays unformatted whatever its case.
  const f = fixture(t);
  f.write("NOTES.TXT");
  assert.equal(plan(f.cwd, f.base, f.commit()).format, false);
});

test("every shared fixture input selects tests", async (t) => {
  for (const path of [
    ".changeset/config.json",
    ".changeset/brave-cats-run.md",
    ".github/workflows/CI.yml",
    ".github/workflows/deploy.yml",
    ".nvmrc",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "rstack.config.ts",
    "lefthook.yml",
    "scripts/install-git-hooks.sh",
    "scripts/droplet/start-caddy.sh",
    "scripts/protos.sh",
    "scripts/dev/start-portless-proxy.mjs",
    "scripts/ci/plan.mjs",
    "scripts/ci/format-changed.test.mjs",
    "scripts/dev/with-infisical.mjs",
    "scripts/dev/with-infisical.test.mjs",
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
    "protos/delino/ownsand/package.json",
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
  f.write("protos/delino/package.json");
  f.write("protos/delino/ownsand/v1/package.json");
  const result = plan(f.cwd, f.base, f.commit());
  assert.equal(result.fixtures, false);
});

const WORKSPACE_MANIFEST_NAME = "pnpm-workspace.yaml";
const WORKSPACE_PACKAGES_KEY = "packages:";
// A planner-supported workspace glob is exactly "<root>/*": a plain directory
// path of one or more segments (such as "apps" or "protos/delino") followed by
// a single-level wildcard. Anything else (negation, "**", relative prefixes,
// bare directories) is not understood by the planner's workspace manifest
// selector.
const WORKSPACE_GLOB_PATTERN =
  /^([^*/\s!.][^*/\s!]*(?:\/[^*/\s!.][^*/\s!]*)*)\/\*$/;

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

test("workspace glob parser accepts nested roots", () => {
  assert.deepEqual(
    workspaceGlobRoots("packages:\n  - apps/*\n  - protos/delino/*\n"),
    ["apps", "protos/delino"],
  );
});

test("workspace glob parser fails closed on unsupported entries", async (t) => {
  for (const entry of [
    "packages/**",
    "'!excluded/*'",
    "apps/a",
    "./apps/*",
    "protos/*/x/*",
    "protos/./x/*",
  ]) {
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

test("Go sources, module files, protobuf inputs and OwnSand end-to-end inputs select Go checks", async (t) => {
  for (const path of [
    "servers/example/main.go",
    "servers/example/go.mod",
    "cmds/example/go.sum",
    "go.mod",
    "protos/delino/example/v1/example.proto",
    "protos/buf.yaml",
    "protos/buf.gen.yaml",
    "protos/buf.lock",
    "scripts/protos.sh",
    "protos/delino/ownsand/buf.gen.yaml",
    "protos/delino/ownsand/package.json",
    "protos/delino/ownsand/tsconfig.json",
    "protos/delino/ownsand/tsconfig.build.json",
    "protos/delino/ownsand/v1/common_pb.ts",
    "packages/ownsand-sdk/package.json",
    "packages/ownsand-sdk/src/client.ts",
    "packages/ownsand-sdk/src/e2e.test.ts",
    "packages/ownsand-sdk/rstest.config.ts",
    "packages/ownsand-sdk/tsconfig.json",
    "protos/delino/accounts/buf.gen.yaml",
    "protos/delino/accounts/package.json",
    "protos/delino/accounts/v1/credit_pb.ts",
    ".github/workflows/CI.yml",
    ".nvmrc",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "servers/ownsand-api/internal/db/migrations/0001_initial.sql",
    "servers/accounts-api/internal/db/migrations/00001_billing_accounts.sql",
    "go/ownsand/relaybinding/testdata/request-digest-vectors.json",
    "go/example/testdata/case.golden",
    "cmds/example/internal/testdata/input.json",
    "internal/example/testdata/input.txt",
    "servers/example/testdata/fixture.json",
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
  f.write("protos/AGENTS.md");
  f.write("protos/delino/ownsand/README.md");
  f.write("protos/delino/ownsand/v1/notes_pb.md");
  f.write("apps/delino-accounts/package.json");
  f.write("docs/buf.yaml");
  f.write("packages/ownsand-sdk-example/src/client.ts");
  f.write("packages/delino-ui/package.json");
  f.write("apps/delino-accounts/src/App.tsx");
  f.write("rstest.config.ts");
  f.write("servers/ownsand-api/README.md");
  f.write(".github/workflows/deploy.yml");
  f.write("docs/testdata/example.json");
  f.write("go/example/mytestdata/example.json");
  f.write("go/example/testdata.json");
  assert.equal(plan(f.cwd, f.base, f.commit()).go, false);
});

test("The relay request digest vectors select Go checks and workspace jobs", (t) => {
  const f = fixture(t);
  f.write("go/ownsand/relaybinding/testdata/request-digest-vectors.json");
  const result = plan(f.cwd, f.base, f.commit());
  assert.equal(result.go, true);
  assert.equal(result.workspace, true);
  assert.equal(result.rust, false);
  assert.equal(result.workflows, false);
});

test("Other Go testdata selects Go checks but not workspace jobs", (t) => {
  const f = fixture(t);
  f.write("go/example/testdata/case.golden");
  const result = plan(f.cwd, f.base, f.commit());
  assert.equal(result.go, true);
  assert.equal(result.workspace, false);
});

test("Rust sources, Cargo files, toolchain files and Tauri crates select Rust checks", async (t) => {
  for (const path of [
    "apps/example/src-tauri/src/lib.rs",
    "apps/example/src-tauri/tauri.conf.json",
    "apps/example/src-tauri/capabilities/default.json",
    "apps/example/src-tauri/Cargo.toml",
    "crates/example/src/lib.rs",
    "crates/example/build.rs",
    "Cargo.toml",
    "Cargo.lock",
    "rust-toolchain",
    ".rustfmt.toml",
    ".github/workflows/CI.yml",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      assert.equal(plan(f.cwd, f.base, f.commit()).rust, true);
    });
  }
  const f = fixture(t);
  f.write("apps/example/src/App.tsx");
  f.write("apps/example/src-tauri.md");
  f.write("crates/example/README.md");
  f.write("docs/Cargo.toml.example");
  f.write("crates/rust-toolchain.md");
  assert.equal(plan(f.cwd, f.base, f.commit()).rust, false);
});

test("deleted Rust files still select Rust checks", (t) => {
  const f = fixture(t);
  f.write("crates/example/src/old.rs");
  const base = f.commit();
  f.git("rm", "--quiet", "crates/example/src/old.rs");
  assert.equal(plan(f.cwd, base, f.commit()).rust, true);
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

test("workspace package paths and root task inputs select workspace tasks", async (t) => {
  for (const path of [
    "apps/delino-accounts/src/App.tsx",
    "packages/delino-ui/src/slider.tsx",
    "packages/ownsand-sdk/rstest.config.ts",
    "servers/accounts-api/scripts/dev.test.mjs",
    "servers/ownsand-api/internal/api/server.go",
    ".github/workflows/CI.yml",
    ".nvmrc",
    "go.mod",
    "go.sum",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "turbo.json",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      assert.equal(plan(f.cwd, f.base, f.commit()).workspace, true);
    });
  }
  // Files directly inside a workspace root are not inside a package, and
  // paths outside the workspace roots are not workspace task inputs.
  const f = fixture(t);
  f.write("apps/AGENTS.md");
  f.write("packages/README.md");
  f.write("servers/LICENSE");
  f.write("docs/packages-delino-ui-foundation.md");
  f.write("crates/example/src/lib.rs");
  f.write("cmds/tool/main.go");
  f.write("scripts/ci/example.mjs");
  f.write(".github/workflows/deploy.yml");
  assert.equal(plan(f.cwd, f.base, f.commit()).workspace, false);
});

test("workflows and local actions select the workflow lint", async (t) => {
  for (const path of [
    ".github/workflows/CI.yml",
    ".github/workflows/deploy.yml",
    ".github/workflows/new.yaml",
    ".github/actions/setup/action.yml",
  ]) {
    await t.test(path, (t) => {
      const f = fixture(t);
      f.write(path);
      assert.equal(plan(f.cwd, f.base, f.commit()).workflows, true);
    });
  }
  const f = fixture(t);
  f.write(".github/AGENTS.md");
  f.write("docs/repository-workflow-contract.md");
  f.write("scripts/ci/plan.mjs");
  f.write("workflows/example.yml");
  assert.equal(plan(f.cwd, f.base, f.commit()).workflows, false);
});

test("deleted workflows still select the workflow lint", (t) => {
  const f = fixture(t);
  f.write(".github/workflows/old.yml");
  const base = f.commit();
  f.git("rm", "--quiet", ".github/workflows/old.yml");
  assert.equal(plan(f.cwd, base, f.commit()).workflows, true);
});

test("deleted workspace package files still select workspace tasks", (t) => {
  const f = fixture(t);
  f.write("packages/delino-ui/src/old.tsx");
  const base = f.commit();
  f.git("rm", "--quiet", "packages/delino-ui/src/old.tsx");
  assert.equal(plan(f.cwd, base, f.commit()).workspace, true);
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
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
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
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
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
    "quality=true\nformat=true\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=false\nrustPlatforms=false\n",
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
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
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
      "quality=true\nformat=true\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=false\nrustPlatforms=false\n",
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
  {
    name: "scheduled main run",
    event: "schedule",
    ref: MAIN_REF,
    schedule: true,
  },
  {
    name: "schedule on another ref",
    event: "schedule",
    ref: "refs/heads/dev",
  },
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

// The reason that the planner records for a platform output of an eventMatrix
// row whose comparison selects (or does not select) the matching Linux output.
function platformReasonFor(row, selectedByDiff) {
  if (row.schedule === true) return OutputReason.Schedule;
  if (row.main === true && selectedByDiff) return OutputReason.MainPush;
  return OutputReason.Diff;
}

test("scheduled-run event constant and detection are exact", () => {
  assert.equal(SCHEDULE_EVENT, "schedule");
  for (const row of eventMatrix) {
    assert.equal(
      isScheduledMain({ GITHUB_EVENT_NAME: row.event, GITHUB_REF: row.ref }),
      row.schedule === true,
      row.name,
    );
  }
});

for (const row of eventMatrix) {
  test(`planner outputs for ${row.name} with a documentation-only range`, (t) => {
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
    // Only a push to main forces fixtures, go and e2e (and so quality), and
    // only the scheduled main run forces the platform jobs; format still
    // follows the comparison for every event.
    const mainPush = row.main === true;
    const scheduled = row.schedule === true;
    assert.equal(
      readFileSync(output, "utf8"),
      `quality=true\nformat=true\nfixtures=${mainPush}\ngo=${mainPush}\nrust=${mainPush}\ne2e=${mainPush}\nworkspace=${mainPush}\nworkflows=${mainPush}\ngoPlatforms=${scheduled}\nrustPlatforms=${scheduled}\n`,
    );
    const record = JSON.parse(logged.join(""));
    assert.equal(record.go, mainPush);
    assert.equal(record.rust, mainPush);
    assert.equal(record.fixtures, mainPush);
    assert.equal(record.e2e, mainPush);
    assert.equal(record.workspace, mainPush);
    assert.equal(record.workflows, mainPush);
    const reason = mainPush ? OutputReason.MainPush : OutputReason.Diff;
    assert.equal(record.goReason, reason);
    assert.equal(record.rustReason, reason);
    assert.equal(record.fixturesReason, reason);
    assert.equal(record.e2eReason, reason);
    assert.equal(record.workspaceReason, reason);
    assert.equal(record.workflowsReason, reason);
    const platformReason = scheduled
      ? OutputReason.Schedule
      : OutputReason.Diff;
    assert.equal(record.goPlatformsReason, platformReason);
    assert.equal(record.rustPlatformsReason, platformReason);
    assert.deepEqual(
      JSON.parse(readFileSync(summary, "utf8").slice("CI plan: ".length)),
      record,
    );
  });
}

test("a later documentation-only main push still selects the fixtures and e2e of a cancelled predecessor", (t) => {
  const f = fixture(t);
  // Push A selects fixtures (package.json) and e2e (an app path).
  f.write("package.json", "{}\n");
  f.write("apps/example/src/index.ts");
  const first = f.commit();
  // Push B, which cancelled A's run, only changes documentation and compares
  // from A's head only.
  f.write("docs/example.md");
  const second = f.commit();
  assert.deepEqual(plan(f.cwd, first, second), {
    quality: true,
    format: true,
    fixtures: false,
    go: false,
    rust: false,
    e2e: false,
    workspace: false,
    workflows: false,
  });
  const run = (event, ref) => {
    const output = join(f.cwd, `output-${event}`.replaceAll("/", "_"));
    rmSync(output, { force: true });
    t.mock.method(console, "log", () => {});
    main(
      {
        CI_BASE: first,
        CI_HEAD: second,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
        GITHUB_EVENT_NAME: event,
        GITHUB_REF: ref,
      },
      f.cwd,
    );
    t.mock.restoreAll();
    return readFileSync(output, "utf8");
  };
  assert.equal(
    run(MAIN_PUSH_EVENT, MAIN_REF),
    "quality=true\nformat=true\nfixtures=true\ngo=true\nrust=true\ne2e=true\nworkspace=true\nworkflows=true\ngoPlatforms=false\nrustPlatforms=false\n",
  );
  assert.equal(
    run("pull_request", "refs/pull/1/merge"),
    "quality=true\nformat=true\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=false\nrustPlatforms=false\n",
  );
});

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
    const record = JSON.parse(logged.at(-1));
    assert.equal(record.goReason, OutputReason.Diff, row.name);
    // Every event except a main push runs the Go platform job for Go changes.
    assert.equal(record.goPlatforms, row.main !== true, row.name);
    assert.equal(
      record.goPlatformsReason,
      platformReasonFor(row, true),
      row.name,
    );
    // A Go-only range does not select fixtures or e2e, so only a main push
    // forces them.
    assert.equal(
      record.fixturesReason,
      row.main ? OutputReason.MainPush : OutputReason.Diff,
      row.name,
    );
    assert.equal(
      record.e2eReason,
      row.main ? OutputReason.MainPush : OutputReason.Diff,
      row.name,
    );
    t.mock.restoreAll();
  }
});

test("fixture and e2e changes keep the diff reason on a main push", (t) => {
  const f = fixture(t);
  // lefthook.yml selects fixtures without selecting the Go checks.
  f.write("lefthook.yml");
  f.write("apps/example/src/index.ts");
  const head = f.commit();
  const logged = [];
  t.mock.method(console, "log", (line) => logged.push(line));
  main(
    {
      CI_BASE: f.base,
      CI_HEAD: head,
      GITHUB_OUTPUT: join(f.cwd, "output"),
      GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
      GITHUB_EVENT_NAME: MAIN_PUSH_EVENT,
      GITHUB_REF: MAIN_REF,
    },
    f.cwd,
  );
  const record = JSON.parse(logged.at(-1));
  // The diff selects fixtures, e2e and the workspace tasks on its own; only go
  // and rust are forced.
  assert.equal(record.fixturesReason, OutputReason.Diff);
  assert.equal(record.e2eReason, OutputReason.Diff);
  assert.equal(record.workspace, true);
  assert.equal(record.workspaceReason, OutputReason.Diff);
  assert.equal(record.goReason, OutputReason.MainPush);
  assert.equal(record.rustReason, OutputReason.MainPush);
});

test("main push selects every whole-suite check for an empty comparison", (t) => {
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
    "quality=true\nformat=false\nfixtures=true\ngo=true\nrust=true\ne2e=true\nworkspace=true\nworkflows=true\ngoPlatforms=false\nrustPlatforms=false\n",
  );
});

test("scheduled main run selects only the platform jobs for an empty comparison", (t) => {
  const f = fixture(t);
  const output = join(f.cwd, "output");
  const logged = [];
  t.mock.method(console, "log", (line) => logged.push(line));
  main(
    {
      CI_BASE: f.base,
      CI_HEAD: f.base,
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
      GITHUB_EVENT_NAME: SCHEDULE_EVENT,
      GITHUB_REF: MAIN_REF,
    },
    f.cwd,
  );
  assert.equal(
    readFileSync(output, "utf8"),
    "quality=false\nformat=false\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=true\nrustPlatforms=true\n",
  );
  const record = JSON.parse(logged.at(-1));
  assert.equal(record.goPlatformsReason, OutputReason.Schedule);
  assert.equal(record.rustPlatformsReason, OutputReason.Schedule);
});

test("main push deselects the platform jobs that its Go and Rust changes select", (t) => {
  const f = fixture(t);
  f.write("cmds/example/main_windows.go");
  f.write("crates/example/src/lib.rs");
  const head = f.commit();
  const logged = [];
  t.mock.method(console, "log", (line) => logged.push(line));
  main(
    {
      CI_BASE: f.base,
      CI_HEAD: head,
      GITHUB_OUTPUT: join(f.cwd, "output"),
      GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
      GITHUB_EVENT_NAME: MAIN_PUSH_EVENT,
      GITHUB_REF: MAIN_REF,
    },
    f.cwd,
  );
  const record = JSON.parse(logged.at(-1));
  assert.equal(record.go, true);
  assert.equal(record.rust, true);
  assert.equal(record.goPlatforms, false);
  assert.equal(record.rustPlatforms, false);
  assert.equal(record.goPlatformsReason, OutputReason.MainPush);
  assert.equal(record.rustPlatformsReason, OutputReason.MainPush);
});

test("planner CLI forces whole-suite checks for a main push and keeps pull request selection", (t) => {
  const f = fixture(t);
  f.write("docs/guide.md");
  const head = f.commit();
  for (const [event, ref, forced] of [
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
      `quality=true\nformat=true\nfixtures=${forced}\ngo=${forced}\nrust=${forced}\ne2e=${forced}\nworkspace=${forced}\nworkflows=${forced}\ngoPlatforms=false\nrustPlatforms=false\n`,
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
    "quality=true\nformat=true\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=false\nrustPlatforms=false\n",
  );
});

// Splits CI.yml into its jobs, keyed by job ID, in file order. A job body is
// every following line that is empty or indented by at least four spaces.
function workflowJobs() {
  const workflow = readFileSync(join(root, ".github/workflows/CI.yml"), "utf8");
  const jobsStart = workflow.indexOf("\njobs:\n");
  assert.notEqual(jobsStart, -1, "CI.yml has no jobs section");
  const jobs = new Map();
  for (const match of workflow
    .slice(jobsStart)
    .matchAll(/^ {2}([a-z][a-z0-9-]*):\n((?:(?: {4}.*)?\n)*)/gm)) {
    jobs.set(match[1], match[2]);
  }
  return jobs;
}

// The table value of a job that runs for every event without the planner.
const ALWAYS_RUN = "always";

// The planner output that selects each validation job, as the `job_outputs`
// table of `ci-result` records it.
const expectedJobOutputs = {
  quality: "quality",
  "go-fmt": ALWAYS_RUN,
  "go-vet": "go",
  "go-test": "go",
  protos: "go",
  "go-platforms": "goPlatforms",
  "rust-fmt": ALWAYS_RUN,
  "rust-clippy": "rust",
  "rust-test": "rust",
  "rust-platforms": "rustPlatforms",
  e2e: "e2e",
  "workspace-packages": "workspace",
  "workspace-servers": "workspace",
  workflows: "workflows",
};

function ciResultTable(jobs) {
  const match = /job_outputs='(\{[^']*\})'/.exec(jobs.get("ci-result"));
  assert.ok(match, "ci-result has no job_outputs table");
  return JSON.parse(match[1]);
}

// Read-only text check: each validation job runs only when the planner output
// in the ci-result table selects it, and ci-result needs every validation
// job. The planner's main-push selection is therefore enforced by ci-result
// without a workflow edit.
test("CI workflow aggregates every validation job by the planner output that selects it", () => {
  const jobs = workflowJobs();
  const changes = jobs.get("changes");
  const ciResult = jobs.get("ci-result");
  const validationJobs = [...jobs.keys()].filter(
    (id) => id !== "changes" && id !== "ci-result",
  );
  const table = ciResultTable(jobs);
  assert.deepEqual(table, expectedJobOutputs);
  assert.deepEqual(Object.keys(table).sort(), [...validationJobs].sort());
  const needs = /^ {4}needs:\s*\[([a-z0-9,\s-]+)\]/m.exec(ciResult);
  assert.ok(needs, "ci-result has no needs list");
  assert.deepEqual(needs[1].split(/[\s,]+/).filter(Boolean), [
    "changes",
    ...validationJobs,
  ]);
  assert.match(ciResult, /^ {4}if: always\(\)$/m);
  for (const [id, job] of jobs) {
    assert.match(job, /^ {4}timeout-minutes: \d+$/m, id);
  }
  // The planner must never publish the marker as an output name.
  assert.doesNotMatch(changes, new RegExp(`^ {6}${ALWAYS_RUN}:`, "m"));
  for (const id of validationJobs) {
    const output = table[id];
    const job = jobs.get(id);
    if (output === ALWAYS_RUN) {
      assert.doesNotMatch(job, /^ {4}(needs|if):/m, id);
      continue;
    }
    assert.match(job, /^ {4}needs: changes$/m, id);
    assert.match(
      job,
      new RegExp(
        `^ {4}if: needs\\.changes\\.outputs\\.${output} == 'true'$`,
        "m",
      ),
      id,
    );
    assert.match(
      changes,
      new RegExp(
        `^ {6}${output}: \\$\\{\\{ steps\\.plan\\.outputs\\.${output} \\}\\}$`,
        "m",
      ),
      id,
    );
  }
});

// Runs the ci-result jq check of CI.yml against a `needs` object.
function ciResultPasses(jobs, needs) {
  const ciResult = jobs.get("ci-result");
  const table = /job_outputs='(\{[^']*\})'/.exec(ciResult)[1];
  const program =
    /--argjson outputs "\$job_outputs" '([^']*)' > \/dev\/null/.exec(ciResult);
  assert.ok(program, "ci-result has no jq check");
  const result = spawnSync(
    "jq",
    ["-e", "--argjson", "outputs", table, program[1]],
    { input: JSON.stringify(needs), encoding: "utf8" },
  );
  if (result.error) {
    throw result.error;
  }
  return result.status === 0;
}

// Runs the aggregate check itself. Every GitHub-hosted Ubuntu runner has jq;
// a local run without jq skips this test.
test("CI aggregate passes only successful or unselected skipped jobs", (t) => {
  if (spawnSync("jq", ["--version"]).error) {
    t.skip("jq is not installed");
    return;
  }
  const jobs = workflowJobs();
  const table = ciResultTable(jobs);
  // Planner outputs, plus the marker as an output: a skipped job that runs
  // without the planner must fail even if the planner published the marker.
  const outputs = [...new Set(Object.values(table))];
  const needsOf = (results, planned, changes = "success") => ({
    changes: { result: changes, outputs: planned },
    ...Object.fromEntries(
      Object.entries(results).map(([id, result]) => [
        id,
        { result, outputs: {} },
      ]),
    ),
  });
  const allSelected = Object.fromEntries(outputs.map((o) => [o, "true"]));
  const noneSelected = Object.fromEntries(outputs.map((o) => [o, "false"]));
  const allSucceeded = Object.fromEntries(
    Object.keys(table).map((id) => [id, "success"]),
  );
  const allSkipped = Object.fromEntries(
    Object.keys(table).map((id) => [id, "skipped"]),
  );
  assert.equal(ciResultPasses(jobs, needsOf(allSucceeded, allSelected)), true);
  for (const [id, output] of Object.entries(table)) {
    if (output === ALWAYS_RUN) {
      // A job that runs without the planner must succeed even when the
      // planner selects nothing.
      assert.equal(
        ciResultPasses(
          jobs,
          needsOf({ ...allSucceeded, [id]: "skipped" }, noneSelected),
        ),
        false,
        `${id} skipped`,
      );
    } else {
      assert.equal(
        ciResultPasses(
          jobs,
          needsOf(
            { ...allSucceeded, [id]: "skipped" },
            { ...allSelected, [output]: "false" },
          ),
        ),
        true,
        `${id} skipped without selection`,
      );
    }
    for (const result of ["skipped", "failure", "cancelled"]) {
      assert.equal(
        ciResultPasses(
          jobs,
          needsOf({ ...allSucceeded, [id]: result }, allSelected),
        ),
        false,
        `${id} ${result} with selection`,
      );
    }
  }
  assert.equal(
    ciResultPasses(
      jobs,
      needsOf({ ...allSucceeded, extra: "success" }, allSelected),
    ),
    false,
    "a needed job without a table entry",
  );
  const [firstJob] = Object.keys(table);
  const { [firstJob]: _omitted, ...missingOne } = allSucceeded;
  assert.equal(
    ciResultPasses(jobs, needsOf(missingOne, allSelected)),
    false,
    "a table entry without a needed job",
  );
  assert.equal(
    ciResultPasses(jobs, needsOf(allSkipped, {}, "failure")),
    false,
    "a failed planner",
  );
});

// The protos job runs the protobuf drift and breaking-change checks with the
// planner's comparison base, so it needs the full history.
test("CI workflow checks protobuf schemas in the protos job", () => {
  const jobs = workflowJobs();
  assert.doesNotMatch(jobs.get("go-test"), /scripts\/protos\.sh/);
  const protosJob = jobs.get("protos");
  assert.match(protosJob, /fetch-depth: 0/);
  assert.match(protosJob, /scripts\/protos\.sh check/);
  assert.match(protosJob, /scripts\/protos\.sh breaking "\$COMPARISON_BASE"/);
  assert.match(
    protosJob,
    /COMPARISON_BASE: \$\{\{ needs\.changes\.outputs\.base \}\}/,
  );
});

test("Rust changes select rust for every event and keep the diff reason", (t) => {
  const f = fixture(t);
  f.write("apps/example/src-tauri/src/lib.rs");
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
    assert.match(readFileSync(output, "utf8"), /\nrust=true\n/, row.name);
    const record = JSON.parse(logged.at(-1));
    assert.equal(record.rustReason, OutputReason.Diff, row.name);
    // Every event except a main push runs the Rust platform job for Rust
    // changes.
    assert.equal(record.rustPlatforms, row.main !== true, row.name);
    assert.equal(
      record.rustPlatformsReason,
      platformReasonFor(row, true),
      row.name,
    );
    // A Rust-only range does not select Go, so only a main push forces it.
    assert.equal(
      record.goReason,
      row.main ? OutputReason.MainPush : OutputReason.Diff,
      row.name,
    );
    t.mock.restoreAll();
  }
});

// Read-only text check: each Rust check runs in its own job. The formatting
// job installs only rustfmt and no build packages; the clippy and test jobs
// build the workspace with the Rust cache.
test("CI workflow runs each Rust check in its own job", () => {
  const jobs = workflowJobs();
  const fmtJob = jobs.get("rust-fmt");
  assert.match(fmtJob, /^\s+run: cargo fmt --all --check$/m);
  assert.match(fmtJob, /--profile minimal --component rustfmt$/m);
  assert.doesNotMatch(fmtJob, /libgtk-4-dev|rust-cache|cargo (clippy|test)/);
  const clippyJob = jobs.get("rust-clippy");
  assert.match(
    clippyJob,
    /^\s+run: cargo clippy --workspace --all-targets --locked -- -D warnings$/m,
  );
  const testJob = jobs.get("rust-test");
  assert.match(testJob, /^\s+run: cargo test --workspace --locked$/m);
  for (const job of [clippyJob, testJob]) {
    assert.match(job, /libgtk-4-dev/);
    assert.match(job, /Swatinem\/rust-cache@[0-9a-f]{40}/);
    assert.match(job, /CEF_PATH=\$HOME\/\.cache\/cef/);
  }
});

// Read-only text check: each Go check runs in its own job. Only the test job
// starts Postgres and installs the workspace dependencies that the OwnSand
// end-to-end suite needs, and only it saves the `go.sum` Go cache.
test("CI workflow runs each Go check in its own job", () => {
  const jobs = workflowJobs();
  const fmtJob = jobs.get("go-fmt");
  assert.match(fmtJob, /gofmt -l/);
  assert.match(fmtJob, /^\s+cache: false$/m);
  const vetJob = jobs.get("go-vet");
  assert.match(vetJob, /^\s+run: go vet \.\/\.\.\.$/m);
  assert.match(vetJob, /^\s+cache: false$/m);
  const testJob = jobs.get("go-test");
  assert.match(testJob, /^\s+run: go test -count=1 \.\/\.\.\.$/m);
  assert.match(testJob, /^\s+cache: true$/m);
  assert.match(testJob, /^ {4}services:\n {6}postgres:$/m);
  assert.match(testJob, /^\s+OWNSAND_E2E: "1"$/m);
  assert.match(testJob, /pnpm install --frozen-lockfile --ignore-scripts/);
  for (const job of [fmtJob, vetJob]) {
    assert.doesNotMatch(job, /services:|go test|pnpm install/);
  }
});

// Read-only text check: the platform jobs run the Go and Rust checks on macOS
// and Windows runners, check out the repository line endings on Windows and
// keep the Go suite without the Linux-only Postgres service.
test("CI workflow runs the Go and Rust checks on macOS and Windows", () => {
  const jobs = workflowJobs();
  const goJob = jobs.get("go-platforms");
  const rustJob = jobs.get("rust-platforms");
  for (const job of [goJob, rustJob]) {
    assert.match(job, /^ {6}fail-fast: false$/m);
    assert.match(job, /^\s+runner: macos-latest$/m);
    assert.match(job, /^\s+runner: windows-latest$/m);
    assert.match(job, /^ {4}runs-on: \$\{\{ matrix\.runner \}\}$/m);
    assert.match(job, /^\s+run: git config --global core\.autocrlf false$/m);
    const lineEndings = job.indexOf("core.autocrlf false");
    assert.ok(lineEndings < job.indexOf("actions/checkout@"));
  }
  assert.match(goJob, /^\s+run: go vet \.\/\.\.\.$/m);
  assert.match(
    goJob,
    /GOARCH="\$other_arch" CGO_ENABLED=1 go vet \.\/\.\.\.$/m,
  );
  // Known failing packages leave only the blocking run; the job still tests
  // them without blocking, and it fails when a listed package is gone.
  assert.match(
    goJob,
    /^\s+go test -count=1 "\$\{tested\[@\]\}" \|\| status=\$\?$/m,
  );
  assert.match(goJob, /^\s+if go test -count=1 "\$package"; then$/m);
  assert.match(goJob, /does not exist; remove it from the list/);
  // bash 3.2 on macOS runners fails on an empty array expansion under
  // `set -u`, so an empty list is expanded with the `+` form and skips the
  // non-blocking run.
  assert.match(
    goJob,
    /^\s+for package in \$\{known_failures\[@\]\+"\$\{known_failures\[@\]\}"\}; do$/m,
  );
  assert.match(goJob, /^\s+if \(\(\$\{#skipped\[@\]\} == 0\)\); then$/m);
  assert.doesNotMatch(goJob, /services:|OWNSAND_E2E: "1"/);
  assert.match(
    rustJob,
    /^\s+run: cargo clippy --workspace --all-targets --locked -- -D warnings$/m,
  );
  assert.match(rustJob, /^\s+run: cargo test --workspace --locked$/m);
  assert.match(rustJob, /Swatinem\/rust-cache@[0-9a-f]{40}/);
  assert.match(rustJob, /^\s+key: \$\{\{ matrix\.runner \}\}$/m);
  assert.match(rustJob, /cygpath -w "\$cef_path"/);
});

// Read-only text check: the fixtures step runs only when the planner selects
// fixtures, and it does not repeat the workspace package tests that the
// workspace-packages and workspace-servers jobs run.
test("CI workflow runs the fixtures in the quality job", () => {
  const jobs = workflowJobs();
  assert.match(
    jobs.get("changes"),
    /^ {6}fixtures: \$\{\{ steps\.plan\.outputs\.fixtures \}\}$/m,
  );
  const qualityJob = jobs.get("quality");
  assert.match(
    qualityJob,
    /^\s+if: needs\.changes\.outputs\.fixtures == 'true'$/m,
  );
  assert.match(qualityJob, /run: pnpm test:tooling$/m);
  assert.doesNotMatch(qualityJob, /run: pnpm test$/m);
});

// Read-only text check: the workspace-packages and workspace-servers filters
// are complements, so together they select every workspace package.
test("CI workflow runs every workspace package in the workspace jobs", () => {
  const jobs = workflowJobs();
  const command = "pnpm exec turbo run build lint typecheck test --continue";
  assert.match(
    jobs.get("workspace-packages"),
    new RegExp(`^\\s+run: ${command} '--filter=!\\./servers/\\*'$`, "m"),
  );
  assert.match(
    jobs.get("workspace-servers"),
    new RegExp(`^\\s+run: ${command} '--filter=\\./servers/\\*'$`, "m"),
  );
});

// Read-only text check: the workflows job lints and audits the workflows with
// pinned tools.
test("CI workflow lints and audits the workflows", () => {
  const workflowsJob = workflowJobs().get("workflows");
  assert.match(workflowsJob, /actionlint@v\d+\.\d+\.\d+/);
  assert.match(workflowsJob, /zizmorcore\/zizmor-action@[0-9a-f]{40}/);
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
      rust: false,
      e2e: false,
      workspace: false,
      workflows: false,
    });
  }
});

test("planner without installed dependencies publishes format=true for a path missing from the working tree", (t) => {
  // The planner cannot evaluate the shared exclusions, so it defers a missing
  // formattable path, excluded or not, to the `quality` job's formatter.
  const bare = mkdtempSync(join(tmpdir(), "ci-plan-bare-"));
  t.after(() => rmSync(bare, { recursive: true, force: true }));
  for (const file of ["plan.mjs", "format-changed.mjs"]) {
    copyFileSync(join(root, "scripts/ci", file), join(bare, file));
  }
  for (const missing of ["vendor/lib.ts", "src/lib.ts"]) {
    const f = fixture(t);
    f.write(missing, "const value = 1;\n");
    const base = f.commit();
    f.write(missing, "const value = 2;\n");
    const head = f.commit();
    f.git("update-index", "--skip-worktree", missing);
    rmSync(join(f.cwd, dirname(missing)), { recursive: true });
    const output = join(f.cwd, "cli-output");
    const summary = join(f.cwd, "cli-summary");
    const result = spawnSync(process.execPath, [join(bare, "plan.mjs")], {
      cwd: f.cwd,
      encoding: "utf8",
      env: plannerEnvironment({
        CI_BASE: base,
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
    "quality=true\nformat=true\nfixtures=false\ngo=false\nrust=false\ne2e=false\nworkspace=false\nworkflows=false\ngoPlatforms=false\nrustPlatforms=false\n",
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

// Fixture for the subdirectory tests: the base commit holds a file in every
// start directory, so each directory exists in the working tree and in the
// base. `diff.relative` is set by the repository-local configuration, which
// `isolateGitEnvironment` does not disable.
function subdirectoryFixture(t, relative) {
  const f = fixture(t);
  f.write("servers/x/main.go", "package main\n");
  f.write("crates/foo/src/lib.rs", "// base\n");
  f.write("cmds/tool/main.go", "package main\n");
  f.write(".nvmrc", "20\n");
  f.base = f.commit();
  if (relative) f.git("config", "diff.relative", "true");
  return f;
}

const startDirectories = ["servers", "crates", "cmds/tool"];

for (const relative of [true, false]) {
  const mode = relative ? "with" : "without";

  test(`planner selects the same paths from a subdirectory ${mode} diff.relative`, (t) => {
    const f = subdirectoryFixture(t, relative);
    const comparisons = [
      {
        name: "go file under servers",
        change: () => f.write("servers/x/main.go", "package main\n// change\n"),
        expected: { go: true, fixtures: false, e2e: false, quality: false },
      },
      {
        name: "shared runtime file at the root",
        change: () => f.write(".nvmrc", "22\n"),
        expected: { go: true, fixtures: true, e2e: true, quality: true },
      },
      {
        name: "rust and go files in sibling directories",
        change: () => {
          f.write("crates/foo/src/lib.rs", "// change\n");
          f.write("cmds/tool/main.go", "package main\n// change\n");
        },
        expected: { go: true, rust: true },
      },
    ];
    for (const { name, change, expected } of comparisons) {
      const previous = f.git("rev-parse", "HEAD");
      change();
      const head = f.commit();
      const fromRoot = plan(f.cwd, previous, head);
      for (const [key, value] of Object.entries(expected)) {
        assert.equal(fromRoot[key], value, `${name}: root ${key}`);
      }
      for (const directory of startDirectories) {
        assert.deepEqual(
          plan(join(f.cwd, directory), previous, head),
          fromRoot,
          `${name}: from ${directory}`,
        );
      }
    }
  });

  test(`planner CLI publishes the same outputs from a subdirectory ${mode} diff.relative`, (t) => {
    const f = subdirectoryFixture(t, relative);
    f.write(".nvmrc", "22\n");
    f.write("servers/x/main.go", "package main\n// change\n");
    const head = f.commit();
    const run = (directory, name) => {
      const output = join(f.cwd, `${name}-output`);
      const summary = join(f.cwd, `${name}-summary`);
      const result = spawnSync(
        process.execPath,
        [join(root, "scripts/ci/plan.mjs")],
        {
          cwd: directory,
          env: plannerEnvironment({
            CI_BASE: f.base,
            CI_HEAD: head,
            GITHUB_OUTPUT: output,
            GITHUB_STEP_SUMMARY: summary,
          }),
          encoding: "utf8",
        },
      );
      assert.equal(result.status, 0, result.stderr);
      return {
        output: readFileSync(output, "utf8"),
        record: JSON.parse(result.stdout),
      };
    };
    const fromRoot = run(f.cwd, "root");
    assert.equal(
      fromRoot.output,
      "quality=true\nformat=false\nfixtures=true\ngo=true\nrust=false\ne2e=true\nworkspace=true\nworkflows=false\ngoPlatforms=true\nrustPlatforms=false\n",
    );
    for (const directory of startDirectories) {
      assert.deepEqual(
        run(join(f.cwd, directory), directory.replaceAll("/", "-")),
        fromRoot,
        `from ${directory}`,
      );
    }
  });
}

test("planner main() publishes the same outputs from a subdirectory with diff.relative", (t) => {
  const f = subdirectoryFixture(t, true);
  f.write(".nvmrc", "22\n");
  const head = f.commit();
  const publish = (directory, name) => {
    const output = join(f.cwd, `${name}-output`);
    main(
      {
        CI_BASE: f.base,
        CI_HEAD: head,
        GITHUB_OUTPUT: output,
        GITHUB_STEP_SUMMARY: join(f.cwd, `${name}-summary`),
      },
      directory,
    );
    return readFileSync(output, "utf8");
  };
  assert.equal(
    publish(join(f.cwd, "servers"), "subdirectory"),
    publish(f.cwd, "root"),
  );
});
