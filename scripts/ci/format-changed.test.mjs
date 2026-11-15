// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { createRequire } from "node:module";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  allFormatFiles,
  changedFormatFiles,
  ciEnabled,
  isThrowawayIndex,
  runFormatter,
  stagedFormatFiles,
} from "./format-changed.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const require = createRequire(import.meta.url);
const lefthook = require("lefthook/get-exe.js").getExePath();

// A developer's global or system Git configuration (core.hooksPath,
// commit.gpgsign, ...) and the GIT_* variables of an enclosing Git hook
// invocation (GIT_DIR, GIT_INDEX_FILE, ...) must not change how the fixture
// repositories behave. Mutates and returns `env`.
function isolateGitEnvironment(env) {
  for (const name of Object.keys(env)) {
    if (name.startsWith("GIT_") || name === "LEFTHOOK_CONFIG") {
      delete env[name];
    }
  }
  env.GIT_CONFIG_GLOBAL = process.platform === "win32" ? "NUL" : "/dev/null";
  env.GIT_CONFIG_NOSYSTEM = "1";
  return env;
}

// The exported selectors run git without an explicit environment, so they
// inherit this process's environment; isolate it before any test runs.
isolateGitEnvironment(process.env);

function fixture(
  t,
  initialCommit = true,
  parentEnv = process.env,
  directoryName = null,
) {
  let cwd = mkdtempSync(join(tmpdir(), "rstack-format-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  if (directoryName !== null) {
    // Place the repository in a child directory with a chosen name; removing
    // the unique parent cleans it up.
    cwd = join(cwd, directoryName);
    mkdirSync(cwd);
  }
  const env = isolateGitEnvironment({ ...parentEnv, LEFTHOOK_BIN: lefthook });
  delete env.CI;
  delete env.TURBO_SCM_BASE;
  delete env.TURBO_SCM_HEAD;
  env.PATH = [
    dirname(process.execPath),
    join(root, "node_modules/.bin"),
    env.PATH,
  ].join(delimiter);
  const git = (...args) =>
    execFileSync("git", args, { cwd, env, encoding: "utf8" }).trim();
  git("init", "--quiet");
  git("config", "user.email", "fixture@example.invalid");
  git("config", "user.name", "Formatting fixture");
  git("config", "core.hooksPath", ".no-hooks");
  const write = (name, text) => {
    mkdirSync(dirname(join(cwd, name)), { recursive: true });
    writeFileSync(join(cwd, name), text);
  };
  write("package.json", '{"type":"module"}\n');
  copyFileSync(join(root, "rstack.config.ts"), join(cwd, "rstack.config.ts"));
  symlinkSync(
    join(root, "node_modules"),
    join(cwd, "node_modules"),
    process.platform === "win32" ? "junction" : "dir",
  );
  write(".gitignore", "node_modules\n");
  const commit = () => {
    git("add", ".");
    git("commit", "--quiet", "-m", "fixture");
    return git("rev-parse", "HEAD");
  };
  const installHook = () => {
    copyFileSync(join(root, "lefthook.yml"), join(cwd, "lefthook.yml"));
    write(
      "scripts/ci/format-changed.mjs",
      readFileSync(join(root, "scripts/ci/format-changed.mjs"), "utf8"),
    );
    git("config", "--unset", "core.hooksPath");
    execFileSync(lefthook, ["install"], { cwd, env, encoding: "utf8" });
  };
  return {
    cwd,
    write,
    git,
    commit,
    installHook,
    env,
    base: initialCommit ? commit() : null,
  };
}

test("fixture ignores hostile global Git configuration and GIT_* variables", (t) => {
  const hostile = mkdtempSync(join(tmpdir(), "rstack-hostile-"));
  t.after(() => rmSync(hostile, { recursive: true, force: true }));
  const globalConfig = join(hostile, "gitconfig");
  writeFileSync(
    globalConfig,
    `[core]\n\thooksPath = ${join(hostile, "hooks")}\n[commit]\n\tgpgsign = true\n`,
  );
  const bare = join(hostile, "bare.git");
  execFileSync("git", ["init", "--quiet", "--bare", bare], {
    env: isolateGitEnvironment({ PATH: process.env.PATH }),
  });
  const f = fixture(t, true, {
    ...process.env,
    GIT_CONFIG_GLOBAL: globalConfig,
    GIT_DIR: bare,
    GIT_INDEX_FILE: join(hostile, "index"),
    GIT_WORK_TREE: hostile,
  });
  f.installHook();
  f.write("hostile.ts", "const value={answer:42}\n");
  f.git("add", "hostile.ts");
  f.git("commit", "--quiet", "-m", "hostile environment");
  assert.equal(
    f.git("show", "HEAD:hostile.ts"),
    "const value = { answer: 42 };",
  );
});

test("pre-commit formats and stages initial-commit files with literal paths", (t) => {
  const f = fixture(t, false);
  f.installHook();
  const names = [
    "space name.tsx",
    "한글[1].ts",
    "-flag.ts",
    "!negation.ts",
    "apps/example/src/desktop.test.tsx",
  ];
  if (process.platform !== "win32") names.push("line\nname.ts", "star*.ts");
  for (const name of names) f.write(name, "const value={answer:42}\n");
  f.write("vendor/ignored.ts", "deliberately invalid {{{\n");
  f.write("untracked.ts", "const untracked={answer:42}\n");
  f.git("add", "--", ...names, "vendor/ignored.ts");
  assert.deepEqual(
    stagedFormatFiles(f.cwd),
    [...names, "vendor/ignored.ts"].sort(),
  );
  f.git("commit", "--quiet", "-m", "initial formatted sources");
  for (const name of names) {
    assert.equal(
      f.git("show", `HEAD:${name}`),
      "const value = { answer: 42 };",
    );
    assert.equal(
      readFileSync(join(f.cwd, name), "utf8"),
      "const value = { answer: 42 };\n",
    );
  }
  assert.equal(
    f.git("show", "HEAD:vendor/ignored.ts"),
    "deliberately invalid {{{",
  );
  assert.equal(
    readFileSync(join(f.cwd, "untracked.ts"), "utf8"),
    "const untracked={answer:42}\n",
  );
  assert.equal(f.git("diff", "--name-only"), "");
});

test("pre-commit preserves unstaged edits in a partially staged file", (t) => {
  const f = fixture(t);
  const padding = Array.from(
    { length: 15 },
    (_, index) => `// Separator ${index}\n`,
  ).join("");
  const original = `const value = { answer: 1 };\n${padding}const tail = 1;\n`;
  f.write("partial.ts", original);
  f.commit();
  f.installHook();
  const staged = `const value={answer:42}\n${padding}const tail = 1;\n`;
  f.write("partial.ts", staged);
  f.git("add", "partial.ts");
  f.write("partial.ts", staged.replace("const tail = 1;", "const tail = 2;"));
  f.git("commit", "--quiet", "-m", "format only staged content");
  const formatted = `const value = { answer: 42 };\n${padding}const tail = 1;\n`;
  assert.equal(f.git("show", "HEAD:partial.ts"), formatted.trim());
  assert.equal(
    readFileSync(join(f.cwd, "partial.ts"), "utf8"),
    formatted.replace("const tail = 1;", "const tail = 2;"),
  );
  assert.equal(f.git("diff", "--cached", "--name-only"), "");
  assert.equal(f.git("diff", "--name-only"), "partial.ts");
});

test("pre-commit rejects a conflicting unstaged edit and preserves both versions", (t) => {
  const f = fixture(t);
  f.write("conflict.ts", "const value = { answer: 1 };\n");
  const head = f.commit();
  f.installHook();
  const staged = "const value={answer:42}\n";
  const unstaged = "const value={answer:43}\n";
  f.write("conflict.ts", staged);
  f.git("add", "conflict.ts");
  f.write("conflict.ts", unstaged);
  const result = spawnSync(
    "git",
    ["commit", "--quiet", "-m", "format conflicting content"],
    { cwd: f.cwd, env: f.env, encoding: "utf8" },
  );
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.equal(f.git("rev-parse", "HEAD"), head);
  assert.equal(readFileSync(join(f.cwd, "conflict.ts"), "utf8"), unstaged);
  assert.equal(f.git("show", ":conflict.ts"), staged.trim());
  assert.equal(f.git("stash", "list"), "");
  const info = join(f.cwd, f.git("rev-parse", "--git-path", "info"));
  assert.deepEqual(
    readdirSync(info).filter((name) => name.endsWith(".patch")),
    [],
  );
});

test("pre-commit formats initial commits when CI is explicitly false", (t) => {
  const f = fixture(t, false);
  f.env.CI = "false";
  f.installHook();
  f.write("initial.ts", "const value={answer:42}\n");
  f.git("add", "initial.ts");
  f.git("commit", "--quiet", "-m", "initial formatted source");
  assert.equal(
    f.git("show", "HEAD:initial.ts"),
    "const value = { answer: 42 };",
  );
  assert.equal(
    readFileSync(join(f.cwd, "initial.ts"), "utf8"),
    "const value = { answer: 42 };\n",
  );
  assert.equal(f.git("diff", "--name-only"), "");
});

test("pre-commit preserves unstaged edits when CI is explicitly false", (t) => {
  const f = fixture(t);
  f.env.CI = "False";
  const padding = Array.from(
    { length: 15 },
    (_, index) => `// Separator ${index}\n`,
  ).join("");
  f.write(
    "partial.ts",
    `const value = { answer: 1 };\n${padding}const tail = 1;\n`,
  );
  f.commit();
  f.installHook();
  const staged = `const value={answer:42}\n${padding}const tail = 1;\n`;
  f.write("partial.ts", staged);
  f.git("add", "partial.ts");
  f.write("partial.ts", staged.replace("const tail = 1;", "const tail = 2;"));
  f.git("commit", "--quiet", "-m", "format only staged content");
  const formatted = `const value = { answer: 42 };\n${padding}const tail = 1;\n`;
  assert.equal(f.git("show", "HEAD:partial.ts"), formatted.trim());
  assert.equal(
    readFileSync(join(f.cwd, "partial.ts"), "utf8"),
    formatted.replace("const tail = 1;", "const tail = 2;"),
  );
  assert.equal(f.git("diff", "--name-only"), "partial.ts");
});

test("pre-commit skips deletion-only and unsupported-file commits", (t) => {
  const f = fixture(t);
  f.write("removed.ts", "const value = 1;\n");
  f.commit();
  f.installHook();
  f.git("rm", "removed.ts");
  assert.deepEqual(stagedFormatFiles(f.cwd), []);
  f.git("commit", "--quiet", "-m", "delete source");
  f.write("notes.txt", "plain text\n");
  f.git("add", "notes.txt");
  assert.deepEqual(stagedFormatFiles(f.cwd), []);
  f.git("commit", "--quiet", "-m", "unsupported source");
  assert.equal(f.git("show", "HEAD:notes.txt"), "plain text");
});

test("pre-commit rejects parser errors without creating a commit", (t) => {
  const f = fixture(t);
  f.installHook();
  f.write("broken.ts", "const broken = ;\n");
  f.git("add", "broken.ts");
  const result = spawnSync(
    "git",
    ["commit", "--quiet", "-m", "invalid source"],
    {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    },
  );
  assert.notEqual(result.status, 0);
  assert.equal(f.git("rev-parse", "HEAD"), f.base);
  assert.match(result.stdout + result.stderr, /ci_format_result/);
  assert.equal(f.git("show", ":broken.ts"), "const broken = ;");
});

test(
  "staged selection skips symlinks and fails for a staged file removed from the worktree",
  { skip: process.platform === "win32" },
  (t) => {
    const f = fixture(t);
    f.write("target.ts", "const value = 1;\n");
    symlinkSync("target.ts", join(f.cwd, "linked.ts"));
    f.git("add", "target.ts", "linked.ts");
    assert.deepEqual(stagedFormatFiles(f.cwd), ["target.ts"]);
    rmSync(join(f.cwd, "target.ts"));
    assert.throws(() => stagedFormatFiles(f.cwd), /ENOENT.*target\.ts/u);
  },
);

test(
  "staged symlinks stay exempt when their worktree entry is missing, and a staged type change is judged by its new mode",
  { skip: process.platform === "win32" },
  (t) => {
    const f = fixture(t);
    f.write("target.ts", "const value = 1;\n");
    f.write("retyped.ts", "const value = 2;\n");
    f.commit();
    symlinkSync("target.ts", join(f.cwd, "linked.ts"));
    // Replace a tracked regular file with a symlink: a staged type change.
    rmSync(join(f.cwd, "retyped.ts"));
    symlinkSync("target.ts", join(f.cwd, "retyped.ts"));
    f.git("add", "linked.ts", "retyped.ts");
    assert.deepEqual(stagedFormatFiles(f.cwd), []);
    // Remove the placeholders, as a sparse checkout or `rm` after `git add`
    // would: a staged symlink is never formatted, so it needs no worktree file.
    rmSync(join(f.cwd, "linked.ts"));
    rmSync(join(f.cwd, "retyped.ts"));
    assert.deepEqual(stagedFormatFiles(f.cwd), []);
    // A missing regular file is still reported next to the exempt symlinks.
    f.write("added.ts", "const   value = 3;\n");
    f.git("add", "added.ts");
    rmSync(join(f.cwd, "added.ts"));
    assert.throws(() => stagedFormatFiles(f.cwd), /added\.ts/u);
    // A staged type change from a symlink back to a regular file is formatted.
    f.git("rm", "--cached", "--quiet", "--", "added.ts");
    f.git("commit", "--quiet", "-m", "symlinks");
    f.write("retyped.ts", "const   value = 4;\n");
    f.git("add", "retyped.ts");
    assert.deepEqual(stagedFormatFiles(f.cwd), ["retyped.ts"]);
  },
);

test("staged selection fails naming each kind of staged path missing from the worktree", (t) => {
  const f = fixture(t);
  f.write("modified.ts", "const value = 1;\n");
  f.write("renamed.ts", "const value = 2;\n");
  f.commit();
  f.write("modified.ts", "const value = 3;\n");
  f.write("added.ts", "const   value = 4;\n");
  f.git("mv", "renamed.ts", "moved.ts");
  f.git("add", "modified.ts", "added.ts");
  assert.deepEqual(stagedFormatFiles(f.cwd), [
    "added.ts",
    "modified.ts",
    "moved.ts",
  ]);
  for (const name of ["added.ts", "modified.ts", "moved.ts"]) {
    rmSync(join(f.cwd, name));
    assert.throws(
      () => stagedFormatFiles(f.cwd),
      (error) => {
        assert.match(error.message, /missing from the working tree/u);
        assert.ok(error.message.includes(name), error.message);
        return true;
      },
    );
    // Restoring the file lets the selection succeed again.
    f.git("checkout-index", "--force", "--", name);
  }
  assert.equal(stagedFormatFiles(f.cwd).length, 3);
});

test("staged deletions and unsupported missing paths do not fail the selection", (t) => {
  const f = fixture(t);
  f.write("removed.ts", "const value = 1;\n");
  f.write("notes.txt", "plain text\n");
  f.write("kept.ts", "const value = 2;\n");
  f.commit();
  f.git("rm", "removed.ts");
  f.write("notes.txt", "changed text\n");
  f.write("new-notes.txt", "new text\n");
  f.git("add", "notes.txt", "new-notes.txt");
  rmSync(join(f.cwd, "new-notes.txt"));
  assert.deepEqual(stagedFormatFiles(f.cwd), []);
  f.write("kept.ts", "const   value = 3;\n");
  f.git("add", "kept.ts");
  assert.deepEqual(stagedFormatFiles(f.cwd), ["kept.ts"]);
});

// Same list as `extensions` in format-changed.mjs; the exclusion probe must work
// for each of them, so a missing extension here would hide a broken probe.
const formatExtensions = [
  ".js",
  ".jsx",
  ".mjs",
  ".cjs",
  ".ts",
  ".tsx",
  ".mts",
  ".cts",
  ".css",
  ".json",
  ".jsonc",
  ".json5",
  ".yaml",
  ".yml",
  ".md",
  ".mdx",
];

// Staged paths that the shared rstack.config.ts exclusions skip even when they
// are present. Their missing worktree file must not fail the selection.
const excludedStagedPaths = [
  "pnpm-lock.yaml",
  "apps/web/pnpm-lock.yaml",
  "package-lock.json",
  "vendor/lib.ts",
  "packages/a/third-party/lib.js",
  "src/schema.generated.ts",
  "protos/gen/api.json",
  ".agents/skills/example/SKILL.md",
];

test("staged selection skips paths excluded by the shared configuration when they are missing from the worktree", (t) => {
  const f = fixture(t);
  for (const name of excludedStagedPaths) f.write(name, "a:   1\n");
  f.write("kept.ts", "const value = 1;\n");
  f.commit();
  for (const name of excludedStagedPaths) f.write(name, "a:    2\n");
  f.write("kept.ts", "const   value = 2;\n");
  f.git("add", ".");
  // Present excluded paths are selected and left to Rstack, as before.
  assert.deepEqual(
    stagedFormatFiles(f.cwd),
    [...excludedStagedPaths, "kept.ts"].sort(),
  );
  for (const name of excludedStagedPaths) rmSync(join(f.cwd, name));
  assert.deepEqual(stagedFormatFiles(f.cwd), ["kept.ts"]);
  // An excluded missing path never hides a missing path Rstack would format.
  rmSync(join(f.cwd, "kept.ts"));
  assert.throws(
    () => stagedFormatFiles(f.cwd),
    /missing from the working tree.*kept\.ts/u,
  );
  // A path excluded by its name is still excluded behind a replaced parent.
  f.git("checkout-index", "--force", "--", "kept.ts");
  rmSync(join(f.cwd, "vendor"), { recursive: true, force: true });
  f.write("vendor", "not a directory\n");
  assert.deepEqual(stagedFormatFiles(f.cwd), ["kept.ts"]);
});

test("the exclusion probe covers every supported extension", (t) => {
  const f = fixture(t);
  for (const extension of formatExtensions) {
    f.write(`vendor/probe${extension}`, "x\n");
    f.write(`probe${extension}`, "x\n");
  }
  f.git("add", ".");
  for (const extension of formatExtensions) {
    // The excluded path alone is missing: nothing fails, the present path stays.
    rmSync(join(f.cwd, `vendor/probe${extension}`));
    assert.ok(
      stagedFormatFiles(f.cwd).includes(`probe${extension}`),
      extension,
    );
    rmSync(join(f.cwd, `probe${extension}`));
    assert.throws(
      () => stagedFormatFiles(f.cwd),
      (error) => {
        // Only the formattable path fails; the excluded sibling is skipped.
        assert.ok(
          error.message.includes(`cannot be formatted: probe${extension}.`),
          error.message,
        );
        assert.ok(!error.message.includes("vendor/"), error.message);
        return true;
      },
      extension,
    );
    f.git("checkout-index", "--force", "--", `probe${extension}`);
  }
});

test(
  "staged selection applies every exemption before the missing-file failure, like the unstaged selectors",
  { skip: process.platform === "win32" },
  (t) => {
    const f = fixture(t);
    f.write("target.ts", "const value = 1;\n");
    f.write("renamed.ts", "const value = 2;\n");
    f.write("removed.ts", "const value = 3;\n");
    f.write("pnpm-lock.yaml", "a:   1\n");
    const head = f.commit();
    symlinkSync("target.ts", join(f.cwd, "linked.ts"));
    f.write("target.ts", "const   value = 5;\n");
    f.write("notes.txt", "plain\n");
    f.write("vendor/lib.ts", "const   value = 4;\n");
    f.write("pnpm-lock.yaml", "a:    2\n");
    f.git("mv", "renamed.ts", "vendor/renamed.ts");
    f.git("rm", "--quiet", "removed.ts");
    f.git("add", ".");
    for (const name of [
      "linked.ts",
      "notes.txt",
      "vendor/lib.ts",
      "vendor/renamed.ts",
      "pnpm-lock.yaml",
    ]) {
      rmSync(join(f.cwd, name), { force: true });
    }
    // Symlink, unsupported extension, shared exclusions, rename into an excluded
    // directory and a staged deletion are all exempt although nothing is on disk.
    assert.deepEqual(stagedFormatFiles(f.cwd), ["target.ts"]);
    // The unstaged selectors treat the same missing paths identically.
    assert.ok(!allFormatFiles(f.cwd).includes("vendor/lib.ts"));
    assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), [
      "target.ts",
    ]);
    // Only a missing path that Rstack would format fails.
    rmSync(join(f.cwd, "target.ts"));
    assert.throws(
      () => stagedFormatFiles(f.cwd),
      /missing from the working tree.*target\.ts/u,
    );
    assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), []);
  },
);

