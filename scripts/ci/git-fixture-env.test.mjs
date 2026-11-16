// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { isolateGitEnvironment } from "./git-fixture-env.mjs";

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
