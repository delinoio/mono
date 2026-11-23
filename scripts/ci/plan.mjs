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
  // `scripts/ci/protos-generate.test.mjs` runs this script against a fake `go`
  // in `test:tooling`. It is also a protobuf input (see `protoInputs`).
  "scripts/protos.sh",
  // `scripts/ci/imported-skills.test.mjs` checks the imported skill copies
  // against this lock file and `scripts/ci/imported-skills.manifest.json`.
  "skills-lock.json",
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

// Workspace packages that the apps depend on through `workspace:*` (the shared
// UI package and the generated TypeScript binding packages). The app e2e
// suites render these sources in a browser, so a change inside one of them
// changes the app behavior without touching an `apps/<app>/` path. Keep this
// list equal to the directories of the workspace dependencies of the apps;
// plan.test.mjs cross-checks it against `apps/*/package.json`, because the
// planner runs without installing dependencies and cannot derive it itself.
// `packages/ownsand-sdk/` is not an app dependency and stays out.
export const e2eWorkspacePackages = [
  "packages/delino-ui/",
  "protos/delino/accounts/",
  "protos/delino/ownsand/",
];

// Paths inside an app package or inside a workspace package that an app
// depends on select the app e2e suites.
export function isE2eInput(path) {
  return (
    e2eInputs.has(path) ||
    /^apps\/[^/]+\//.test(path) ||
    e2eWorkspacePackages.some((prefix) => path.startsWith(prefix))
  );
}

// Workspace package roots, as directories that hold one level of packages.
// `protos/delino` holds the TypeScript binding packages, one per schema
// directory. Keep these roots in sync with `pnpm-workspace.yaml`;
// plan.test.mjs cross-checks them.
export const workspaceRoots = ["apps", "packages", "protos/delino", "servers"];

// Single workspace packages outside the workspace roots: the release-only Go
// command packages under `cmds/`. Their manifests declare no package tasks
// and exist so that Changesets versions them and `deploy.yml` releases them
// (see the deployment workflow contract), so only the manifest, not the Go
// source, is a workspace input. plan.test.mjs checks that they declare no
// scripts and that `pnpm-workspace.yaml` lists exactly these directories.
export const workspacePackageDirs = ["cmds/ownsand", "cmds/ownsand-agent"];

function isWorkspacePackageFile(path, file) {
  return workspacePackageDirs.some((dir) => path === `${dir}/${file}`);
}

// Only the root `turbo.json` and the `turbo.json` placed directly inside a
// workspace package configure the tasks that the Turbo fixtures inspect. Turbo
// ignores a `turbo.json` at any other depth, so it does not select fixtures.
export function isTurboConfigInput(path) {
  return (
    path === "turbo.json" ||
    isWorkspacePackageFile(path, "turbo.json") ||
    new RegExp(`^(${workspaceRoots.join("|")})/[^/]+/turbo\\.json$`).test(path)
  );
}

// A package manifest directly inside a workspace package selects the Turbo
// fixtures, which resolve every workspace package.
export function isWorkspaceManifest(path) {
  return (
    isWorkspacePackageFile(path, "package.json") ||
    new RegExp(`^(${workspaceRoots.join("|")})/[^/]+/package\\.json$`).test(
      path,
    )
  );
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
    isWorkspaceManifest(path) ||
    new RegExp(`^(${workspaceRoots.join("|")})/[^/]+/`).test(path)
  );
}

// GitHub Actions workflows and local actions select the `workflows` job, which
// lints them with actionlint and audits them with zizmor.
export function isWorkflowInput(path) {
  return (
    path.startsWith(".github/workflows/") ||
    path.startsWith(".github/actions/") ||
    // The actionlint configuration changes which diagnostics fail the lint.
    path === ".github/actionlint.yaml"
  );
}

// Protobuf schema inputs: the schemas, the buf configuration and the
// generation script. The protos job, which the `go` output selects, lints
// them, checks breaking changes and fails when the committed Go or TypeScript
// bindings differ from the generated output.
const protoInputs = new Set([
  "protos/buf.yaml",
  "protos/buf.gen.yaml",
  "protos/buf.lock",
  "scripts/protos.sh",
]);

// The TypeScript binding packages in `protos/delino/<name>/`: the committed
// `*_pb.ts` bindings that the drift check compares, and the package manifest,
// TypeScript configuration and generation template that pin the plugin and
// define how the OwnSand SDK end-to-end test resolves the bindings.
const protoTypeScriptPackageInput =
  /^protos\/delino\/[^/]+\/(?:.+_pb\.ts|package\.json|buf\.gen\.yaml|tsconfig(?:\.[^/]+)?\.json)$/;

// The OwnSand SDK package. The OwnSand end-to-end suite
// (servers/ownsand-api/e2e) runs its `src/e2e.test.ts` with rstest against
// the real components, so every path of the package (sources, tests, rstest
// and TypeScript configuration) selects the Go checks.
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

// The development launcher of ownsand-api. Its test
// TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase in
// `servers/ownsand-api/cmd/ownsand-api` runs the launcher against a Go-built
// server, so a change to the launcher alone must select the Go checks. The
// launcher test (`dev.test.mjs`) and the accounts-api launcher run no Go test.
const ownsandApiLauncher = "servers/ownsand-api/scripts/dev.mjs";