test("CLI --staged accepts a staged excluded file missing from the worktree", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  f.write("pnpm-lock.yaml", "a:   1\n");
  f.git("add", "pnpm-lock.yaml");
  rmSync(join(f.cwd, "pnpm-lock.yaml"));
  for (const args of [["--staged"], ["--staged", "--write"]]) {
    const result = spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
    const output = `${args.join(" ")}: ${result.stdout}${result.stderr}`;
    assert.equal(result.status, 0, output);
    assert.doesNotMatch(result.stderr, /ci_format_error/u, output);
    assert.match(result.stdout, /"candidates":0/u, output);
  }
});

test("CLI --staged exits 2 naming a staged file removed from the worktree", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  f.write("b.ts", "const   value = 1;\n");
  f.git("add", "b.ts");
  rmSync(join(f.cwd, "b.ts"));
  for (const args of [["--staged"], ["--staged", "--write"]]) {
    const result = spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
    const output = `${args.join(" ")}: ${result.stdout}${result.stderr}`;
    assert.equal(result.status, 2, output);
    assert.match(result.stderr, /ci_format_error/u, output);
    assert.match(result.stderr, /b\.ts/u, output);
  }
});

// Lefthook hides unstaged changes (including a plain `rm` after `git add`) by
// restoring the staged content before the hook runs, so the hook only sees a
// staged path missing from the worktree when Git itself omits it from the
// unstaged diff. A skip-worktree entry, as in a sparse checkout, does that.
test("pre-commit rejects a staged skip-worktree file missing from the worktree and keeps deletions working", (t) => {
  const f = fixture(t);
  f.write("tracked.ts", "const value = 1;\n");
  f.write("sparse.ts", "const value = 1;\n");
  const head = f.commit();
  f.installHook();
  const commit = () =>
    spawnSync("git", ["commit", "--quiet", "-m", "x"], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
  f.write("sparse.ts", "const   value = 2;\n");
  f.git("add", "sparse.ts");
  f.git("update-index", "--skip-worktree", "sparse.ts");
  rmSync(join(f.cwd, "sparse.ts"));
  const rejected = commit();
  const output = rejected.stdout + rejected.stderr;
  assert.notEqual(rejected.status, 0, output);
  assert.equal(f.git("rev-parse", "HEAD"), head);
  assert.match(output, /ci_format_error/u);
  assert.match(output, /sparse\.ts/u);
  assert.equal(f.git("show", ":sparse.ts"), "const   value = 2;");
  // Restoring the staged state of the path and deleting another file works.
  f.git("update-index", "--no-skip-worktree", "sparse.ts");
  f.git("reset", "--quiet", "--", "sparse.ts");
  f.git("checkout", "--quiet", "--", "sparse.ts");
  f.git("rm", "--quiet", "tracked.ts");
  const accepted = commit();
  assert.equal(accepted.status, 0, accepted.stdout + accepted.stderr);
  assert.notEqual(f.git("rev-parse", "HEAD"), head);
});

test("pre-commit accepts a staged skip-worktree lockfile missing from the worktree", (t) => {
  const f = fixture(t);
  f.write("pnpm-lock.yaml", "lockfileVersion: '9.0'\n");
  f.write("tracked.ts", "const value = 1;\n");
  const head = f.commit();
  f.installHook();
  f.write("pnpm-lock.yaml", "lockfileVersion:   '9.1'\n");
  f.git("add", "pnpm-lock.yaml");
  f.git("update-index", "--skip-worktree", "pnpm-lock.yaml");
  rmSync(join(f.cwd, "pnpm-lock.yaml"));
  const accepted = spawnSync("git", ["commit", "--quiet", "-m", "lockfile"], {
    cwd: f.cwd,
    env: f.env,
    encoding: "utf8",
  });
  const output = accepted.stdout + accepted.stderr;
  assert.equal(accepted.status, 0, output);
  assert.doesNotMatch(output, /ci_format_error/u);
  assert.notEqual(f.git("rev-parse", "HEAD"), head);
  // Rstack never rewrites an excluded lockfile.
  assert.equal(
    f.git("show", "HEAD:pnpm-lock.yaml"),
    "lockfileVersion:   '9.1'",
  );
});

test("real Rstack checks whole changed files without rewriting, then fixes them", async (t) => {
  const f = fixture(t);
  const names = ["space name.ts", "한글[1].ts", "-flag.ts", "!negation.ts"];
  if (process.platform !== "win32") names.push("line\nname.ts", "star*.ts");
  for (const name of names) f.write(name, "const value={answer:42}\n");
  f.write("unrelated.go", "not Go\n");
  const head = f.commit();
  const paths = changedFormatFiles(f.cwd, f.base, head);
  assert.deepEqual(paths, names.sort());
  const before = paths.map((name) => readFileSync(join(f.cwd, name), "utf8"));
  assert.equal(await runFormatter(f.cwd, paths), 1);
  assert.deepEqual(
    paths.map((name) => readFileSync(join(f.cwd, name), "utf8")),
    before,
  );
  assert.equal(await runFormatter(f.cwd, paths, "write"), 0);
  assert.equal(await runFormatter(f.cwd, paths), 0);
  // Validation checks merge/worktree bytes, not the selected head blob.
  f.write(names[0], "const broken = ;\n");
  assert.equal(await runFormatter(f.cwd, paths), 2);
});

test("deletions, renames, empty ranges, local edits and invalid comparisons", async (t) => {
  const f = fixture(t);
  f.write("old.ts", "const old = 1;\n");
  f.write("deleted.ts", "const deleted = 1;\n");
  const base = f.commit();
  renameSync(join(f.cwd, "old.ts"), join(f.cwd, "new.ts"));
  rmSync(join(f.cwd, "deleted.ts"));
  const head = f.commit();
  assert.deepEqual(changedFormatFiles(f.cwd, base, head), ["new.ts"]);
  assert.deepEqual(changedFormatFiles(f.cwd, head, head), []);
  assert.equal(await runFormatter(f.cwd, []), 0);
  f.write("local.md", "# Local\n");
  f.write("new.ts", "const changed=2\n");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), [
    "local.md",
    "new.ts",
  ]);
  assert.throws(() => changedFormatFiles(f.cwd, "", head), /comparison SHA/);
  assert.throws(() => changedFormatFiles(f.cwd, "a".repeat(40), head));
});

