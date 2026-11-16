// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import {
  changedFormatFiles,
  MissingPathPolicy,
  NonUtf8Policy,
} from "./format-changed.mjs";

const fixtureInputs = new Set([
  ".github/workflows/CI.yml",
  ".nvmrc",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
  "rstack.config.ts",
  "lefthook.yml",
  "scripts/install-git-hooks.sh",
]);

// Shared runtime and workspace inputs that change every app e2e suite.
const e2eInputs = new Set([
  ".github/workflows/CI.yml",
  ".nvmrc",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
  "turbo.json",
]);

// Paths inside an app package select the app e2e suites.
export function isE2eInput(path) {
  return e2eInputs.has(path) || /^apps\/[^/]+\//.test(path);
}

// Workspace package roots, as directories that hold one level of packages.
// Keep these roots in sync with `pnpm-workspace.yaml`; plan.test.mjs
// cross-checks them.
export const workspaceRoots = ["apps", "packages", "servers"];

// Only the root `turbo.json` and the `turbo.json` placed directly inside a
// workspace package configure the tasks that the Turbo fixtures inspect. Turbo
// ignores a `turbo.json` at any other depth, so it does not select fixtures.
export function isTurboConfigInput(path) {
  return (
    path === "turbo.json" ||
    new RegExp(`^(${workspaceRoots.join("|")})/[^/]+/turbo\\.json$`).test(path)
  );
}

// A package manifest directly inside a workspace package selects the Turbo
// fixtures, which resolve every workspace package.
export function isWorkspaceManifest(path) {
  return new RegExp(
    `^(${workspaceRoots.join("|")})/[^/]+/package\\.json$`,
  ).test(path);
}

// Go sources and module files select the repository Go checks.
export function isGoInput(path) {
  return path.endsWith(".go") || /(^|\/)go\.(mod|sum)$/.test(path);
}

export function plan(cwd, base, head) {
  // Reuse the formatter's revision validation and literal-path selection. The
  // planner runs without installed dependencies and cannot evaluate the Rstack
  // exclusions, so a non-UTF-8 formattable name and a formattable path missing
  // from the working tree are deferred: they select formatting and the
  // `quality` job's formatter gives the verdict.
  const format =
    changedFormatFiles(
      cwd,
      base,
      head,
      false,
      NonUtf8Policy.Defer,
      MissingPathPolicy.Defer,
    ).length > 0;
  const paths = execFileSync(
    "git",
    ["diff", "--name-only", "--no-renames", "-z", base, head, "--"],
    // Match the formatter's limit so large comparisons do not hit ENOBUFS.
    { cwd, encoding: "utf8", maxBuffer: 32 * 1024 * 1024 },
  ).split("\0");
  const fixtures = paths.some(
    (path) =>
      fixtureInputs.has(path) ||
      path.startsWith("scripts/ci/") ||
      isTurboConfigInput(path) ||
      isWorkspaceManifest(path),
  );
  const go = paths.some(isGoInput);
  const e2e = paths.some(isE2eInput);
  return { quality: format || fixtures, format, fixtures, go, e2e };
}

// Why the `go` output has its value. Logged in the ci_plan record only.
export const GoReason = Object.freeze({
  Diff: "diff",
  MainPush: "main-push",
});

// Runner-provided event context that identifies a push to the main branch.
export const MAIN_PUSH_EVENT = "push";
export const MAIN_REF = "refs/heads/main";

export function isMainPush(env) {
  return (
    env.GITHUB_EVENT_NAME === MAIN_PUSH_EVENT && env.GITHUB_REF === MAIN_REF
  );
}

export function main(env = process.env, cwd = process.cwd()) {
  const diffPlan = plan(cwd, env.CI_BASE, env.CI_HEAD);
  // Branch-wide cancellation can cancel a main-push run before its Go job
  // finishes, and the next push then compares only against its own parent.
  // Every main push therefore runs the Go checks against its own head so a
  // later documentation-only push cannot hide a cancelled predecessor's Go
  // range. Pull request and manual comparisons keep affected selection.
  const mainPush = isMainPush(env);
  const result = { ...diffPlan, go: diffPlan.go || mainPush };
  const goReason = mainPush && !diffPlan.go ? GoReason.MainPush : GoReason.Diff;
  const record = {
    event: "ci_plan",
    validationRevision: execFileSync("git", ["rev-parse", "HEAD"], {
      cwd,
      encoding: "utf8",
    }).trim(),
    base: env.CI_BASE,
    head: env.CI_HEAD,
    ...result,
    goReason,
  };
  console.log(JSON.stringify(record));
  appendFileSync(
    env.GITHUB_OUTPUT,
    Object.entries(result)
      .map(([key, value]) => `${key}=${value}\n`)
      .join(""),
  );
  appendFileSync(
    env.GITHUB_STEP_SUMMARY,
    `CI plan: ${JSON.stringify(record)}\n`,
  );
}

// import.meta.main needs Node 24.2+; it also holds for symlinked script paths.
// A selection failure (a comparison revision that does not exist, an unset or
// malformed comparison, an unreadable checkout) must reach the log as one
// parseable record, like the formatter's, not as a stack trace. Exit code 2 is
// the formatter's handled-failure code and tells it apart from a crash (1).
// `main()` writes the outputs and the summary only after `plan()` succeeds, so
// a failure still publishes nothing and the `changes` job still fails closed.
if (import.meta.main) {
  try {
    main();
  } catch (error) {
    console.error(
      JSON.stringify({ event: "ci_format_error", message: error.message }),
    );
    process.exitCode = 2;
  }
}
