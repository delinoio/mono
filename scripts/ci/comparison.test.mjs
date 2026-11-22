// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  BaseSource,
  ComparisonEvent,
  main,
  resolveComparison,
} from "./comparison.mjs";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";
import { plan } from "./plan.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const script = join(root, "scripts/ci/comparison.mjs");

// The resolver and the planner run git in this process; a developer's Git
// configuration and the GIT_* variables of an enclosing hook must not reach
// them.
isolateGitEnvironment(process.env);

// Environment for a spawned resolver CLI without inherited GITHUB_* variables.
function cliEnvironment(extra = {}) {
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

// A repository with one commit on `main` and helpers to grow its history.
function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), "ci-comparison-"));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (...args) =>
    execFileSync("git", args, {
      cwd,
      encoding: "utf8",
      env: isolateGitEnvironment({ ...process.env }),
    }).trim();
  git("init", "--quiet", "--initial-branch=main");
  git("config", "user.name", "CI fixture");
  git("config", "user.email", "fixture@example.invalid");
  git("config", "core.hooksPath", ".no-hooks");
  const commit = (path, content = `${path}\n`) => {
    mkdirSync(dirname(join(cwd, path)), { recursive: true });
    writeFileSync(join(cwd, path), content);
    git("add", ".");
    git("commit", "--quiet", "-m", `change ${path}`);
    return git("rev-parse", "HEAD");
  };
  const fork = commit("LICENSE");
  return { cwd, git, commit, fork };
}

// Builds the state of a documentation-only pull request whose base branch
// gained a Go change after the branch was created, as GitHub checks it out:
// the test merge commit of the pull request head into the newer base branch.
function staleBaseFixture(t) {
  const f = fixture(t);
  f.git("switch", "--quiet", "-c", "feature");
  const prHead = f.commit("docs/guide.md");
  f.git("switch", "--quiet", "main");
  const mainTip = f.commit("cmds/tool/main.go", "package main\n");
  f.git("merge", "--quiet", "--no-ff", "--no-edit", "feature");
  const merge = f.git("rev-parse", "HEAD");
  return { ...f, prHead, mainTip, merge };
}

const noChecks = {
  quality: false,
  format: false,
  fixtures: false,
  go: false,
  rust: false,
  e2e: false,
  workspace: false,
  workflows: false,
};

test("comparison events are the CI.yml triggers", () => {
  assert.deepEqual(Object.values(ComparisonEvent), [
    "pull_request",
    "push",
    "workflow_dispatch",
  ]);
  const workflow = readFileSync(join(root, ".github/workflows/CI.yml"), "utf8");
  const triggers = workflow.slice(
    workflow.indexOf("\non:\n") + 1,
    workflow.indexOf("\npermissions:"),
  );
  for (const event of Object.values(ComparisonEvent)) {
    assert.match(triggers, new RegExp(`^  ${event}:`, "mu"), event);
  }
});

test("a pull request compares the first parent of its test merge commit", (t) => {
  const f = staleBaseFixture(t);
  assert.deepEqual(
    resolveComparison(
      { GITHUB_EVENT_NAME: ComparisonEvent.PullRequest, PR_HEAD: f.prHead },
      f.cwd,
    ),
    {
      base: f.mainTip,
      head: f.merge,
      requestedBase: f.mainTip,
      baseSource: BaseSource.MergeParent,
    },
  );
  // The resolved comparison selects only the pull request's documentation
  // change, not the Go change that only the base branch made.
  assert.deepEqual(plan(f.cwd, f.mainTip, f.merge), {
    ...noChecks,
    quality: true,
    format: true,
  });
});

test("the planner rejects a base branch revision that the pull request head does not contain", (t) => {
  const f = staleBaseFixture(t);
  // A two-dot diff from the base branch tip to the pull request head lists the
  // Go file that only the base branch changed, so it would select Go.
  assert.match(
    f.git("diff", "--name-only", f.mainTip, f.prHead),
    /cmds\/tool\/main\.go/u,
  );
  assert.throws(
    () => plan(f.cwd, f.mainTip, f.prHead),
    /is not an ancestor of head/u,
  );
  // The fork point stays a valid comparison.
  assert.deepEqual(plan(f.cwd, f.fork, f.prHead), {
    ...noChecks,
    quality: true,
    format: true,
  });
});

test("a pull request checkout must be the test merge commit of the event head", (t) => {
  const f = staleBaseFixture(t);
  const env = { GITHUB_EVENT_NAME: ComparisonEvent.PullRequest };
  // The checkout is a merge commit of another head.
  assert.throws(
    () => resolveComparison({ ...env, PR_HEAD: f.mainTip }, f.cwd),
    /is not the test merge commit of the pull request head/u,
  );
  // The checkout is the pull request head itself, not a merge commit.
  f.git("switch", "--quiet", "--detach", f.prHead);
  assert.throws(
    () => resolveComparison({ ...env, PR_HEAD: f.prHead }, f.cwd),
    /is not the test merge commit of the pull request head/u,
  );
  for (const prHead of [undefined, "", "0".repeat(40), "feature", "HEAD"]) {
    assert.throws(
      () => resolveComparison({ ...env, PR_HEAD: prHead }, f.cwd),
      /PR_HEAD must be a nonzero 40-character commit SHA/u,
    );
  }
});