// A path staged with new content whose working tree was then restored to the
// HEAD bytes ("MM" in `git status`) differs from HEAD only in the index.
function stageThenRevert(f, name, committed, staged) {
  f.write(name, staged);
  f.git("add", name);
  f.write(name, committed);
}

test("default local selection includes a path that differs from HEAD only in the index", (t) => {
  const f = fixture(t);
  f.write("f.ts", "const value = 1;\n");
  f.write("kept.ts", "const kept = 1;\n");
  f.write("gone.ts", "const gone = 1;\n");
  const head = f.commit();
  stageThenRevert(f, "f.ts", "const value = 1;\n", "const   value = 2;\n");
  assert.equal(f.git("status", "--porcelain", "--", "f.ts"), "MM f.ts");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), ["f.ts"]);
  // Staged deletions, unsupported paths and unchanged files stay unselected.
  f.git("rm", "--quiet", "gone.ts");
  f.write("notes.txt", "plain\n");
  f.git("add", "notes.txt");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), ["f.ts"]);
});

test("default local selection still includes worktree edits and untracked paths", (t) => {
  const f = fixture(t);
  f.write("staged.ts", "const staged = 1;\n");
  f.write("edited.ts", "const edited = 1;\n");
  const head = f.commit();
  stageThenRevert(
    f,
    "staged.ts",
    "const staged = 1;\n",
    "const   staged = 2;\n",
  );
  f.write("edited.ts", "const edited = 2;\n");
  f.write("untracked.md", "# Untracked\n");
  f.write("added.ts", "const added = 1;\n");
  f.git("add", "added.ts");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), [
    "added.ts",
    "edited.ts",
    "staged.ts",
    "untracked.md",
  ]);
});

test("explicit comparisons exclude paths that differ from HEAD only in the index", (t) => {
  const f = fixture(t);
  f.write("f.ts", "const value = 1;\n");
  const head = f.commit();
  stageThenRevert(f, "f.ts", "const value = 1;\n", "const   value = 2;\n");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head), []);
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, false), []);
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = (args, env) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env,
      encoding: "utf8",
    });
  for (const [args, env] of [
    [["--base", head, "--head", head], f.env],
    [[], { ...f.env, CI: "true", TURBO_SCM_BASE: head, TURBO_SCM_HEAD: head }],
  ]) {
    const result = run(args, env);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /"candidates":0/u, result.stdout);
    assert.match(result.stdout, /"local":false/u, result.stdout);
  }
});

test("--staged selection keeps reporting an index-only path", (t) => {
  const f = fixture(t);
  f.write("f.ts", "const value = 1;\n");
  f.commit();
  stageThenRevert(f, "f.ts", "const value = 1;\n", "const   value = 2;\n");
  assert.deepEqual(stagedFormatFiles(f.cwd), ["f.ts"]);
});

