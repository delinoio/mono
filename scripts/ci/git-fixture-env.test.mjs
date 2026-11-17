// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { isolateGitEnvironment, parseGitVersion } from "./git-fixture-env.mjs";

const nullDevice = process.platform === "win32" ? "NUL" : "/dev/null";

// Creates a scratch directory holding `git/ignore` and `git/attributes`, the
// files Git reads from its XDG default location without any configuration.
function xdgConfigDirectory(t, ignore, attributes) {
  const directory = mkdtempSync(join(tmpdir(), "git-fixture-env-xdg-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  mkdirSync(join(directory, "git"), { recursive: true });
  writeFileSync(join(directory, "git", "ignore"), ignore);
  writeFileSync(join(directory, "git", "attributes"), attributes);
  return directory;
}

// Runs Git in a fresh repository with the isolated environment plus the given
// ambient overrides, which stand in for the developer's own environment.
function repository(t, ambient) {
  const cwd = mkdtempSync(join(tmpdir(), "git-fixture-env-repo-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const env = isolateGitEnvironment({ PATH: process.env.PATH, ...ambient });
  const git = (...args) =>
    execFileSync("git", args, { cwd, env, encoding: "utf8" }).trim();
  git("init", "--quiet");
  return { cwd, git };
}

test("isolation returns the documented environment and drops ambient GIT_* and LEFTHOOK* variables", () => {
  const env = isolateGitEnvironment({
    PATH: "/bin",
    GIT_DIR: "/somewhere",
    GIT_ATTR_NOSYSTEM: "0",
    GIT_CONFIG_COUNT: "1",
    GIT_CONFIG_KEY_0: "core.excludesFile",
    GIT_CONFIG_VALUE_0: "/somewhere/ignore",
    LEFTHOOK: "0",
    LEFTHOOK_BIN: "/somewhere/lefthook",
    LEFTHOOK_CONFIG: "/somewhere/lefthook.yml",
    LEFTHOOK_EXCLUDE: "rstack-fmt",
    LEFTHOOK_OUTPUT: "summary",
    UNRELATED: "kept",
  });
  assert.deepEqual(env, {
    PATH: "/bin",
    UNRELATED: "kept",
    GIT_CONFIG_GLOBAL: nullDevice,
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_ATTR_NOSYSTEM: "1",
    GIT_CONFIG_COUNT: "2",
    GIT_CONFIG_KEY_0: "core.excludesFile",
    GIT_CONFIG_VALUE_0: nullDevice,
    GIT_CONFIG_KEY_1: "core.attributesFile",
    GIT_CONFIG_VALUE_1: nullDevice,
  });
});

test("isolation ignores the global ignore file under XDG_CONFIG_HOME", (t) => {
  const xdg = xdgConfigDirectory(t, "*.json\n", "*.dat -text\n");
  const { cwd, git } = repository(t, { XDG_CONFIG_HOME: xdg });
  writeFileSync(join(cwd, "a.json"), "{}\n");
  writeFileSync(join(cwd, "b.dat"), "data\n");
  git("add", "--", "a.json", "b.dat");
  assert.deepEqual(git("ls-files").split("\n"), ["a.json", "b.dat"]);
});

test("isolation ignores the global attributes file under XDG_CONFIG_HOME", (t) => {
  const xdg = xdgConfigDirectory(t, "*.json\n", "*.dat -text\n");
  const { git } = repository(t, { XDG_CONFIG_HOME: xdg });
  assert.equal(
    git("check-attr", "text", "--", "b.dat"),
    "b.dat: text: unspecified",
  );
});

test("isolation ignores the global Git files under HOME when XDG_CONFIG_HOME is unset", (t) => {
  const home = mkdtempSync(join(tmpdir(), "git-fixture-env-home-"));
  t.after(() => rmSync(home, { recursive: true, force: true }));
  mkdirSync(join(home, ".config", "git"), { recursive: true });
  writeFileSync(join(home, ".config", "git", "ignore"), "*.json\n");
  writeFileSync(join(home, ".config", "git", "attributes"), "*.dat -text\n");
  const { cwd, git } = repository(t, { HOME: home });
  writeFileSync(join(cwd, "a.json"), "{}\n");
  writeFileSync(join(cwd, "b.dat"), "data\n");
  git("add", "--", "a.json", "b.dat");
  assert.deepEqual(git("ls-files").split("\n"), ["a.json", "b.dat"]);
  assert.equal(
    git("check-attr", "text", "--", "b.dat"),
    "b.dat: text: unspecified",
  );
});

test("isolation lets a path under a globally ignored directory be added", (t) => {
  const xdg = xdgConfigDirectory(t, "vendor/\n", "");
  const { cwd, git } = repository(t, { XDG_CONFIG_HOME: xdg });
  mkdirSync(join(cwd, "vendor"));
  writeFileSync(join(cwd, "vendor", "ignored.ts"), "export {};\n");
  git("add", "--", "vendor/ignored.ts");
  assert.equal(git("ls-files"), "vendor/ignored.ts");
});

test("parseGitVersion reads stock, vendor-suffixed and unparseable output", () => {
  assert.deepEqual(parseGitVersion("git version 2.43.0"), {
    major: 2,
    minor: 43,
  });
  assert.deepEqual(parseGitVersion("git version 2.31.1"), {
    major: 2,
    minor: 31,
  });
  assert.deepEqual(parseGitVersion("git version 2.43.0.windows.1"), {
    major: 2,
    minor: 43,
  });
  assert.deepEqual(parseGitVersion("git version 2.43.0.vfs.0.1"), {
    major: 2,
    minor: 43,
  });
  assert.deepEqual(parseGitVersion("git version 2.39.5 (Apple Git-154)"), {
    major: 2,
    minor: 39,
  });
  assert.equal(parseGitVersion("not git"), null);
});

// Runs the helper in a child node process (fresh version cache) with a fake
// `git` that prints `versionLine` first on PATH.
function gateWithFakeGit(t, versionLine) {
  const bin = mkdtempSync(join(tmpdir(), "git-fixture-env-fakegit-"));
  t.after(() => rmSync(bin, { recursive: true, force: true }));
  writeFileSync(join(bin, "git"), `#!/bin/sh\necho '${versionLine}'\n`);
  chmodSync(join(bin, "git"), 0o755);
  const helper = new URL("./git-fixture-env.mjs", import.meta.url).href;
  const script = `import(${JSON.stringify(helper)}).then((m) => { console.log(JSON.stringify(m.isolateGitEnvironment({}))); });`;
  return spawnSync(process.execPath, ["-e", script], {
    encoding: "utf8",
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
  });
}

test(
  "version gate rejects Git 2.31 and names the minimum and GIT_CONFIG_GLOBAL",
  { skip: process.platform === "win32" },
  (t) => {
    const result = gateWithFakeGit(t, "git version 2.31.1");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /2\.31/);
    assert.match(result.stderr, /Git 2\.32/);
    assert.match(result.stderr, /GIT_CONFIG_GLOBAL/);
  },
);

test(
  "version gate accepts Git 2.32.0",
  { skip: process.platform === "win32" },
  (t) => {
    const result = gateWithFakeGit(t, "git version 2.32.0");
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).GIT_CONFIG_GLOBAL, nullDevice);
  },
);

test(
  "version gate rejects unparseable output and quotes it",
  { skip: process.platform === "win32" },
  (t) => {
    const result = gateWithFakeGit(t, "hello");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /hello/);
    assert.match(result.stderr, /GIT_CONFIG_GLOBAL/);
  },
);

test("isolation ignores a hostile global gitconfig under HOME", (t) => {
  const home = mkdtempSync(join(tmpdir(), "git-fixture-env-home-"));
  t.after(() => rmSync(home, { recursive: true, force: true }));
  writeFileSync(
    join(home, ".gitconfig"),
    "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /bin/false\n",
  );
  const { git } = repository(t, { HOME: home });
  git(
    "-c",
    "user.name=t",
    "-c",
    "user.email=t@example.com",
    "commit",
    "--allow-empty",
    "-m",
    "x",
  );
  assert.equal(git("rev-list", "--count", "HEAD"), "1");
});