test("a push compares the prior commit", (t) => {
  const f = fixture(t);
  const head = f.commit("docs/guide.md");
  assert.deepEqual(
    resolveComparison(
      { GITHUB_EVENT_NAME: ComparisonEvent.Push, PUSH_BASE: f.fork },
      f.cwd,
    ),
    {
      base: f.fork,
      head,
      requestedBase: f.fork,
      baseSource: BaseSource.MergeBase,
    },
  );
});

test("a push that rewrote history compares the merge base with the prior commit", (t) => {
  const f = fixture(t);
  const before = f.commit("cmds/tool/main.go", "package main\n");
  f.git("reset", "--quiet", "--hard", f.fork);
  const head = f.commit("docs/guide.md");
  const comparison = resolveComparison(
    { GITHUB_EVENT_NAME: ComparisonEvent.Push, PUSH_BASE: before },
    f.cwd,
  );
  assert.deepEqual(comparison, {
    base: f.fork,
    head,
    requestedBase: before,
    baseSource: BaseSource.MergeBase,
  });
  assert.equal(plan(f.cwd, comparison.base, comparison.head).go, false);
});

test("a push without a prior commit compares a local empty-tree commit", (t) => {
  const f = fixture(t);
  const head = f.commit("cmds/tool/main.go", "package main\n");
  const comparison = resolveComparison(
    { GITHUB_EVENT_NAME: ComparisonEvent.Push, PUSH_BASE: "0".repeat(40) },
    f.cwd,
  );
  assert.equal(comparison.head, head);
  assert.equal(comparison.requestedBase, "0".repeat(40));
  assert.equal(comparison.baseSource, BaseSource.EmptyTree);
  assert.equal(
    f.git("rev-list", "--parents", "-n", "1", comparison.base),
    comparison.base,
  );
  assert.equal(f.git("ls-tree", comparison.base), "");
  // The planner accepts the empty-tree commit although it is not an ancestor.
  assert.equal(plan(f.cwd, comparison.base, comparison.head).go, true);
});

test("the planner accepts no other unrelated base", (t) => {
  const f = fixture(t);
  const head = f.commit("docs/guide.md");
  // A parentless commit of a nonempty tree.
  const tree = f.git("rev-parse", `${head}^{tree}`);
  const unrelated = f.git("commit-tree", tree, "-m", "unrelated");
  assert.throws(
    () => plan(f.cwd, unrelated, head),
    /is not an ancestor of head/u,
  );
});

test("a push rejects an invalid or unknown prior commit", (t) => {
  const f = fixture(t);
  f.commit("docs/guide.md");
  const env = { GITHUB_EVENT_NAME: ComparisonEvent.Push };
  for (const before of [undefined, "", "main", "HEAD~1"]) {
    assert.throws(
      () => resolveComparison({ ...env, PUSH_BASE: before }, f.cwd),
      /PUSH_BASE must be a nonzero 40-character commit SHA/u,
    );
  }
  assert.throws(() =>
    resolveComparison({ ...env, PUSH_BASE: "a".repeat(40) }, f.cwd),
  );
});

test("a manual dispatch compares the merge base with origin/main by default", (t) => {
  const f = staleBaseFixture(t);
  f.git("update-ref", "refs/remotes/origin/main", f.mainTip);
  f.git("switch", "--quiet", "--detach", f.prHead);
  assert.deepEqual(
    resolveComparison(
      { GITHUB_EVENT_NAME: ComparisonEvent.WorkflowDispatch, MANUAL_BASE: "" },
      f.cwd,
    ),
    {
      base: f.fork,
      head: f.prHead,
      requestedBase: f.mainTip,
      baseSource: BaseSource.MergeBase,
    },
  );
});

test("a manual base input compares its merge base with the checkout", (t) => {
  const f = staleBaseFixture(t);
  f.git("branch", "base-branch", f.mainTip);
  f.git("switch", "--quiet", "--detach", f.prHead);
  const env = { GITHUB_EVENT_NAME: ComparisonEvent.WorkflowDispatch };
  // A base input that moved past the fork point, given as a branch or a SHA.
  for (const input of ["base-branch", f.mainTip]) {
    assert.deepEqual(
      resolveComparison({ ...env, MANUAL_BASE: input }, f.cwd),
      {
        base: f.fork,
        head: f.prHead,
        requestedBase: f.mainTip,
        baseSource: BaseSource.MergeBase,
      },
      input,
    );
  }
  // An ancestor input is its own merge base.
  assert.equal(
    resolveComparison({ ...env, MANUAL_BASE: f.fork }, f.cwd).base,
    f.fork,
  );
  // Option-like and unknown inputs are rejected, not passed as Git options.
  const marker = join(f.cwd, "option-output");
  for (const input of [
    `--output=${marker}`,
    "missing-branch",
    "a".repeat(40),
  ]) {
    assert.throws(() =>
      resolveComparison({ ...env, MANUAL_BASE: input }, f.cwd),
    );
  }
  assert.equal(existsSync(marker), false);
});