test("CLI default local run selects an index-only path and format:check fails on unformatted bytes", (t) => {
  const f = fixture(t);
  f.write("f.ts", "const value = 1;\n");
  const head = f.commit();
  // The default local comparison needs an origin/main ref to find the merge base.
  f.git("update-ref", "refs/remotes/origin/main", head);
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = () =>
    spawnSync(process.execPath, [script], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
  stageThenRevert(f, "f.ts", "const value = 1;\n", "const   value = 2;\n");
  // The formatter checks working-tree bytes, not the staged blob, so formatted
  // working-tree bytes pass even though the path is now selected.
  const formatted = run();
  assert.equal(formatted.status, 0, formatted.stdout + formatted.stderr);
  assert.match(formatted.stdout, /"candidates":1/u, formatted.stdout);
  assert.match(formatted.stdout, /"local":true/u, formatted.stdout);
  // Unformatted working-tree bytes in the same MM-selected path fail the check.
  f.write("f.ts", "const   value = 3;\n");
  const unformatted = run();
  assert.equal(unformatted.status, 1, unformatted.stdout + unformatted.stderr);
  assert.match(unformatted.stdout, /"candidates":1/u, unformatted.stdout);
});

// A checkout without symlink support (Windows without privileges, or
// `core.symlinks=false`) materializes a symlink the index records with mode
// 120000 as a regular file holding the link target. Build exactly that state
// without a native symlink, so the fixtures also run where symlinks need
// privileges. The placeholder is a parse error as TypeScript, so a selector that
// let it reach Rstack would fail the check and the write flow.
const SymlinkPlaceholder = "../target.ts";

function recordSymlink(f, name, placeholder = SymlinkPlaceholder) {
  f.git("config", "core.symlinks", "false");
  const blob = execFileSync("git", ["hash-object", "-w", "--stdin"], {
    cwd: f.cwd,
    env: f.env,
    encoding: "utf8",
    input: placeholder,
  }).trim();
  f.git("update-index", "--add", "--cacheinfo", `120000,${blob},${name}`);
  f.write(name, placeholder);
}

const indexMode = (f, name) =>
  f.git("ls-files", "--stage", "--", name).split(" ")[0];

test("every selector skips an index-recorded symlink checked out as a regular file", (t) => {
  const f = fixture(t);
  f.write("regular.ts", "const regular = 1;\n");
  recordSymlink(f, "linked.ts");
  recordSymlink(f, "dir/linked.md", "../README.md");
  recordSymlink(f, "tab\tname.json", "../x.json");
  const head = f.commit();
  assert.equal(indexMode(f, "linked.ts"), "120000");
  assert.equal(indexMode(f, "regular.ts"), "100644");
  // Local edits and an explicit range that both cover the placeholders.
  f.write("regular.ts", "const regular = 2;\n");
  f.write("untracked.ts", "const untracked = 1;\n");
  const all = allFormatFiles(f.cwd);
  assert.ok(all.includes("regular.ts"), all.join(","));
  assert.ok(
    !all.some((name) => /linked|tab/u.test(name)),
    `all selection kept a symlink: ${all.join(",")}`,
  );
  assert.deepEqual(changedFormatFiles(f.cwd, f.base, head), ["regular.ts"]);
  assert.deepEqual(changedFormatFiles(f.cwd, f.base, head, true), [
    "regular.ts",
    "untracked.ts",
  ]);
  // Git keeps the recorded symlink type when `core.symlinks=false` re-adds the
  // placeholder, so the selection stays correct after `git add`.
  f.write("linked.ts", "../other.ts");
  f.git("add", "linked.ts");
  assert.equal(indexMode(f, "linked.ts"), "120000");
  assert.ok(!allFormatFiles(f.cwd).includes("linked.ts"));
});

test("--staged selection skips a recorded symlink, including before the first commit", (t) => {
  const f = fixture(t, false);
  f.write("regular.ts", "const   regular = 1;\n");
  recordSymlink(f, "linked.ts");
  recordSymlink(f, "gone.ts");
  rmSync(join(f.cwd, "gone.ts"));
  f.git("add", "regular.ts");
  // HEAD is unborn, and the missing placeholder of a recorded symlink does not
  // fail the selection that rejects other missing staged paths.
  assert.deepEqual(stagedFormatFiles(f.cwd), ["regular.ts"]);
});

test("--staged selection skips a path whose type changed to a recorded symlink", (t) => {
  const f = fixture(t);
  f.write("changed.ts", "const changed = 1;\n");
  f.write("kept.ts", "const kept = 1;\n");
  f.commit();
  recordSymlink(f, "changed.ts");
  f.write("kept.ts", "const   kept = 2;\n");
  f.git("add", "kept.ts");
  assert.match(
    f.git("diff", "--cached", "--raw", "--", "changed.ts"),
    /\sT\tchanged\.ts$/u,
  );
  assert.deepEqual(stagedFormatFiles(f.cwd), ["kept.ts"]);
});

test("explicit comparisons use the validation index, not the comparison head", (t) => {
  const f = fixture(t);
  f.write("was-link.ts", "const was = 1;\n");
  f.write("now-link.ts", "const now = 1;\n");
  f.write("other.ts", "const other = 1;\n");
  const base = f.commit();
  // The comparison head records `was-link.ts` as a symlink and `now-link.ts`
  // as a regular file; the checked-out validation tree records the reverse.
  recordSymlink(f, "was-link.ts");
  f.write("now-link.ts", "const now = 2;\n");
  f.write("other.ts", "const other = 2;\n");
  f.git("add", "-A", ".");
  f.git("commit", "--quiet", "-m", "head");
  const head = f.git("rev-parse", "HEAD");
  assert.deepEqual(changedFormatFiles(f.cwd, base, head), [
    "now-link.ts",
    "other.ts",
  ]);
  // Validation tree: swap the recorded types in the index only.
  f.git("rm", "--cached", "--quiet", "was-link.ts");
  f.write("was-link.ts", "const was = 3;\n");
  f.git("add", "was-link.ts");
  recordSymlink(f, "now-link.ts");
  assert.equal(indexMode(f, "was-link.ts"), "100644");
  assert.equal(indexMode(f, "now-link.ts"), "120000");
  assert.deepEqual(changedFormatFiles(f.cwd, base, head), [
    "other.ts",
    "was-link.ts",
  ]);
});

test("local selection keeps untracked regular files and skips native symlinks", (t) => {
  const f = fixture(t);
  f.write("tracked.ts", "const tracked = 1;\n");
  const head = f.commit();
  recordSymlink(f, "linked.ts");
  f.write("untracked.ts", "const untracked = 1;\n");
  assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), [
    "untracked.ts",
  ]);
  if (process.platform !== "win32") {
    symlinkSync("tracked.ts", join(f.cwd, "native.ts"));
    assert.deepEqual(changedFormatFiles(f.cwd, head, head, true), [
      "untracked.ts",
    ]);
    f.git("add", "native.ts");
    assert.equal(indexMode(f, "native.ts"), "120000");
    assert.deepEqual(stagedFormatFiles(f.cwd), []);
  }
});

test("CLI selectors never hand a recorded symlink to Rstack and keep its bytes and mode", async (t) => {
  const f = fixture(t);
  f.write("regular.ts", "const regular = 1;\n");
  const head = f.commit();
  f.git("update-ref", "refs/remotes/origin/main", head);
  recordSymlink(f, "linked.ts");
  f.write("regular.ts", "const   regular = 2;\n");
  f.git("add", "regular.ts");
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = (args, env = f.env) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env,
      encoding: "utf8",
    });
  const stageBefore = f.git("ls-files", "--stage");
  // The placeholder would be a parse error (status 2) if Rstack received it.
  const candidates = {
    "": 1,
    "--all": allFormatFiles(f.cwd).length,
    "--staged": 1,
  };
  assert.ok(!allFormatFiles(f.cwd).includes("linked.ts"));
  for (const args of [[], ["--all"], ["--staged"]]) {
    const result = run(args);
    const output = `${args.join(" ")}: ${result.stdout}${result.stderr}`;
    assert.match(
      result.stdout,
      new RegExp(`"candidates":${candidates[args.join(" ")]}[,}]`, "u"),
      output,
    );
    assert.equal(result.status, 1, output);
  }
  for (const args of [[], ["--all"], ["--staged"]]) {
    const result = run([...args, "--write"]);
    assert.equal(
      result.status,
      0,
      `${args.join(" ")}: ${result.stdout}${result.stderr}`,
    );
  }
  assert.equal(
    readFileSync(join(f.cwd, "linked.ts"), "utf8"),
    SymlinkPlaceholder,
  );
  assert.equal(
    readFileSync(join(f.cwd, "regular.ts"), "utf8"),
    "const regular = 2;\n",
  );
  assert.equal(indexMode(f, "linked.ts"), "120000");
  assert.equal(
    f.git("ls-files", "--stage", "--", "linked.ts"),
    stageBefore.split("\n").find((line) => line.endsWith("\tlinked.ts")),
  );
});

