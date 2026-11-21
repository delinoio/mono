// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import {
  changedFormatFiles,
  MissingPathPolicy,
  NonUtf8Policy,
  repositoryRoot,
} from "./format-changed.mjs";

const fixtureInputs = new Set([
  // The deploy fixtures read the repository Changesets config, and the
  // deploy workflow calls the scripts that they cover.
  ".changeset/config.json",
  ".github/workflows/deploy.yml",
  ".github/workflows/CI.yml",
  ".nvmrc",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
  "rstack.config.ts",
  "lefthook.yml",
  "scripts/install-git-hooks.sh",
  // The Droplet fixtures run this helper against a stubbed `docker`.
  "scripts/droplet/start-caddy.sh",
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

// Root inputs of every workspace package task: the runtime selector, the
// workspace manifests and lockfile, the Turbo task configuration, the root Go
// module that the server packages build, and this workflow.
const workspaceTaskInputs = new Set([
  ".github/workflows/CI.yml",
  ".nvmrc",
  "go.mod",
  "go.sum",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
  "turbo.json",
]);

// Any path inside a workspace package, or a root input of its tasks, selects
// the `workspace-packages` and `workspace-servers` jobs. Together they run the
// build, lint, typecheck and test tasks of every workspace package, because
// `turbo run --affected` selects no package
// when only a root input such as `package.json`, `turbo.json` or `go.mod`
// changes (checked with turbo 2.11.11).
export function isWorkspaceInput(path) {
  return (
    workspaceTaskInputs.has(path) ||
    path === relayDigestVectors ||
    new RegExp(`^(${workspaceRoots.join("|")})/[^/]+/`).test(path)
  );
}

// GitHub Actions workflows and local actions select the `workflows` job, which
// lints them with actionlint and audits them with zizmor.
export function isWorkflowInput(path) {
  return (
    path.startsWith(".github/workflows/") || path.startsWith(".github/actions/")
  );
}

// Protobuf schema inputs: the schemas, the buf configuration, the generation
// script and the TypeScript generation templates and plugin pins of the
// OwnSand SDK and the delino-accounts web app. The protos job, which the `go`
// output selects, lints them, checks breaking changes and fails when the
// committed Go or TypeScript bindings differ from the generated output.
const protoInputs = new Set([
  "protos/buf.yaml",
  "protos/buf.gen.yaml",
  "protos/buf.lock",
  "scripts/protos.sh",
  "packages/ownsand-sdk/buf.gen.yaml",
  "packages/ownsand-sdk/package.json",
  "apps/delino-accounts/buf.gen.yaml",
  "apps/delino-accounts/package.json",
]);

// The committed TypeScript bindings of the delino-accounts web app, which the
// drift check of the protos job compares.
const accountsWebBindings = "apps/delino-accounts/src/gen/";

// The OwnSand SDK package. The drift check compares its committed TypeScript
// bindings, and the OwnSand end-to-end suite (servers/ownsand-api/e2e) runs
// its `src/e2e.test.ts` with rstest against the real components, so every
// path of the package (sources, tests, rstest and TypeScript configuration)
// selects the Go checks.
const ownsandSdkPackage = "packages/ownsand-sdk/";

// The go-test and protos jobs install the workspace dependencies with the
// Node.js runtime of `.nvmrc`, so the workspace manifests and the lockfile
// change what the TypeScript SDK end-to-end test and the protobuf generators
// run with. The workflow defines the jobs' steps, services and caches.
const goWorkspaceInputs = new Set([
  ".github/workflows/CI.yml",
  ".nvmrc",
  "package.json",
  "pnpm-lock.yaml",
  "pnpm-workspace.yaml",
]);

// The accounts-api and ownsand-api schema migrations, embedded into each
// service and applied by the Postgres-backed tests (and the OwnSand end-to-end
// suite) that the go-test job runs.
const goMigrations = [
  "servers/accounts-api/internal/db/migrations/",
  "servers/ownsand-api/internal/db/migrations/",
];

// The relay request digest vectors are read by the Go relaybinding tests and by
// the TypeScript SDK digest test of `packages/ownsand-sdk`, so a change selects
// both the Go checks and the workspace jobs.
const relayDigestVectors =
  "go/ownsand/relaybinding/testdata/request-digest-vectors.json";

// Fixture data under a `testdata` directory of a Go tree is read by Go tests
// (the Go tool ignores such directories as packages, so it holds no `.go`
// package sources that the `.go` selector would catch).
const goTestdata = /^(?:go|cmds|internal|servers)\/(?:.+\/)?testdata\//;

// Go sources, module files, protobuf schema inputs and the inputs of the
// OwnSand end-to-end suite select the repository Go checks.
export function isGoInput(path) {
  return (
    path.endsWith(".go") ||
    /(^|\/)go\.(mod|sum)$/.test(path) ||
    path.endsWith(".proto") ||
    protoInputs.has(path) ||
    goWorkspaceInputs.has(path) ||
    path.startsWith(ownsandSdkPackage) ||
    path.startsWith(accountsWebBindings) ||
    goMigrations.some((prefix) => path.startsWith(prefix)) ||
    path === relayDigestVectors ||
    goTestdata.test(path)
  );
}

// Root Rust toolchain and formatter configuration, and the workflow that
// defines the Rust job's packages, toolchain installation and caches.
const rustInputs = new Set([
  ".github/workflows/CI.yml",
  "rust-toolchain",
  ".rustfmt.toml",
]);

// Rust sources, Cargo manifests and lockfiles, the root toolchain files and
// every path of a Tauri app crate select the Rust checks. A Tauri crate also
// compiles its `tauri.conf.json`, capabilities and icons into the binary.
export function isRustInput(path) {
  return (
    rustInputs.has(path) ||
    path.endsWith(".rs") ||
    /(^|\/)Cargo\.(toml|lock)$/.test(path) ||
    /(^|\/)src-tauri\//.test(path)
  );
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
    // Run from the repository root, like the formatter selectors: with
    // `diff.relative=true`, `git diff --name-only` prints paths relative to
    // (and limited to) its working directory, so a run from a subdirectory
    // would otherwise drop every change outside it and strip the prefix that
    // the selectors below match. Match the formatter's buffer limit so large
    // comparisons do not hit ENOBUFS.
    {
      cwd: repositoryRoot(cwd),
      encoding: "utf8",
      maxBuffer: 32 * 1024 * 1024,
    },
  ).split("\0");
  const fixtures = paths.some(
    (path) =>
      fixtureInputs.has(path) ||
      path.startsWith("scripts/ci/") ||
      // The development scripts (the root `dev` portless preflight and the
      // Infisical launcher) have fixtures in `test:tooling`.
      path.startsWith("scripts/dev/") ||
      // A pending changeset is validated in the quality job, so that an
      // invalid one cannot reach main and stop every deployment.
      path.startsWith(".changeset/") ||
      isTurboConfigInput(path) ||
      isWorkspaceManifest(path),
  );
  const go = paths.some(isGoInput);
  const rust = paths.some(isRustInput);
  const e2e = paths.some(isE2eInput);
  const workspace = paths.some(isWorkspaceInput);
  const workflows = paths.some(isWorkflowInput);
  return {
    quality: format || fixtures,
    format,
    fixtures,
    go,
    rust,
    e2e,
    workspace,
    workflows,
  };
}

// Why a forced output has its value. Logged in the ci_plan record only.
export const OutputReason = Object.freeze({
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

// The planner publishes its selections through these runner-provided files.
const PUBLICATION_VARIABLES = Object.freeze([
  "GITHUB_OUTPUT",
  "GITHUB_STEP_SUMMARY",
]);

export function main(env = process.env, cwd = process.cwd()) {
  // Check the publication targets before any selection or logging so a missing
  // variable fails closed with nothing printed or written.
  for (const name of PUBLICATION_VARIABLES) {
    if (typeof env[name] !== "string" || env[name] === "") {
      throw new Error(
        `${name} is not set; the planner publishes its selections through it`,
      );
    }
  }
  const diffPlan = plan(cwd, env.CI_BASE, env.CI_HEAD);
  // Branch-wide cancellation can cancel a main-push run before its Go, Rust,
  // fixtures, e2e, workspace or workflows jobs finish, and the next push then
  // compares only against its own parent. Every main push therefore runs the Go
  // checks, the Rust checks, the fixtures, the e2e suites, the workspace package
  // tasks and the workflow lint against its own head so a later documentation-only push cannot hide a
  // cancelled predecessor's range. The fixtures step lives in
  // the `quality` job, so forcing fixtures forces quality too. `format` stays
  // diff-derived: the formatter compares the same range and a forced run
  // would not widen it. Pull request and manual comparisons keep affected
  // selection.
  const mainPush = isMainPush(env);
  const fixtures = diffPlan.fixtures || mainPush;
  const result = {
    ...diffPlan,
    fixtures,
    quality: diffPlan.quality || fixtures,
    go: diffPlan.go || mainPush,
    rust: diffPlan.rust || mainPush,
    e2e: diffPlan.e2e || mainPush,
    workspace: diffPlan.workspace || mainPush,
    workflows: diffPlan.workflows || mainPush,
  };
  const reasonOf = (selectedByDiff) =>
    mainPush && !selectedByDiff ? OutputReason.MainPush : OutputReason.Diff;
  const record = {
    event: "ci_plan",
    validationRevision: execFileSync("git", ["rev-parse", "HEAD"], {
      cwd,
      encoding: "utf8",
    }).trim(),
    base: env.CI_BASE,
    head: env.CI_HEAD,
    ...result,
    goReason: reasonOf(diffPlan.go),
    rustReason: reasonOf(diffPlan.rust),
    fixturesReason: reasonOf(diffPlan.fixtures),
    e2eReason: reasonOf(diffPlan.e2e),
    workspaceReason: reasonOf(diffPlan.workspace),
    workflowsReason: reasonOf(diffPlan.workflows),
  };
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
  // Logged last so a failed publication never leaves a success record behind.
  console.log(JSON.stringify(record));
}

// import.meta.main needs Node 24.2+; it also holds for symlinked script paths.
// A selection failure (a comparison revision that does not exist, an unset or
// malformed comparison, an unreadable checkout) must reach the log as one
// parseable record, like the formatter's, not as a stack trace. Exit code 2 is
// the formatter's handled-failure code and tells it apart from a crash (1).
// `main()` checks both publication variables first, then writes the outputs and
// the summary only after `plan()` succeeds, and logs the ci_plan record last, so
// a selection or configuration failure publishes and prints nothing and the
// `changes` job still fails closed. Residual limit: a real I/O failure on the
// second write (the summary) can still leave the outputs file written; atomic
// publishing is out of scope.
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
