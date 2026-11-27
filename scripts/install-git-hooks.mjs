// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { lstatSync, readFileSync, realpathSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import { basename, dirname, isAbsolute, join, sep } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const gitExecutable = process.platform === "win32" ? "git.exe" : "git";

function git(args, missingStatus) {
  const result = spawnSync(gitExecutable, args, { encoding: "utf8" });
  if (result.error) {
    throw result.error;
  }
  if (result.status === missingStatus) {
    return null;
  }
  if (result.status !== 0) {
    throw new Error(result.stderr.trim() || `git ${args.join(" ")} failed`);
  }
  return result.stdout;
}

function realPath(path) {
  try {
    return realpathSync.native(path);
  } catch (error) {
    if (error.code !== "ENOENT" && error.code !== "ENOTDIR") {
      throw error;
    }
    return null;
  }
}

function comparableHooksPath(path) {
  // Resolve symlinks before interpreting ".." in an existing path. For a
  // missing hooks directory, resolve its parent and keep the final component.
  const existingPath = realPath(path);
  if (existingPath !== null) {
    return existingPath;
  }
  while (basename(path) === ".") {
    path = dirname(path);
  }
  const parent = realPath(dirname(path));
  return parent === null ? null : join(parent, basename(path));
}

function protectHookBackup(hooksPath) {
  const hook = join(hooksPath, "pre-commit");
  const backup = `${hook}.old`;
  if (
    lstatSync(backup, { throwIfNoEntry: false }) &&
    statSync(hook, { throwIfNoEntry: false })?.isFile() &&
    !readFileSync(hook).includes("LEFTHOOK")
  ) {
    throw new Error(
      `Refusing to install Lefthook: ${hook} is not managed by Lefthook and ${backup} already exists.\n` +
        "Move or merge one of the two files, then run pnpm run prepare again.",
    );
  }
}

function workTreeRoot() {
  try {
    if (git(["rev-parse", "--is-inside-work-tree"], 128)?.trim() === "true") {
      return git(["rev-parse", "--show-toplevel"]).replace(/\r?\n$/, "");
    }
    return null;
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
    return null;
  }
}

function install() {
  const projectRoot = realpathSync.native(
    fileURLToPath(new URL("..", import.meta.url)),
  );
  const repoRoot = workTreeRoot();

  // An archive below another worktree does not own that worktree's Git hooks.
  if (repoRoot === null || realpathSync.native(repoRoot) !== projectRoot) {
    console.log("Git metadata is unavailable; skipping Lefthook installation.");
    return 0;
  }

  const args = ["install"];
  const configuredPath = git(
    ["config", "--null", "--path", "--get", "core.hooksPath"],
    1,
  )?.slice(0, -1);
  if (configuredPath) {
    const commonDir = git([
      "rev-parse",
      "--path-format=absolute",
      "--git-common-dir",
    ]).replace(/\r?\n$/, "");
    const absolutePath = isAbsolute(configuredPath)
      ? configuredPath
      : `${repoRoot}${sep}${configuredPath}`;
    const hooksPath = comparableHooksPath(absolutePath);

    if (
      hooksPath !== null &&
      hooksPath === comparableHooksPath(join(commonDir, "hooks"))
    ) {
      // Lefthook rejects a configured hooks path even when it points to Git's
      // shared hooks directory. Force only for that directory and check the
      // pre-commit backup because force bypasses Lefthook's overwrite guard.
      // Remove this branch when Lefthook handles the shared path safely.
      protectHookBackup(hooksPath);
      args.push("--force");
    }
  }

  const { getExePath } = require("lefthook/get-exe.js");
  const result = spawnSync(getExePath(), args, { stdio: "inherit" });
  if (result.error) {
    throw result.error;
  }
  if (result.signal) {
    throw new Error(`Lefthook stopped with signal ${result.signal}`);
  }
  return result.status;
}

try {
  process.exitCode = install();
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