test("pre-commit leaves a recorded symlink placeholder and its index mode untouched", (t) => {
  const f = fixture(t, false);
  f.installHook();
  f.write("regular.ts", "const   regular = {answer:42}\n");
  recordSymlink(f, "linked.ts");
  f.git("add", "regular.ts");
  const linkedEntry = f.git("ls-files", "--stage", "--", "linked.ts");
  const result = spawnSync("git", ["commit", "--quiet", "-m", "initial"], {
    cwd: f.cwd,
    env: f.env,
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout + result.stderr, /"candidates":1/u);
  assert.equal(
    f.git("show", "HEAD:regular.ts"),
    "const regular = { answer: 42 };",
  );
  assert.equal(
    f.git("ls-tree", "HEAD", "--", "linked.ts").split(" ")[0],
    "120000",
  );
  assert.equal(f.git("ls-files", "--stage", "--", "linked.ts"), linkedEntry);
  assert.equal(
    readFileSync(join(f.cwd, "linked.ts"), "utf8"),
    SymlinkPlaceholder,
  );
});

test("all-file selection skips unstaged deletions and retains existing files", async (t) => {
  const f = fixture(t);
  f.write("deleted.ts", "const deleted = 1;\n");
  f.write("retained.ts", "const retained = 1;\n");
  f.commit();
  rmSync(join(f.cwd, "deleted.ts"));
  const paths = allFormatFiles(f.cwd);
  assert.ok(!paths.includes("deleted.ts"));
  assert.ok(paths.includes("retained.ts"));
  assert.equal(await runFormatter(f.cwd, paths, "write"), 0);
  assert.equal(await runFormatter(f.cwd, paths), 0);
  assert.ok(f.git("status", "--porcelain").includes("D deleted.ts"));
});

// Leaves `conflict.ts` unmerged (index stages 1, 2 and 3) by merging two
// branches that change the same line. The merge exits non-zero by design.
function conflictedMerge(f) {
  f.write("conflict.ts", "const value = 0;\n");
  f.commit();
  const main = f.git("branch", "--show-current");
  f.git("switch", "--quiet", "-c", "side");
  f.write("conflict.ts", "const value = 1;\n");
  f.git("commit", "--quiet", "-am", "side");
  f.git("switch", "--quiet", main);
  f.write("conflict.ts", "const value = 2;\n");
  f.git("commit", "--quiet", "-am", "main");
  assert.throws(() => f.git("merge", "--quiet", "side"));
  assert.equal(
    f.git("ls-files", "--unmerged", "--", "conflict.ts").split("\n").length,
    3,
  );
}

// Uppercase and mixed-case spellings of supported extensions, next to the
// lowercase spelling of the same stem, to prove the spellings behave alike.
const caseVariants = [
  "notes.MD",
  "Component.TSX",
  "data.JSON",
  "mixed.Yml",
  "Lower.MD",
  "lower.md",
];

test("every selector selects supported extensions in any letter case", (t) => {
  const f = fixture(t);
  const head = f.base;
  for (const name of caseVariants) f.write(name, "# Case\n");
  f.write("UPPER.TXT", "still unsupported\n");
  f.git("add", "--", ...caseVariants, "UPPER.TXT");
  assert.deepEqual(stagedFormatFiles(f.cwd), [...caseVariants].sort());
  assert.deepEqual(
    changedFormatFiles(f.cwd, head, head, true),
    [...caseVariants].sort(),
  );
  const committed = f.commit();
  assert.deepEqual(
    changedFormatFiles(f.cwd, head, committed),
    [...caseVariants].sort(),
  );
  assert.deepEqual(
    changedFormatFiles(f.cwd, head, committed, true),
    [...caseVariants].sort(),
  );
  assert.deepEqual(
    allFormatFiles(f.cwd),
    [...caseVariants, "package.json", "rstack.config.ts"].sort(),
  );
});

test("case-insensitive selection still skips deleted paths and unsupported names", (t) => {
  const f = fixture(t);
  f.write("gone.MD", "# Gone\n");
  const base = f.commit();
  f.git("rm", "--quiet", "gone.MD");
  f.write("kept.MD", "# Kept\n");
  f.write("UPPER.TXT", "unsupported\n");
  f.git("add", "--", "kept.MD", "UPPER.TXT");
  assert.deepEqual(stagedFormatFiles(f.cwd), ["kept.MD"]);
  assert.deepEqual(changedFormatFiles(f.cwd, base, base, true), ["kept.MD"]);
});

test("CLI selectors format an uppercase-extension file and leave excluded paths alone", async (t) => {
  const f = fixture(t);
  f.write("Notes.MD", "# Title\n\n*  item\n");
  f.write("vendor/Ignored.TS", "deliberately invalid {{{\n");
  const base = f.base;
  const head = f.commit();
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = (args) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
  const explicit = run(["--base", base, "--head", head]);
  assert.equal(explicit.status, 1, explicit.stdout + explicit.stderr);
  assert.match(explicit.stdout, /"candidates":2,/u, explicit.stdout);
  assert.match(explicit.stderr + explicit.stdout, /Notes\.MD/u);
  const all = run(["--all"]);
  assert.equal(all.status, 1, all.stdout + all.stderr);
  assert.match(all.stderr + all.stdout, /Notes\.MD/u);
  const written = run(["--base", base, "--head", head, "--write"]);
  assert.equal(written.status, 0, written.stdout + written.stderr);
  assert.equal(
    readFileSync(join(f.cwd, "Notes.MD"), "utf8"),
    "# Title\n\n- item\n",
  );
  assert.equal(
    readFileSync(join(f.cwd, "vendor/Ignored.TS"), "utf8"),
    "deliberately invalid {{{\n",
  );
});

test("pre-commit hook formats and stages a staged uppercase-extension file", (t) => {
  const f = fixture(t);
  f.installHook();
  f.write("Notes.MD", "# Title\n\n*  item\n");
  f.write("Component.TSX", "const   value={answer:42}\n");
  f.git("add", "--", "Notes.MD", "Component.TSX");
  f.git("commit", "--quiet", "-m", "uppercase extensions");
  assert.equal(f.git("show", "HEAD:Notes.MD"), "# Title\n\n- item");
  assert.equal(
    f.git("show", "HEAD:Component.TSX"),
    "const value = { answer: 42 };",
  );
  assert.equal(f.git("diff", "--name-only"), "");
});

test("all-file selection returns an unmerged path once, in sorted order", (t) => {
  const f = fixture(t);
  f.write("zeta.ts", "const zeta = 1;\n");
  f.write("alpha.ts", "const alpha = 1;\n");
  f.write("notes.txt", "not formattable\n");
  conflictedMerge(f);
  const paths = allFormatFiles(f.cwd);
  assert.deepEqual(paths, [
    "alpha.ts",
    "conflict.ts",
    "package.json",
    "rstack.config.ts",
    "zeta.ts",
  ]);
  assert.deepEqual(paths, [...paths].sort());
  assert.equal(new Set(paths).size, paths.length);
});

test("all-file selection of a conflict-free index is unique and sorted", (t) => {
  const f = fixture(t);
  f.write("zeta.ts", "const zeta = 1;\n");
  f.write("alpha.ts", "const alpha = 1;\n");
  f.write("notes.txt", "not formattable\n");
  f.commit();
  const paths = allFormatFiles(f.cwd);
  assert.deepEqual(paths, [
    "alpha.ts",
    "package.json",
    "rstack.config.ts",
    "zeta.ts",
  ]);
});

test("CLI --all reports one candidate per distinct path after a conflicted merge", (t) => {
  const f = fixture(t);
  f.write("zeta.ts", "const zeta = 1;\n");
  conflictedMerge(f);
  // `git ls-files` repeats the unmerged path once per stage; count it once.
  const distinct = new Set(f.git("ls-files").split("\n").filter(Boolean)).size;
  const result = spawnSync(
    process.execPath,
    [join(root, "scripts/ci/format-changed.mjs"), "--all"],
    { cwd: f.cwd, env: f.env, encoding: "utf8" },
  );
  const output = `${result.stdout}${result.stderr}`;
  // `.gitignore` is tracked but not a supported extension, so subtract it.
  const candidates = allFormatFiles(f.cwd).length;
  assert.equal(candidates, distinct - 1, output);
  assert.match(
    result.stdout,
    new RegExp(`"event":"ci_format".*"candidates":${candidates},`, "u"),
    output,
  );
  // The conflict markers are a parse error, which the formatter reports.
  assert.notEqual(result.status, 0, output);
});

test("Rstack owns exclusions and retains Markdown prose and embedded code", async (t) => {
  const f = fixture(t);
  const ignored = [
    "packages/example/src/gen/client.ts",
    "vendor/code.ts",
    "data/fixtures/input.json",
    "data/testdata/input.json",
    "pnpm-lock.yaml",
    "guidance.generated.json",
    "output/dist/code.ts",
  ];
  for (const name of ignored) f.write(name, "deliberately invalid {{{\n");
  f.commit();
  assert.equal(await runFormatter(f.cwd, ignored), 0);
  assert.ok(
    ignored.every(
      (name) =>
        readFileSync(join(f.cwd, name), "utf8") ===
        "deliberately invalid {{{\n",
    ),
  );
  const md =
    "# Guide\n\nA long prose line that stays on this line without being wrapped by formatting.\n\n```js\nconst x={a:1}\n```\n";
  f.write("guide.md", md);
  assert.equal(await runFormatter(f.cwd, ["guide.md"]), 0);
  assert.equal(readFileSync(join(f.cwd, "guide.md"), "utf8"), md);
});

test("authored localization JSON remains eligible for formatting", async (t) => {
  const f = fixture(t);
  const name = "apps/example-chrome-extension/public/_locales/en/messages.json";
  const source = '{"title":{"message":"Browser context"}}\n';
  f.write(name, source);
  const head = f.commit();
  const paths = changedFormatFiles(f.cwd, f.base, head);
  assert.deepEqual(paths, [name]);
  assert.equal(await runFormatter(f.cwd, paths), 1);
  assert.equal(readFileSync(join(f.cwd, name), "utf8"), source);
  assert.equal(await runFormatter(f.cwd, paths, "write"), 0);
  assert.deepEqual(
    JSON.parse(readFileSync(join(f.cwd, name), "utf8")),
    JSON.parse(source),
  );
  assert.equal(await runFormatter(f.cwd, paths), 0);
});

test("CLI rejects incomplete comparisons and CI-wide formatting", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  const env = { ...process.env, CI: "true" };
  delete env.TURBO_SCM_BASE;
  delete env.TURBO_SCM_HEAD;
  for (const args of [
    [],
    ["--base", f.base],
    ["--all", "--base", f.base, "--head", f.base],
    ["--staged"],
  ]) {
    const result = spawnSync(process.execPath, [script, ...args], {
      cwd: f.cwd,
      env,
      encoding: "utf8",
    });
    assert.equal(result.status, 2);
    assert.match(result.stderr, /ci_format_error/);
  }
});

test("CLI rejects staged selection with other selection modes", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  for (const args of [["--all"], ["--base", f.base, "--head", f.base]]) {
    const result = spawnSync(process.execPath, [script, "--staged", ...args], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
    assert.equal(result.status, 2);
    assert.match(result.stderr, /Staged formatting requires local execution/);
  }
  const result = spawnSync(process.execPath, [script, "--staged"], {
    cwd: f.cwd,
    env: { ...f.env, TURBO_SCM_BASE: f.base, TURBO_SCM_HEAD: f.base },
    encoding: "utf8",
  });
  assert.equal(result.status, 2);
});

test("CI is enabled unless unset, empty or an explicit false", () => {
  for (const value of [
    undefined,
    "",
    "  ",
    "false",
    "FALSE",
    "False",
    " false ",
  ])
    assert.equal(ciEnabled({ CI: value }), false, JSON.stringify(value));
  for (const value of ["true", "TRUE", "1", "0", "no", "falsey"])
    assert.equal(ciEnabled({ CI: value }), true, value);
  assert.equal(ciEnabled({}), false);
});

test("CLI treats an explicit false CI value as local staged execution", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  for (const ci of ["false", "FALSE", ""]) {
    const empty = spawnSync(process.execPath, [script, "--staged"], {
      cwd: f.cwd,
      env: { ...f.env, CI: ci },
      encoding: "utf8",
    });
    assert.equal(empty.status, 0, `${JSON.stringify(ci)}: ${empty.stderr}`);
    assert.match(empty.stdout, /"event":"ci_format_result"/);
    assert.doesNotMatch(empty.stderr, /ci_format_error/);
  }
  f.write("staged.ts", "const value={answer:42}\n");
  f.git("add", "staged.ts");
  const result = spawnSync(process.execPath, [script, "--staged", "--write"], {
    cwd: f.cwd,
    env: { ...f.env, CI: "false" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /"candidates":1/);
  assert.equal(
    readFileSync(join(f.cwd, "staged.ts"), "utf8"),
    "const value = { answer: 42 };\n",
  );
});

test("CLI keeps staged rejections when CI is explicitly false", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  const env = { ...f.env, CI: "false" };
  for (const args of [["--all"], ["--base", f.base, "--head", f.base]]) {
    const result = spawnSync(process.execPath, [script, "--staged", ...args], {
      cwd: f.cwd,
      env,
      encoding: "utf8",
    });
    assert.equal(result.status, 2);
    assert.match(result.stderr, /Staged formatting requires local execution/);
  }
  const result = spawnSync(process.execPath, [script, "--staged"], {
    cwd: f.cwd,
    env: { ...env, TURBO_SCM_BASE: f.base, TURBO_SCM_HEAD: f.base },
    encoding: "utf8",
  });
  assert.equal(result.status, 2);
  assert.match(result.stderr, /Staged formatting requires local execution/);
});