// Go sources, module files, protobuf schema inputs and the inputs of the
// OwnSand end-to-end suite select the repository Go checks.
export function isGoInput(path) {
  return (
    path.endsWith(".go") ||
    /(^|\/)go\.(mod|sum)$/.test(path) ||
    path.endsWith(".proto") ||
    protoInputs.has(path) ||
    protoTypeScriptPackageInput.test(path) ||
    goWorkspaceInputs.has(path) ||
    path.startsWith(ownsandSdkPackage) ||
    goMigrations.some((prefix) => path.startsWith(prefix)) ||
    path === relayDigestVectors ||
    path === ownsandApiLauncher ||
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

// A two-dot diff from a base that is not an ancestor of the head also lists the
// reverse of every change that only the base contains, so the selectors would
// run checks for changes that the head never made. `scripts/ci/comparison.mjs`
// resolves an ancestor base for every event; this check fails closed if a
// comparison from anywhere else breaks that rule. The only accepted base that
// is not an ancestor is a parentless commit of the empty tree: the local
// comparison commit of a push without a prior commit, whose diff lists every
// path of the head and nothing else.
function assertAncestorComparison(root, base, head) {
  try {
    execFileSync("git", ["merge-base", "--is-ancestor", base, head], {
      cwd: root,
      stdio: "ignore",
    });
    return;
  } catch (error) {
    if (error.status !== 1) throw error;
  }
  const git = (...args) =>
    execFileSync("git", args, {
      cwd: root,
      encoding: "utf8",
      input: "",
    }).trim();
  const [, ...parents] = git("rev-list", "--parents", "-n", "1", base).split(
    " ",
  );
  const emptyTree = git("hash-object", "-t", "tree", "--stdin");
  if (parents.length === 0 && git("rev-parse", `${base}^{tree}`) === emptyTree)
    return;
  throw new Error(
    `Comparison base ${base} is not an ancestor of head ${head}; ` +
      "a two-dot diff would also select the reverse of the base's own changes",
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
  // The formatter selection above validated both revisions.
  assertAncestorComparison(repositoryRoot(cwd), base, head);
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
      // The imported skill copies are verified against their lock entries by
      // `scripts/ci/imported-skills.test.mjs`, so a change to a skill file
      // must run it.
      path.startsWith(".agents/skills/") ||
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
  Schedule: "schedule",
});

// Runner-provided event context that identifies a push to the main branch.
export const MAIN_PUSH_EVENT = "push";
export const MAIN_REF = "refs/heads/main";

export function isMainPush(env) {
  return (
    env.GITHUB_EVENT_NAME === MAIN_PUSH_EVENT && env.GITHUB_REF === MAIN_REF
  );
}

// Runner-provided event context that identifies the scheduled run. GitHub
// starts scheduled workflows only on the default branch.
export const SCHEDULE_EVENT = "schedule";

export function isScheduledMain(env) {
  return (
    env.GITHUB_EVENT_NAME === SCHEDULE_EVENT && env.GITHUB_REF === MAIN_REF
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
  // compares only against its own parent. Every main push therefore runs the
  // Linux Go checks, the Linux Rust checks, the fixtures, the e2e suites, the workspace package
  // tasks and the workflow lint against its own head so a later documentation-only push cannot hide a
  // cancelled predecessor's range. The fixtures step lives in
  // the `quality` job, so forcing fixtures forces quality too. `format` stays
  // diff-derived: the formatter compares the same range and a forced run
  // would not widen it. Pull request and manual comparisons keep affected
  // selection.
  const mainPush = isMainPush(env);
  const fixtures = diffPlan.fixtures || mainPush;
  // The macOS and Windows jobs (`go-platforms` and `rust-platforms`) are the
  // most expensive CI jobs, and a main push repeats the platform validation
  // that its pull request already passed. They therefore follow their own
  // outputs: a pull request or manual comparison selects them with `go` and
  // `rust`, a main push never selects them, and the scheduled run on main
  // always selects both. The scheduled run validates each platform against
  // the main head, so a platform break that two merged pull requests make
  // together surfaces within a day, and it saves the main-scoped platform
  // caches that pull requests restore.
  const scheduled = isScheduledMain(env);
  const result = {
    ...diffPlan,
    fixtures,
    quality: diffPlan.quality || fixtures,
    go: diffPlan.go || mainPush,
    rust: diffPlan.rust || mainPush,
    e2e: diffPlan.e2e || mainPush,
    workspace: diffPlan.workspace || mainPush,
    workflows: diffPlan.workflows || mainPush,
    goPlatforms: scheduled || (diffPlan.go && !mainPush),
    rustPlatforms: scheduled || (diffPlan.rust && !mainPush),
  };
  const reasonOf = (selectedByDiff) =>
    mainPush && !selectedByDiff ? OutputReason.MainPush : OutputReason.Diff;
  // `main-push` on a platform output means that the main push deselected it;
  // `schedule` means that the scheduled run selected it.
  const platformReasonOf = (selectedByDiff) => {
    if (scheduled) return OutputReason.Schedule;
    if (mainPush && selectedByDiff) return OutputReason.MainPush;
    return OutputReason.Diff;
  };
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
    goPlatformsReason: platformReasonOf(diffPlan.go),
    rustPlatformsReason: platformReasonOf(diffPlan.rust),
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
