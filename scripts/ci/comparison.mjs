// SPDX-License-Identifier: Apache-2.0
//
// Resolves the comparison that the `changes` job publishes as its `base` and
// `head` outputs. The planner, the formatter and the protobuf breaking-change
// check all read that comparison, and the planner and the formatter list its
// changed paths with a two-dot `git diff base head`. A two-dot diff from a
// base that is not an ancestor of the head also lists the reverse of every
// change that only the base contains, so every event resolves a base that is
// an ancestor of the head: the planner then selects checks for the changes
// that the head adds and nothing else.
import { execFileSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import { repositoryRoot } from "./format-changed.mjs";
import { isWorkspaceCli } from "./workspace-node.mjs";

// The workflow events that CI.yml runs on.
export const ComparisonEvent = Object.freeze({
  PullRequest: "pull_request",
  Push: "push",
  WorkflowDispatch: "workflow_dispatch",
  Schedule: "schedule",
});

// How the base of a resolved comparison was found. Logged in the
// ci_comparison record only.
export const BaseSource = Object.freeze({
  // The first parent of the pull request test merge commit.
  MergeParent: "merge-parent",
  // The merge base of the requested base and the checkout.
  MergeBase: "merge-base",
  // A local commit of the empty tree, for a push without a prior commit.
  EmptyTree: "empty-tree",
  // The checkout itself, for the scheduled run.
  Checkout: "checkout",
});

// The base branch revision for a manual dispatch without a `base` input.
const MANUAL_DEFAULT_BASE = "origin/main";

// The runner-provided files that receive the outputs and the summary.
const PUBLICATION_VARIABLES = Object.freeze([
  "GITHUB_OUTPUT",
  "GITHUB_STEP_SUMMARY",
]);

// Every Git call gets an empty standard input, so `hash-object --stdin` hashes
// the empty tree and no call can wait for the runner's input.
const git = (cwd, ...args) =>
  execFileSync("git", args, { cwd, encoding: "utf8", input: "" }).trim();

function commitSha(value, name) {
  if (!/^[0-9a-f]{40}$/u.test(value ?? "") || /^0+$/u.test(value)) {
    throw new Error(`${name} must be a nonzero 40-character commit SHA`);
  }
  return value;
}

function mergeBase(cwd, base, head) {
  try {
    return git(cwd, "merge-base", base, head);
  } catch (error) {
    // `git merge-base` exits 1 without output when the revisions have no
    // common ancestor; every other failure is a Git error.
    if (error.status === 1 && !String(error.stderr ?? "").trim()) {
      throw new Error(`${base} and ${head} have no common ancestor`);
    }
    throw error;
  }
}

// A push without a prior commit (the first push of the branch) compares
// against a local commit of the empty tree, so every path of the head is
// changed. The commit is never pushed; the protos job skips its
// breaking-change check because its checkout does not contain the commit.
function emptyTreeCommit(cwd) {
  const tree = git(cwd, "hash-object", "-t", "tree", "--stdin");
  return git(
    cwd,
    "-c",
    "user.name=CI",
    "-c",
    "user.email=ci@example.invalid",
    "commit-tree",
    tree,
    "-m",
    "Empty comparison",
  );
}

// Returns { base, head, requestedBase, baseSource }. `head` is always the
// checkout, the revision that the jobs validate.
export function resolveComparison(env, cwd = process.cwd()) {
  const root = repositoryRoot(cwd);
  const head = git(root, "rev-parse", "--verify", "HEAD^{commit}");
  switch (env.GITHUB_EVENT_NAME) {
    case ComparisonEvent.PullRequest: {
      // A pull request run checks out the GitHub test merge commit of the
      // pull request head into the base branch, with the base branch
      // revision as its first parent and the pull request head as its second.
      // The comparison uses that first parent, not `pull_request.base.sha`
      // from the event payload: GitHub does not update the payload base when
      // the base branch moves, so it can be a revision older than the merge
      // parent, and it is not always an ancestor of the pull request head.
      const prHead = commitSha(env.PR_HEAD, "PR_HEAD");
      const [, ...parents] = git(
        root,
        "rev-list",
        "--parents",
        "-n",
        "1",
        head,
      ).split(" ");
      if (parents.length !== 2 || parents[1] !== prHead) {
        throw new Error(
          `The pull request checkout ${head} is not the test merge commit of the pull request head ${prHead}`,
        );
      }
      return {
        base: parents[0],
        head,
        requestedBase: parents[0],
        baseSource: BaseSource.MergeParent,
      };
    }
    case ComparisonEvent.Push: {
      const before = env.PUSH_BASE ?? "";
      if (/^0+$/u.test(before)) {
        return {
          base: emptyTreeCommit(root),
          head,
          requestedBase: before,
          baseSource: BaseSource.EmptyTree,
        };
      }
      // The prior commit is an ancestor of the head unless the push rewrote
      // history; then the merge base excludes the commits that the push
      // removed.
      const requestedBase = commitSha(before, "PUSH_BASE");
      return {
        base: mergeBase(root, requestedBase, head),
        head,
        requestedBase,
        baseSource: BaseSource.MergeBase,
      };
    }
    case ComparisonEvent.WorkflowDispatch: {
      const input = env.MANUAL_BASE ?? "";
      const requestedBase = git(
        root,
        "rev-parse",
        "--verify",
        "--end-of-options",
        `${input === "" ? MANUAL_DEFAULT_BASE : input}^{commit}`,
      );
      return {
        base: mergeBase(root, requestedBase, head),
        head,
        requestedBase,
        baseSource: BaseSource.MergeBase,
      };
    }
    case ComparisonEvent.Schedule:
      // The scheduled run validates the main head on the macOS and Windows
      // runners only; `scripts/ci/plan.mjs` selects those jobs for it without
      // a diff. An empty comparison keeps every diff-selected job skipped.
      return {
        base: head,
        head,
        requestedBase: head,
        baseSource: BaseSource.Checkout,
      };
    default:
      throw new Error(
        `Unsupported comparison event ${JSON.stringify(env.GITHUB_EVENT_NAME ?? null)}`,
      );
  }
}

export function main(env = process.env, cwd = process.cwd()) {
  // Check the publication targets before resolving so a missing variable
  // fails closed with nothing printed or written.
  for (const name of PUBLICATION_VARIABLES) {
    if (typeof env[name] !== "string" || env[name] === "") {
      throw new Error(
        `${name} is not set; the comparison is published through it`,
      );
    }
  }
  const comparison = resolveComparison(env, cwd);
  appendFileSync(
    env.GITHUB_OUTPUT,
    `base=${comparison.base}\nhead=${comparison.head}\n`,
  );
  appendFileSync(
    env.GITHUB_STEP_SUMMARY,
    `Validation revision: ${comparison.head}\nComparison: ${comparison.base}..${comparison.head}\n`,
  );
  // Logged last so a failed publication never leaves a success record behind.
  console.log(
    JSON.stringify({
      event: "ci_comparison",
      eventName: env.GITHUB_EVENT_NAME,
      ...comparison,
    }),
  );
}

// A resolution failure must reach the log as one parseable record, like the
// planner's. Exit code 2 tells a handled failure apart from a crash (1).
if (isWorkspaceCli(import.meta)) {
  try {
    main();
  } catch (error) {
    console.error(
      JSON.stringify({ event: "ci_comparison_error", message: error.message }),
    );
    process.exitCode = 2;
  }
}