test("CLI keeps CI restrictions for non-false CI values", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  for (const ci of ["true", "TRUE", "1", "0"]) {
    const env = { ...f.env, CI: ci };
    for (const [args, message] of [
      [["--staged"], /Staged formatting requires local execution/],
      [[], /CI formatting requires the planner comparison/],
      [["--all"], /CI formatting requires the planner comparison/],
      [
        ["--all", "--base", f.base, "--head", f.base],
        /CI formatting must use changed files/,
      ],
    ]) {
      const result = spawnSync(process.execPath, [script, ...args], {
        cwd: f.cwd,
        env,
        encoding: "utf8",
      });
      assert.equal(result.status, 2, `${ci} ${args.join(" ")}`);
      assert.match(result.stderr, message);
    }
  }
});

test("explicit false CI behaves exactly like unset CI for non-staged runs", (t) => {
  const f = fixture(t);
  const script = join(root, "scripts/ci/format-changed.mjs");
  for (const args of [["--all"], ["--base", f.base, "--head", f.base], []]) {
    const run = (env) =>
      spawnSync(process.execPath, [script, ...args], {
        cwd: f.cwd,
        env,
        encoding: "utf8",
      });
    const unset = run(f.env);
    const disabled = run({ ...f.env, CI: "false" });
    assert.equal(disabled.status, unset.status, args.join(" "));
    // Rstack reports wall-clock duration; separate runs need not take equal time.
    const normalizeTiming = (output) =>
      output.replace(/\b\d+(?:\.\d+)?(?:ms|s)\b/gu, "<duration>");
    assert.equal(
      normalizeTiming(disabled.stdout),
      normalizeTiming(unset.stdout),
      args.join(" "),
    );
    assert.equal(disabled.stderr, unset.stderr, args.join(" "));
    assert.doesNotMatch(disabled.stderr, /CI formatting/);
  }
  const comparison = spawnSync(
    process.execPath,
    [script, "--base", f.base, "--head", f.base],
    { cwd: f.cwd, env: { ...f.env, CI: "false" }, encoding: "utf8" },
  );
  assert.equal(comparison.status, 0, comparison.stderr);
});

// Parent-path breakage: a tracked `a/b.ts` whose parent `a` became a regular
// file (ENOTDIR) or a self-referencing symlink (ELOOP). Git accepts both.
const brokenParents = [
  {
    name: "ENOTDIR",
    skip: false,
    breakParent: (f) => {
      rmSync(join(f.cwd, "a"), { recursive: true });
      f.write("a", "not a directory\n");
    },
  },
  {
    name: "ELOOP",
    skip: process.platform === "win32",
    breakParent: (f) => {
      rmSync(join(f.cwd, "a"), { recursive: true });
      symlinkSync("a", join(f.cwd, "a"));
    },
  },
];

for (const { name, skip, breakParent } of brokenParents) {
  const setup = (t) => {
    const f = fixture(t);
    f.write("a/b.ts", "const value = 1;\n");
    const head = f.commit();
    f.write("a/b.ts", "const value = 2;\n");
    f.git("add", "a/b.ts");
    breakParent(f);
    return { f, head };
  };

  test(
    `unstaged selectors skip tracked paths whose parent raises ${name}; staged selection fails`,
    { skip },
    (t) => {
      const { f, head } = setup(t);
      assert.doesNotMatch(allFormatFiles(f.cwd).join("\n"), /^a\/b\.ts$/mu);
      assert.deepEqual(changedFormatFiles(f.cwd, f.base, head, true), []);
      // A staged blob that cannot be formatted must block the commit.
      assert.throws(
        () => stagedFormatFiles(f.cwd),
        new RegExp(`${name}.*a/b\\.ts`, "u"),
      );
    },
  );

  test(
    `non-local comparison still throws when a parent raises ${name}`,
    { skip },
    (t) => {
      const { f, head } = setup(t);
      assert.throws(
        () => changedFormatFiles(f.cwd, f.base, head),
        new RegExp(name),
      );
    },
  );

  test(
    `--all does not crash and --staged --write exits 2 naming the path when a parent raises ${name}`,
    { skip },
    (t) => {
      const { f } = setup(t);
      const script = join(root, "scripts/ci/format-changed.mjs");
      const run = (args) =>
        spawnSync(process.execPath, [script, ...args], {
          cwd: f.cwd,
          env: f.env,
          encoding: "utf8",
        });
      // `--all` may exit 1 for unrelated fixture files; 2 is the crash code.
      const all = run(["--all"]);
      const allOutput = `--all: ${all.stdout}${all.stderr}`;
      assert.notEqual(all.status, 2, allOutput);
      assert.doesNotMatch(allOutput, /ci_format_error/u);
      const staged = run(["--staged", "--write"]);
      const stagedOutput = `--staged --write: ${staged.stdout}${staged.stderr}`;
      assert.equal(staged.status, 2, stagedOutput);
      assert.match(staged.stderr, /ci_format_error/u, stagedOutput);
      assert.match(staged.stderr, /a\/b\.ts/u, stagedOutput);
    },
  );
}

test(
  "selectors keep unrelated lstat errors fatal",
  { skip: process.platform === "win32" || process.getuid?.() === 0 },
  (t) => {
    const f = fixture(t);
    f.write("a/b.ts", "const value = 1;\n");
    const head = f.commit();
    f.git("add", "a/b.ts");
    chmodSync(join(f.cwd, "a"), 0o000);
    try {
      assert.throws(() => allFormatFiles(f.cwd), /EACCES/);
      assert.throws(
        () => changedFormatFiles(f.cwd, f.base, head, true),
        /EACCES/,
      );
    } finally {
      chmodSync(join(f.cwd, "a"), 0o755);
    }
  },
);

test("root package.json declares the ES module type used by rstack.config.ts", () => {
  // The fixtures write their own package.json, so they cannot show the Node
  // MODULE_TYPELESS_PACKAGE_JSON warning that a typeless root package causes.
  const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  assert.equal(manifest.type, "module");
});

test("formatter CLI runs main through a symlinked script path", (t) => {
  const f = fixture(t);
  f.write("unformatted.ts", "const value={answer:42}\n");
  f.git("add", "unformatted.ts");
  const links = mkdtempSync(join(tmpdir(), "rstack-format-link-"));
  t.after(() => rmSync(links, { recursive: true, force: true }));
  symlinkSync(
    join(root, "scripts/ci"),
    join(links, "ci"),
    process.platform === "win32" ? "junction" : "dir",
  );
  for (const script of [
    join(root, "scripts/ci/format-changed.mjs"),
    join(links, "ci/format-changed.mjs"),
  ]) {
    const result = spawnSync(process.execPath, [script, "--staged"], {
      cwd: f.cwd,
      env: f.env,
      encoding: "utf8",
    });
    assert.match(result.stderr + result.stdout, /"event":"ci_format/, script);
  }
});

// Stand-in Rstack for process-boundary tests: records its pid per batch and
// every termination signal it receives, then either exits with STANDIN_EXIT or
// sleeps long enough for the test to signal the parent. The signal handlers are
// installed before the batch is recorded: the test signals as soon as it sees
// batches.log, and a signal that arrived earlier would kill the stand-in by the
// default action without a signals.log entry.
const standIn = `// Test stand-in for the Rstack entry point.
const fs = require("node:fs");
const dir = process.env.STANDIN_DIR;
for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"])
  process.on(signal, () => {
    fs.appendFileSync(dir + "/signals.log", process.pid + " " + signal + "\\n");
    process.exit(0);
  });
fs.appendFileSync(dir + "/batches.log", process.pid + "\\n");
if (process.env.STANDIN_EXIT) process.exit(Number(process.env.STANDIN_EXIT));
setTimeout(() => {
  fs.appendFileSync(dir + "/marker", process.pid + "\\n");
  process.exit(0);
}, 5000);
`;

// An isolated repository with its own copy of the script and a stand-in
// node_modules/rstack; the standard fixture symlinks the real node_modules.
function standInFixture(t, fileCount) {
  const f = fixture(t, false);
  rmSync(join(f.cwd, "node_modules"), { recursive: true });
  f.write(
    "node_modules/rstack/package.json",
    '{"name":"rstack","version":"0.0.0","bin":{"rs":"./bin/rs.js"}}\n',
  );
  f.write("node_modules/rstack/bin/rs.js", standIn);
  f.write(
    "scripts/ci/format-changed.mjs",
    readFileSync(join(root, "scripts/ci/format-changed.mjs"), "utf8"),
  );
  for (let index = 0; index < fileCount; index += 1)
    f.write(`files/f${index}.ts`, `export const value${index} = 1;\n`);
  f.commit();
  const state = join(f.cwd, "state");
  mkdirSync(state);
  const lines = (name) => {
    try {
      return readFileSync(join(state, name), "utf8")
        .split("\n")
        .filter(Boolean);
    } catch {
      return [];
    }
  };
  return { f, state, lines };
}

async function waitFor(predicate, what) {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return;
    await new Promise((done) => setTimeout(done, 50));
  }
  assert.fail(`Timed out waiting for ${what}`);
}

function isAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error.code !== "ESRCH";
  }
}

const signalCases = [
  { signal: "SIGTERM", exitCode: 143 },
  { signal: "SIGINT", exitCode: 130 },
  { signal: "SIGHUP", exitCode: 129 },
];

for (const { signal, exitCode } of signalCases) {
  test(
    `${signal} to the formatter is forwarded to the Rstack child and stops further batches`,
    { skip: process.platform === "win32" },
    async (t) => {
      // 120 tracked files plus the fixture files are more than two 50-path batches.
      const { f, state, lines } = standInFixture(t, 120);
      const parent = spawn(
        process.execPath,
        [join(f.cwd, "scripts/ci/format-changed.mjs"), "--all"],
        {
          cwd: f.cwd,
          env: { ...f.env, STANDIN_DIR: state },
          stdio: ["ignore", "ignore", "pipe"],
        },
      );
      t.after(() => parent.kill("SIGKILL"));
      let stderr = "";
      parent.stderr.on("data", (chunk) => (stderr += chunk));
      const closed = new Promise((done) =>
        parent.once("close", (code, closeSignal) =>
          done({ code, closeSignal }),
        ),
      );
      await waitFor(() => lines("batches.log").length > 0, "the first batch");
      const child = Number(lines("batches.log")[0]);
      parent.kill(signal);
      const { code, closeSignal } = await closed;
      assert.equal(closeSignal, null, "the parent must exit by itself");
      assert.equal(code, exitCode);
      assert.deepEqual(lines("signals.log"), [`${child} ${signal}`]);
      assert.equal(isAlive(child), false, "the child must be dead");
      assert.equal(lines("batches.log").length, 1, "no further batch starts");
      assert.deepEqual(
        lines("marker"),
        [],
        "the child must not run to the end",
      );
      assert.match(
        stderr,
        new RegExp(`"event":"ci_format_signal","signal":"${signal}"`),
      );
    },
  );
}