test("a revision without a common ancestor fails", (t) => {
  const f = fixture(t);
  const head = f.commit("docs/guide.md");
  const tree = f.git("rev-parse", `${head}^{tree}`);
  const unrelated = f.git("commit-tree", tree, "-m", "unrelated");
  assert.throws(
    () =>
      resolveComparison(
        { GITHUB_EVENT_NAME: ComparisonEvent.Push, PUSH_BASE: unrelated },
        f.cwd,
      ),
    /have no common ancestor/u,
  );
});

test("an unsupported event fails", (t) => {
  const f = fixture(t);
  for (const event of [undefined, "", "pull_request_target", "schedule"]) {
    assert.throws(
      () => resolveComparison({ GITHUB_EVENT_NAME: event }, f.cwd),
      /Unsupported comparison event/u,
    );
  }
});

test("the resolver gives the same comparison from a subdirectory", (t) => {
  const f = staleBaseFixture(t);
  const env = {
    GITHUB_EVENT_NAME: ComparisonEvent.PullRequest,
    PR_HEAD: f.prHead,
  };
  assert.deepEqual(
    resolveComparison(env, join(f.cwd, "docs")),
    resolveComparison(env, f.cwd),
  );
});

test("main() publishes the outputs and summary and logs one record", (t) => {
  const f = staleBaseFixture(t);
  const output = join(f.cwd, "output");
  const summary = join(f.cwd, "summary");
  const result = spawnSync(process.execPath, [script], {
    cwd: f.cwd,
    encoding: "utf8",
    env: cliEnvironment({
      GITHUB_EVENT_NAME: ComparisonEvent.PullRequest,
      PR_HEAD: f.prHead,
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: summary,
    }),
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(
    readFileSync(output, "utf8"),
    `base=${f.mainTip}\nhead=${f.merge}\n`,
  );
  assert.equal(
    readFileSync(summary, "utf8"),
    `Validation revision: ${f.merge}\nComparison: ${f.mainTip}..${f.merge}\n`,
  );
  assert.deepEqual(JSON.parse(result.stdout), {
    event: "ci_comparison",
    eventName: ComparisonEvent.PullRequest,
    base: f.mainTip,
    head: f.merge,
    requestedBase: f.mainTip,
    baseSource: BaseSource.MergeParent,
  });
  assert.equal(result.stderr, "");
});

test("a failed resolution prints one error record and writes nothing", (t) => {
  const f = staleBaseFixture(t);
  const output = join(f.cwd, "output");
  const summary = join(f.cwd, "summary");
  const result = spawnSync(process.execPath, [script], {
    cwd: f.cwd,
    encoding: "utf8",
    env: cliEnvironment({
      GITHUB_EVENT_NAME: ComparisonEvent.PullRequest,
      PR_HEAD: f.mainTip,
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: summary,
    }),
  });
  assert.equal(result.status, 2);
  assert.equal(result.stdout, "");
  const record = JSON.parse(result.stderr.trim().split("\n").at(-1));
  assert.equal(record.event, "ci_comparison_error");
  assert.match(record.message, /is not the test merge commit/u);
  assert.equal(existsSync(output), false);
  assert.equal(existsSync(summary), false);
});

test("main() fails before resolving when a publication target is unset", (t) => {
  const f = fixture(t);
  f.commit("docs/guide.md");
  for (const missing of ["GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"]) {
    const env = {
      GITHUB_EVENT_NAME: ComparisonEvent.Push,
      PUSH_BASE: f.fork,
      GITHUB_OUTPUT: join(f.cwd, "output"),
      GITHUB_STEP_SUMMARY: join(f.cwd, "summary"),
    };
    delete env[missing];
    assert.throws(
      () => main(env, f.cwd),
      new RegExp(`${missing} is not set`, "u"),
    );
    assert.equal(existsSync(join(f.cwd, "output")), false);
    assert.equal(existsSync(join(f.cwd, "summary")), false);
  }
});

test("CI resolves the comparison with this script and never reads the payload base", () => {
  const workflow = readFileSync(join(root, ".github/workflows/CI.yml"), "utf8");
  const step = workflow.slice(
    workflow.indexOf("      - name: Resolve affected comparison\n"),
    workflow.indexOf("      - name: Select affected validation\n"),
  );
  assert.match(step, /^        id: comparison$/mu);
  assert.match(step, /^        run: node scripts\/ci\/comparison\.mjs$/mu);
  assert.doesNotMatch(workflow, /github\.event\.pull_request\.base\b/u);
});