for (const status of [0, 1, 2]) {
  test(`formatter exits ${status} when every Rstack batch exits ${status}`, async (t) => {
    const { f, state, lines } = standInFixture(t, 60);
    const result = spawnSync(
      process.execPath,
      [join(f.cwd, "scripts/ci/format-changed.mjs"), "--all"],
      {
        cwd: f.cwd,
        env: { ...f.env, STANDIN_DIR: state, STANDIN_EXIT: String(status) },
        encoding: "utf8",
      },
    );
    assert.equal(result.status, status, result.stderr);
    assert.ok(lines("batches.log").length > 1, "every batch runs");
    assert.match(result.stdout, new RegExp(`"status":${status}`));
  });
}

// Pathspec commits run the hook against Git's throwaway `next-index-*.lock`
// index, so the hook must refuse to format there instead of staging a fix that
// Git then discards. These fixtures drive the real Lefthook hook.
const Unformatted = "const a={x:2}\n";
const Formatted = "const a = { x: 2 };";

// A fixture whose hook files are committed, so status output only lists the
// files a scenario creates.
function hookedFixture(t) {
  const f = fixture(t);
  f.installHook();
  f.git("add", ".");
  f.git("commit", "--quiet", "-m", "hook files");
  return f;
}

function runCommit(f, ...args) {
  return spawnSync("git", ["commit", "--quiet", ...args], {
    cwd: f.cwd,
    env: f.env,
    encoding: "utf8",
  });
}

function runCommitIn(cwd, env, ...args) {
  return spawnSync("git", ["commit", "--quiet", ...args], {
    cwd,
    env,
    encoding: "utf8",
  });
}

function assertNoHookLeftovers(f) {
  assert.equal(f.git("stash", "list"), "");
  const info = join(f.cwd, f.git("rev-parse", "--git-path", "info"));
  assert.deepEqual(
    readdirSync(info).filter((name) => name.endsWith(".patch")),
    [],
  );
}

test("isThrowawayIndex matches only Git's pathspec-commit temporary index", () => {
  for (const GIT_INDEX_FILE of [
    undefined,
    "",
    ".git/index",
    "/x/.git/index",
    "/x/.git/index.lock",
    ".git/index.lock",
    "/x/.git/worktrees/w/index",
    "/x/.git/worktrees/w/index.lock",
    "C:\\x\\.git\\index",
  ])
    assert.equal(isThrowawayIndex({ GIT_INDEX_FILE }), false, GIT_INDEX_FILE);
  for (const GIT_INDEX_FILE of [
    "/x/.git/next-index-91305.lock",
    ".git/next-index-1263.lock",
    "/x/.git/worktrees/w/next-index-7.lock",
    "C:\\x\\.git\\next-index-7.lock",
  ])
    assert.equal(isThrowawayIndex({ GIT_INDEX_FILE }), true, GIT_INDEX_FILE);
});

test("pre-commit refuses a pathspec commit that needs formatting and recovers through add plus commit", (t) => {
  const f = hookedFixture(t);
  f.write("a.ts", Unformatted);
  f.write("b.ts", "const b={y:1}\n");
  f.git("add", "a.ts", "b.ts");
  const head = f.git("rev-parse", "HEAD");
  const result = runCommit(f, "-m", "pathspec", "--", "a.ts");
  const output = result.stdout + result.stderr;
  assert.notEqual(result.status, 0, output);
  assert.match(output, /ci_format_error/);
  assert.match(output, /git commit <path> cannot be formatted/);
  assert.equal(f.git("rev-parse", "HEAD"), head);
  assert.equal(f.git("show", ":a.ts"), Unformatted.trim());
  assert.equal(readFileSync(join(f.cwd, "a.ts"), "utf8"), Unformatted);
  assert.equal(f.git("status", "--porcelain"), "A  a.ts\nA  b.ts");
  assertNoHookLeftovers(f);
  f.git("add", "--", "a.ts", "b.ts");
  f.git("commit", "--quiet", "-m", "recovered");
  assert.equal(f.git("show", "HEAD:a.ts"), Formatted);
  assert.equal(f.git("show", "HEAD:b.ts"), "const b = { y: 1 };");
  assert.equal(f.git("status", "--porcelain"), "");
});

test("pre-commit refuses a pathspec commit with a parser error", (t) => {
  const f = hookedFixture(t);
  f.write("broken.ts", "const broken = ;\n");
  f.git("add", "broken.ts");
  const head = f.git("rev-parse", "HEAD");
  const result = runCommit(f, "-m", "pathspec", "--", "broken.ts");
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.equal(f.git("rev-parse", "--verify", "HEAD"), head);
  assert.equal(f.git("status", "--porcelain"), "A  broken.ts");
});

test("pre-commit lets a pathspec commit of formatted content through", (t) => {
  const f = hookedFixture(t);
  f.write("c.ts", "const c = { x: 1 };\n");
  f.git("add", "c.ts");
  const result = runCommit(f, "-m", "pathspec formatted", "--", "c.ts");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(f.git("show", "HEAD:c.ts"), "const c = { x: 1 };");
  assert.equal(f.git("status", "--porcelain"), "");
});

test("pre-commit still formats git commit -a and keeps the index clean", (t) => {
  const f = hookedFixture(t);
  f.write("a.ts", "const a = { x: 1 };\n");
  f.git("add", "a.ts");
  f.git("commit", "--quiet", "-m", "formatted base");
  f.write("a.ts", Unformatted);
  const result = runCommit(f, "-a", "-m", "all");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(f.git("show", "HEAD:a.ts"), Formatted);
  assert.equal(f.git("show", ":a.ts"), Formatted);
  assert.equal(f.git("status", "--porcelain"), "");
});

test("pre-commit still formats git commit -i and keeps the index clean", (t) => {
  const f = hookedFixture(t);
  f.write("a.ts", "const a = { x: 1 };\n");
  f.git("add", "a.ts");
  f.git("commit", "--quiet", "-m", "formatted base");
  f.write("a.ts", Unformatted);
  f.git("add", "a.ts");
  const result = runCommit(f, "-i", "-m", "include", "--", "a.ts");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(f.git("show", "HEAD:a.ts"), Formatted);
  assert.equal(f.git("show", ":a.ts"), Formatted);
  assert.equal(f.git("status", "--porcelain"), "");
});

test("pre-commit refuses git commit --only and leaves the staged state untouched", (t) => {
  const f = hookedFixture(t);
  f.write("a.ts", "const a = { x: 1 };\n");
  f.git("add", "a.ts");
  f.git("commit", "--quiet", "-m", "formatted base");
  f.write("a.ts", Unformatted);
  const head = f.git("rev-parse", "HEAD");
  const result = runCommit(f, "--only", "-m", "only", "a.ts");
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout + result.stderr, /ci_format_error/);
  assert.equal(f.git("rev-parse", "HEAD"), head);
  assert.equal(readFileSync(join(f.cwd, "a.ts"), "utf8"), Unformatted);
  assert.equal(f.git("status", "--porcelain"), "M a.ts");
  assertNoHookLeftovers(f);
});

test("pre-commit refuses git commit --amend with a path and keeps the amended commit", (t) => {
  const f = hookedFixture(t);
  f.write("a.ts", "const a = { x: 1 };\n");
  f.git("add", "a.ts");
  f.git("commit", "--quiet", "-m", "formatted");
  const head = f.git("rev-parse", "HEAD");
  f.write("a.ts", Unformatted);
  const result = runCommit(f, "--amend", "--no-edit", "--", "a.ts");
  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout + result.stderr, /ci_format_error/);
  assert.equal(f.git("rev-parse", "HEAD"), head);
  assert.equal(f.git("show", "HEAD:a.ts"), "const a = { x: 1 };");
  assert.equal(readFileSync(join(f.cwd, "a.ts"), "utf8"), Unformatted);
  assert.equal(f.git("status", "--porcelain"), "M a.ts");
  assertNoHookLeftovers(f);
});

test("pre-commit formats plain commits from a linked worktree", (t) => {
  const f = hookedFixture(t);
  const linked = join(f.cwd, "..", `${f.cwd.split(/[\\/]/u).pop()}-linked`);
  t.after(() => rmSync(linked, { recursive: true, force: true }));
  f.git("worktree", "add", "--quiet", "-b", "linked", linked);
  symlinkSync(
    join(root, "node_modules"),
    join(linked, "node_modules"),
    process.platform === "win32" ? "junction" : "dir",
  );
  writeFileSync(join(linked, "a.ts"), Unformatted);
  const git = (...args) =>
    execFileSync("git", args, {
      cwd: linked,
      env: f.env,
      encoding: "utf8",
    }).trim();
  git("add", "a.ts");
  const plain = runCommitIn(linked, f.env, "-m", "linked");
  assert.equal(plain.status, 0, plain.stdout + plain.stderr);
  assert.equal(git("show", "HEAD:a.ts"), Formatted);
  assert.equal(git("show", ":a.ts"), Formatted);
  assert.equal(git("status", "--porcelain"), "");
  // A pathspec commit in the linked worktree is refused the same way.
  writeFileSync(join(linked, "d.ts"), "const d={z:3}\n");
  git("add", "d.ts");
  const head = git("rev-parse", "HEAD");
  const refused = runCommitIn(linked, f.env, "-m", "linked path", "--", "d.ts");
  assert.notEqual(refused.status, 0, refused.stdout + refused.stderr);
  assert.equal(git("rev-parse", "HEAD"), head);
  assert.equal(git("status", "--porcelain"), "A  d.ts");
});

// Selection must depend on the repository, never on the directory the script
// is started from. `git diff` prints repository-root-relative paths while
// `git ls-files` prints paths relative to, and limited to, its working
// directory; a selector that mixed the two returned an empty or partial list
// from a subdirectory.
const subdirectoryFiles = {
  "apps/web/page.ts": "const   page = 1;\n",
  "docs/guide.md": "# Guide\n\n*  item\n",
  "scripts/tool.ts": "const   tool = 1;\n",
  "packages/ui/data.JSON": '{"a":1}\n',
};
const subdirectories = ["scripts", "apps/web", "docs"];

function subdirectoryFixture(t) {
  const f = fixture(t);
  for (const [name, text] of Object.entries(subdirectoryFiles)) {
    f.write(name, text);
  }
  return f;
}

test("every selector returns the same repository-wide files from a subdirectory", (t) => {
  const f = subdirectoryFixture(t);
  const names = Object.keys(subdirectoryFiles).sort();
  f.git("add", "--", ...names);
  const staged = stagedFormatFiles(f.cwd);
  assert.deepEqual(staged, names);
  const base = f.base;
  const head = f.commit();
  f.write("docs/guide.md", "# Guide\n\n*  changed\n");
  f.write("untracked/new.ts", "const   fresh = 1;\n");
  const all = allFormatFiles(f.cwd);
  assert.ok(all.includes("apps/web/page.ts"), all.join(","));
  const explicit = changedFormatFiles(f.cwd, base, head);
  assert.deepEqual(explicit, names);
  const local = changedFormatFiles(f.cwd, head, head, true);
  assert.deepEqual(local, ["docs/guide.md", "untracked/new.ts"]);
  for (const sub of subdirectories) {
    const dir = join(f.cwd, sub);
    assert.deepEqual(allFormatFiles(dir), all, `--all from ${sub}`);
    assert.deepEqual(
      changedFormatFiles(dir, base, head),
      explicit,
      `explicit comparison from ${sub}`,
    );
    assert.deepEqual(
      changedFormatFiles(dir, head, head, true),
      local,
      `local selection from ${sub}`,
    );
  }
});

test("staged selection from a subdirectory matches the root, including the missing-path failure", (t) => {
  const f = subdirectoryFixture(t);
  const names = Object.keys(subdirectoryFiles).sort();
  f.git("add", "--", ...names);
  for (const sub of subdirectories) {
    assert.deepEqual(stagedFormatFiles(join(f.cwd, sub)), names, sub);
  }
  // The missing-path check resolves against the root, so a path that is really
  // absent still fails and a path that exists no longer fails from a subdirectory.
  rmSync(join(f.cwd, "docs/guide.md"));
  for (const dir of [f.cwd, join(f.cwd, "scripts")]) {
    assert.throws(
      () => stagedFormatFiles(dir),
      /missing from the working tree.*docs\/guide\.md/u,
    );
  }
});

test("symlink and exclusion handling is unchanged when run from a subdirectory", (t) => {
  const f = fixture(t);
  f.write("scripts/regular.ts", "const regular = 1;\n");
  f.write("vendor/code.ts", "const   vendored = 1;\n");
  recordSymlink(f, "apps/linked.ts");
  recordSymlink(f, "scripts/linked.md", "../README.md");
  const head = f.commit();
  const dir = join(f.cwd, "scripts");
  assert.ok(!allFormatFiles(dir).some((name) => /linked/u.test(name)));
  assert.deepEqual(
    changedFormatFiles(dir, f.base, head),
    changedFormatFiles(f.cwd, f.base, head),
  );
  assert.ok(
    !changedFormatFiles(dir, f.base, head).some((n) => /linked/u.test(n)),
  );
  // A staged excluded path missing from the worktree is skipped by the
  // exclusion probe, which must also run at the repository root.
  f.write("vendor/code.ts", "const   vendored = 2;\n");
  f.git("add", "--", "vendor/code.ts");
  rmSync(join(f.cwd, "vendor/code.ts"));
  assert.deepEqual(stagedFormatFiles(dir), []);
  assert.deepEqual(stagedFormatFiles(f.cwd), []);
});

test("runFormatter started in a subdirectory formats repository-root-relative paths", async (t) => {
  const f = subdirectoryFixture(t);
  const dir = join(f.cwd, "scripts");
  const paths = ["apps/web/page.ts", "docs/guide.md"];
  assert.equal(await runFormatter(dir, paths), 1);
  assert.equal(await runFormatter(dir, paths, "write"), 0);
  assert.equal(
    readFileSync(join(f.cwd, "apps/web/page.ts"), "utf8"),
    "const page = 1;\n",
  );
  assert.equal(
    readFileSync(join(f.cwd, "docs/guide.md"), "utf8"),
    "# Guide\n\n- item\n",
  );
  assert.equal(await runFormatter(dir, paths), 0);
});

test("CLI selects the same files and exit status from a subdirectory as from the root", (t) => {
  const f = subdirectoryFixture(t);
  const names = Object.keys(subdirectoryFiles).sort();
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = (cwd, args) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd,
      env: f.env,
      encoding: "utf8",
    });
  // Reduces a run to what must not depend on the invoking directory.
  const outcome = (result) => {
    const record = JSON.parse(
      result.stdout.split("\n").find((line) => line.includes('"ci_format"')),
    );
    return {
      status: result.status,
      candidates: record.candidates,
      base: record.base,
      head: record.head,
      staged: record.staged,
      local: record.local,
    };
  };
  const base = f.base;
  f.git("add", "--", ...names);
  const modes = [["--staged"]];
  const head = f.commit();
  modes.push(["--base", base, "--head", head], ["--all"], []);
  f.write("untracked/new.ts", "const   fresh = 1;\n");
  for (const args of modes) {
    // The default local run needs origin/main; point it at the base commit.
    if (args.length === 0)
      f.git("update-ref", "refs/remotes/origin/main", base);
    if (args[0] === "--staged") {
      f.write("staged/one.ts", "const   one = 1;\n");
      f.git("add", "--", "staged/one.ts");
    }
    const expected = outcome(run(f.cwd, args));
    assert.ok(expected.candidates > 0, JSON.stringify([args, expected]));
    assert.equal(expected.status, 1, JSON.stringify([args, expected]));
    for (const sub of subdirectories) {
      const result = run(join(f.cwd, sub), args);
      assert.deepEqual(
        outcome(result),
        expected,
        `${args.join(" ")} from ${sub}\n${result.stdout}${result.stderr}`,
      );
    }
    if (args[0] === "--staged")
      f.git("reset", "--quiet", "--", "staged/one.ts");
  }
});

test("CLI --write from a subdirectory formats files outside that subdirectory", (t) => {
  const f = subdirectoryFixture(t);
  const base = f.base;
  const head = f.commit();
  const result = spawnSync(
    process.execPath,
    [
      join(root, "scripts/ci/format-changed.mjs"),
      "--base",
      base,
      "--head",
      head,
      "--write",
    ],
    { cwd: join(f.cwd, "scripts"), env: f.env, encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /"candidates":4,/u, result.stdout);
  assert.equal(
    readFileSync(join(f.cwd, "apps/web/page.ts"), "utf8"),
    "const page = 1;\n",
  );
  assert.equal(
    readFileSync(join(f.cwd, "docs/guide.md"), "utf8"),
    "# Guide\n\n- item\n",
  );
});

test("pre-commit hook formats staged files when the commit runs in a subdirectory", (t) => {
  const f = subdirectoryFixture(t);
  f.installHook();
  f.git("add", "--", "apps/web/page.ts", "docs/guide.md", "scripts/tool.ts");
  execFileSync("git", ["commit", "--quiet", "-m", "from subdirectory"], {
    cwd: join(f.cwd, "scripts"),
    env: f.env,
    encoding: "utf8",
  });
  assert.equal(f.git("show", "HEAD:apps/web/page.ts"), "const page = 1;");
  assert.equal(f.git("show", "HEAD:docs/guide.md"), "# Guide\n\n- item");
  assert.equal(f.git("show", "HEAD:scripts/tool.ts"), "const tool = 1;");
});

// `git rev-parse --show-toplevel` ends its output with one newline that is not
// part of the path. A checkout directory whose own name ends in whitespace must
// survive that newline removal; trimming all whitespace pointed every selector
// at a nonexistent directory (`spawnSync git ENOENT`).
const trailingSpaceDirectory = "checkout ";

function trailingSpaceFixture(t) {
  let f;
  try {
    f = fixture(t, true, process.env, trailingSpaceDirectory);
  } catch (error) {
    // Windows rejects or silently strips a trailing space in a directory name.
    if (process.platform === "win32") return null;
    throw error;
  }
  if (!f.cwd.endsWith(trailingSpaceDirectory)) return null;
  if (!readdirSync(dirname(f.cwd)).includes(trailingSpaceDirectory))
    return null;
  for (const [name, text] of Object.entries(subdirectoryFiles)) {
    f.write(name, text);
  }
  return f;
}

test("selectors work in a repository directory whose name ends in whitespace", (t) => {
  const f = trailingSpaceFixture(t);
  if (f === null)
    return t.skip("trailing-space directory names are unsupported");
  const names = Object.keys(subdirectoryFiles).sort();
  f.git("add", "--", ...names);
  const base = f.base;
  for (const dir of [f.cwd, join(f.cwd, "scripts")]) {
    assert.deepEqual(stagedFormatFiles(dir), names, `--staged from ${dir}`);
  }
  const head = f.commit();
  for (const dir of [f.cwd, join(f.cwd, "scripts"), join(f.cwd, "apps/web")]) {
    assert.deepEqual(
      changedFormatFiles(dir, base, head),
      names,
      `explicit comparison from ${dir}`,
    );
    assert.deepEqual(changedFormatFiles(dir, head, head, true), []);
    assert.ok(
      allFormatFiles(dir).includes("apps/web/page.ts"),
      `--all from ${dir}`,
    );
  }
});

test("CLI runs from the root and a subdirectory of a whitespace-terminated repository path", (t) => {
  const f = trailingSpaceFixture(t);
  if (f === null)
    return t.skip("trailing-space directory names are unsupported");
  const names = Object.keys(subdirectoryFiles).sort();
  const script = join(root, "scripts/ci/format-changed.mjs");
  const run = (cwd, args) =>
    spawnSync(process.execPath, [script, ...args], {
      cwd,
      env: f.env,
      encoding: "utf8",
    });
  f.git("add", "--", ...names);
  for (const args of [["--staged"], ["--all"]]) {
    for (const dir of [f.cwd, join(f.cwd, "scripts")]) {
      const result = run(dir, args);
      const detail = `${args.join(" ")} from ${dir}\n${result.stdout}${result.stderr}`;
      assert.doesNotMatch(result.stderr, /ENOENT/u, detail);
      assert.match(result.stdout, /"ci_format"/u, detail);
      // The unformatted fixture files fail the check; an unusable root would
      // have thrown before any candidate was selected.
      assert.equal(result.status, 1, detail);
      assert.match(result.stdout, /"candidates":[1-9]/u, detail);
    }
  }
});
