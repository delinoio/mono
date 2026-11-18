// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the shell steps of `.github/workflows/deploy.yml`:
// `check-pending-changesets.sh` (against a stubbed `gh`) and
// `deploy-release.sh` (against a local bare remote).
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const guard = join(root, "scripts/ci/check-pending-changesets.sh");
const release = join(root, "scripts/ci/deploy-release.sh");
const baseEnv = isolateGitEnvironment({ ...process.env });
const provider = join(root, "scripts/ci/deploy-provider.sh");
// The scripts need Bash 4.4 or newer (`mapfile`, empty arrays under `set -u`);
// macOS ships Bash 3.2.
const bashVersion = spawnSync(
  "bash",
  ["-c", 'echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"'],
  {
    encoding: "utf8",
  },
)
  .stdout?.trim()
  .split(" ")
  .map(Number) ?? [0, 0];
const skip =
  process.platform === "win32"
    ? "the deploy scripts run on Linux"
    : bashVersion[0] < 4 || (bashVersion[0] === 4 && bashVersion[1] < 4)
      ? `the deploy scripts need Bash 4.4 or newer, found ${bashVersion.join(".")}`
      : false;

function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), "deploy-scripts-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// A `gh` stub that answers `gh api <path> [--jq <filter>]` from a JSON map of
// path prefix to a list of responses. Each response is used once, and the
// last one repeats. A response is {body} for success or {error} for failure.
function stubGh(dir, responses) {
  const bin = join(dir, "bin");
  mkdirSync(bin);
  // The stub uses require(); keep it CommonJS even when the scratch directory
  // sits inside a package with "type": "module".
  writeFileSync(join(bin, "package.json"), '{"type":"commonjs"}\n');
  writeFileSync(join(dir, "responses.json"), JSON.stringify(responses));
  writeFileSync(join(dir, "calls.json"), "{}");
  const stub = join(bin, "gh");
  writeFileSync(
    stub,
    `#!/usr/bin/env node
const fs = require("node:fs");
const { execFileSync } = require("node:child_process");
const dir = ${JSON.stringify(dir)};
const args = process.argv.slice(2);
const path = args[1];
if (process.env.GH_STUB_ARGS_LOG) fs.appendFileSync(process.env.GH_STUB_ARGS_LOG, path + "\\n");
const jq = args.includes("--jq") ? args[args.indexOf("--jq") + 1] : ".";
const responses = JSON.parse(fs.readFileSync(dir + "/responses.json", "utf8"));
const calls = JSON.parse(fs.readFileSync(dir + "/calls.json", "utf8"));
const key = Object.keys(responses).find((prefix) => path.startsWith(prefix));
if (!key) { process.stderr.write("gh: Not Found (HTTP 404)\\n"); process.exit(1); }
const count = calls[key] ?? 0;
calls[key] = count + 1;
fs.writeFileSync(dir + "/calls.json", JSON.stringify(calls));
const list = responses[key];
const response = list[Math.min(count, list.length - 1)];
if (response.error) { process.stderr.write(response.error + "\\n"); process.exit(1); }
process.stdout.write(execFileSync("jq", ["-r", jq], { input: JSON.stringify(response.body) }));
`,
  );
  chmodSync(stub, 0o755);
  return bin;
}

function runGuard(t, responses, blobs, runNumber = "7") {
  const dir = scratch(t);
  const bin = stubGh(dir, responses);
  const result = spawnSync("bash", [guard], {
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      CHANGESET_BLOBS: JSON.stringify(blobs),
      REPOSITORY: "owner/repo",
      WORKFLOW: "deploy.yml",
      GITHUB_RUN_NUMBER: runNumber,
      GH_TOKEN: "token",
      CHECK_RETRY_DELAY: "0",
      GH_STUB_ARGS_LOG: join(dir, "args.log"),
    },
  });
  const calls = JSON.parse(readFileSync(join(dir, "calls.json"), "utf8"));
  const args = readFileSync(join(dir, "args.log"), {
    encoding: "utf8",
    flag: "a+",
  });
  return { ...result, calls, args };
}

const runPage = (entries) => ({
  body: {
    workflow_runs: entries.map(
      ([run_number, status, conclusion = null, run_attempt = 1]) => ({
        id: 1000 + run_number,
        run_number,
        status,
        conclusion,
        run_attempt,
      }),
    ),
  },
});
// Each argument is one page of runs, newest first. An empty page follows.
const runPages = (...pages) => ({
  "repos/owner/repo/actions/workflows/deploy.yml/runs": [
    ...pages.map(runPage),
    runPage([]),
  ],
});
const runs = (...entries) => runPages(entries);
const jobs = (runNumber, ...startedAt) => ({
  [`repos/owner/repo/actions/runs/${1000 + runNumber}/jobs`]: [
    { body: { jobs: startedAt.map((started_at) => ({ started_at })) } },
  ],
});
const changeset = (id, ...list) => ({
  [`repos/owner/repo/contents/.changeset/${id}.md`]: list,
});

test("guard passes a current plan", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"], [6, "completed"]),
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /The plan is current/);
});

test("guard ignores a newer run that waits in the queue", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([8, "pending"], [7, "in_progress"]),
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test("guard stops a rerun after a newer run started", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([9, "completed"], [8, "pending"], [7, "completed"]),
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /plan is stale: run #9 .* started after this run #7/,
  );
});

test("guard ignores a newer run cancelled in the queue", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs(
        [9, "pending"],
        [8, "completed", "cancelled"],
        [7, "in_progress"],
      ),
      ...jobs(8),
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /Ignoring run #8/);
});

test(
  "guard stops a rerun after a newer run was cancelled mid-deploy",
  { skip },
  (t) => {
    const result = runGuard(
      t,
      {
        ...runs([8, "completed", "cancelled"], [7, "completed", "failure"]),
        ...jobs(8, "2026-11-18T00:00:00Z", null),
        ...changeset("a", { body: { sha: "blob-a" } }),
      },
      { a: "blob-a" },
    );
    assert.equal(result.status, 1);
    assert.match(
      result.stdout,
      /plan is stale: run #8 .* started after this run #7/,
    );
    // The jobs of every attempt count, not only the latest attempt.
    assert.match(result.args, /actions\/runs\/1008\/jobs\?filter=all&/);
  },
);

test("guard reads further pages until it reaches this run", { skip }, (t) => {
  const cancelled = Array.from({ length: 100 }, (_, i) => [
    110 - i,
    "completed",
    "cancelled",
  ]);
  const jobsOfCancelled = Object.assign({}, ...cancelled.map(([n]) => jobs(n)));
  const result = runGuard(
    t,
    {
      ...runPages(cancelled, [
        [8, "completed", "failure"],
        [7, "in_progress"],
      ]),
      ...jobsOfCancelled,
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(
    result.stdout,
    /plan is stale: run #8 .* started after this run #7/,
  );
});

test("guard lists only runs on main", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset("a", { body: { sha: "blob-a" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.args, /runs\?branch=main&/);
});

test(
  "guard stops a rerun while a newer deployed run waits to rerun",
  { skip },
  (t) => {
    const result = runGuard(
      t,
      {
        ...runs([8, "queued", null, 2], [7, "in_progress"]),
        ...jobs(8, "2026-11-18T00:00:00Z"),
        ...changeset("a", { body: { sha: "blob-a" } }),
      },
      { a: "blob-a" },
    );
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(
      result.stdout,
      /plan is stale: run #8 .* started after this run #7/,
    );
  },
);

test(
  "guard ignores a queued rerun in which no job ever started",
  { skip },
  (t) => {
    const result = runGuard(
      t,
      {
        ...runs([8, "pending", null, 2], [7, "in_progress"]),
        ...jobs(8),
        ...changeset("a", { body: { sha: "blob-a" } }),
      },
      { a: "blob-a" },
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /Ignoring run #8/);
  },
);

test("guard reports a released changeset as stale", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset("a", { error: "gh: Not Found (HTTP 404)" }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /plan is stale: .changeset\/a.md is no longer on main/,
  );
  assert.equal(result.calls["repos/owner/repo/contents/.changeset/a.md"], 1);
});

test("guard reports an edited changeset as stale", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset("a", { body: { sha: "blob-new" } }),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /plan is stale: .changeset\/a.md changed on main/,
  );
});

test(
  "guard reports other API errors as errors after retries",
  { skip },
  (t) => {
    const result = runGuard(
      t,
      {
        ...runs([7, "in_progress"]),
        ...changeset("a", { error: "gh: Bad credentials (HTTP 401)" }),
      },
      { a: "blob-a" },
    );
    assert.equal(result.status, 1);
    assert.doesNotMatch(result.stdout, /plan is stale/);
    assert.match(
      result.stdout,
      /Could not read .changeset\/a.md on main: gh: Bad credentials \(HTTP 401\)/,
    );
    assert.equal(result.calls["repos/owner/repo/contents/.changeset/a.md"], 3);
  },
);

test("guard retries a transient API error", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset(
        "a",
        { error: "gh: Server Error (HTTP 502)" },
        { body: { sha: "blob-a" } },
      ),
    },
    { a: "blob-a" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(result.calls["repos/owner/repo/contents/.changeset/a.md"], 2);
});

// --- deploy-release.sh -----------------------------------------------------

function git(cwd, ...args) {
  return execFileSync("git", args, {
    cwd,
    env: baseEnv,
    encoding: "utf8",
  }).trim();
}

function commitAll(cwd, message) {
  git(cwd, "add", "-A");
  git(
    cwd,
    "-c",
    "user.name=t",
    "-c",
    "user.email=t@example.invalid",
    "commit",
    "--quiet",
    "--no-verify",
    "-m",
    message,
  );
}

const changesetConfig = readFileSync(
  join(root, ".changeset/config.json"),
  "utf8",
);

// A workspace with packages `a`, `b` and `delidev` (ignored by the
// repository Changesets config), the repository Changesets config, a
// pushed `main` and the repository `node_modules` for the Changesets CLI.
function releaseFixture(t) {
  const dir = scratch(t);
  const remote = join(dir, "remote.git");
  const work = join(dir, "work");
  git(dir, "init", "--quiet", "--bare", "--initial-branch=main", remote);
  mkdirSync(work);
  git(work, "init", "--quiet", "--initial-branch=main");
  git(work, "remote", "add", "origin", remote);
  writeFileSync(
    join(work, "package.json"),
    JSON.stringify({ name: "fixture", private: true }),
  );
  writeFileSync(join(work, "pnpm-workspace.yaml"), "packages:\n  - pkgs/*\n");
  writeFileSync(join(work, ".gitignore"), "node_modules\n");
  for (const name of ["a", "b", "delidev"]) {
    mkdirSync(join(work, "pkgs", name), { recursive: true });
    writeFileSync(
      join(work, "pkgs", name, "package.json"),
      `${JSON.stringify({ name, version: "0.0.0", private: true }, null, 2)}\n`,
    );
  }
  mkdirSync(join(work, ".changeset"));
  writeFileSync(join(work, ".changeset/config.json"), changesetConfig);
  symlinkSync(join(root, "node_modules"), join(work, "node_modules"), "dir");
  execFileSync(
    "pnpm",
    ["install", "--lockfile-only", "--no-frozen-lockfile", "--ignore-scripts"],
    {
      cwd: work,
      env: baseEnv,
      stdio: "ignore",
    },
  );
  commitAll(work, "base");
  git(work, "push", "--quiet", "origin", "HEAD:refs/heads/main");
  return { dir, remote, work };
}

function addChangeset(work, id, releases) {
  const header = Object.entries(releases)
    .map(([name, type]) => `"${name}": ${type}`)
    .join("\n");
  writeFileSync(
    join(work, ".changeset", `${id}.md`),
    `---\n${header}\n---\n\nChange ${id}.\n`,
  );
  commitAll(work, `changeset ${id}`);
  git(work, "push", "--quiet", "origin", "HEAD:refs/heads/main");
  return git(work, "rev-parse", "HEAD");
}

// Pushes a commit to `main` from a second clone, as if a PR merged during the
// deployment.
function moveMain(dir, remote, file, content) {
  const other = join(dir, `other-${Math.random().toString(36).slice(2)}`);
  git(dir, "clone", "--quiet", "--branch", "main", remote, other);
  writeFileSync(join(other, file), content);
  commitAll(other, `move main: ${file}`);
  git(other, "push", "--quiet", "origin", "HEAD:refs/heads/main");
}

// Blob SHAs of the changesets at `sha`, as the plan job records them.
function changesetBlobs(work, sha) {
  const blobs = {};
  for (const line of git(work, "ls-tree", sha, ".changeset/").split("\n")) {
    const match = /^\S+ blob (\S+)\t\.changeset\/(.+)\.md$/.exec(line);
    if (match && match[2] !== "README") blobs[match[2]] = match[1];
  }
  return blobs;
}

function runRelease(work, sha, releases, extraEnv = {}) {
  const summary = join(work, "..", "summary.md");
  writeFileSync(summary, "");
  const blobs =
    /^[0-9a-f]{40}$/.test(sha) && sha !== "0".repeat(40)
      ? changesetBlobs(work, sha)
      : {};
  const result = spawnSync("bash", [release], {
    cwd: work,
    encoding: "utf8",
    env: {
      ...baseEnv,
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@example.invalid",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@example.invalid",
      RELEASES: JSON.stringify(releases),
      CHANGESET_BLOBS: JSON.stringify(blobs),
      SHA: sha,
      GITHUB_STEP_SUMMARY: summary,
      ...extraEnv,
    },
  });
  return { ...result, summary: readFileSync(summary, "utf8") };
}

const remoteGit = (remote, ...args) => git(remote, ...args);

test("release pushes the version commit and tags atomically", { skip }, (t) => {
  const { remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "patch" });
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(remoteGit(remote, "rev-parse", "a@0.0.1^{commit}^"), sha);
  assert.equal(
    remoteGit(remote, "rev-parse", "a@0.0.1^{commit}"),
    remoteGit(remote, "rev-parse", "main"),
  );
  assert.match(
    remoteGit(remote, "show", "main:pkgs/a/package.json"),
    /"version": "0.0.1"/,
  );
  assert.equal(
    remoteGit(remote, "ls-tree", "--name-only", "main", ".changeset/one.md"),
    "",
  );
  assert.match(result.summary, /`a@0.0.1`/);
});

test("release consumes none-only changesets without tags", { skip }, (t) => {
  const { remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "none", { a: "none" });
  const result = runRelease(work, sha, []);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(remoteGit(remote, "tag"), "");
  assert.equal(
    remoteGit(remote, "ls-tree", "--name-only", "main", ".changeset/none.md"),
    "",
  );
  assert.match(result.summary, /No tags/);
});

test(
  "release skips the commit when only ignored packages changed",
  { skip },
  (t) => {
    const { remote, work } = releaseFixture(t);
    const sha = addChangeset(work, "native", { delidev: "patch" });
    const result = runRelease(work, sha, []);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /changed nothing/);
    assert.equal(remoteGit(remote, "rev-parse", "main"), sha);
  },
);

test(
  "release keeps tags on the deployed tree when main moved",
  { skip },
  (t) => {
    const { dir, remote, work } = releaseFixture(t);
    const sha = addChangeset(work, "one", { a: "patch" });
    moveMain(dir, remote, "pkgs/b/later.txt", "not deployed\n");
    const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /main moved during the deployment/);
    // The tag holds the deployed tree; main has the later commit and the bump.
    assert.equal(remoteGit(remote, "rev-parse", "a@0.0.1^{commit}^"), sha);
    assert.throws(() =>
      remoteGit(remote, "cat-file", "-e", "a@0.0.1:pkgs/b/later.txt"),
    );
    assert.equal(
      remoteGit(remote, "show", "main:pkgs/b/later.txt"),
      "not deployed",
    );
    assert.match(
      remoteGit(remote, "show", "main:pkgs/a/package.json"),
      /"version": "0.0.1"/,
    );
  },
);

test(
  "release keeps changesets merged during the deployment pending",
  { skip },
  (t) => {
    const { dir, remote, work } = releaseFixture(t);
    const sha = addChangeset(work, "one", { a: "patch" });
    moveMain(
      dir,
      remote,
      ".changeset/later.md",
      '---\n"b": patch\n---\n\nLater.\n',
    );
    const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(
      remoteGit(
        remote,
        "ls-tree",
        "--name-only",
        "main",
        ".changeset/later.md",
      ),
      ".changeset/later.md",
    );
    assert.match(
      remoteGit(remote, "show", "main:pkgs/b/package.json"),
      /"version": "0.0.0"/,
    );
  },
);

test("release fails at once when a tag already exists", { skip }, (t) => {
  const { remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "none" });
  git(work, "tag", "a@0.0.0");
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.0" }]);
  assert.equal(result.status, 1);
  assert.match(result.stdout, /Tag a@0.0.0 already exists/);
  assert.equal(remoteGit(remote, "rev-parse", "main"), sha);
});

test("release stops when main withdrew a planned changeset", { skip }, (t) => {
  const { dir, remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "patch" });
  // Another clone deletes the planned changeset while the run deploys.
  const other = join(dir, "withdraw");
  git(dir, "clone", "--quiet", "--branch", "main", remote, other);
  git(other, "rm", "--quiet", ".changeset/one.md");
  commitAll(other, "withdraw changeset");
  git(other, "push", "--quiet", "origin", "HEAD:refs/heads/main");
  const withdrawn = remoteGit(remote, "rev-parse", "main");
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /\.changeset\/one\.md changed or was removed on main/,
  );
  assert.equal(remoteGit(remote, "rev-parse", "main"), withdrawn);
  assert.equal(remoteGit(remote, "tag"), "");
});

test("release stops when main keeps moving", { skip }, (t) => {
  const { dir, remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "patch" });
  moveMain(dir, remote, "moved.txt", "moved\n");
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }], {
    DEPLOY_RELEASE_MAX_ATTEMPTS: "1",
  });
  assert.equal(result.status, 1);
  assert.match(result.stdout, /main kept moving/);
  assert.equal(remoteGit(remote, "tag"), "");
});

test(
  "release refuses a checkout that is not the deployed revision",
  { skip },
  (t) => {
    const { work } = releaseFixture(t);
    addChangeset(work, "one", { a: "patch" });
    const result = runRelease(work, "0".repeat(40), []);
    assert.equal(result.status, 1);
    assert.match(result.stdout, /HEAD is not the deployed revision/);
  },
);

// --- deploy-provider.sh ----------------------------------------------------

// Stubs `railway` and `curl`. Responses are keyed by the first two `railway`
// arguments (`deployment list`, `logs <id>`) or by `curl`. Each response is
// used once and the last one repeats; {exit} makes the call fail.
function stubProvider(dir, responses) {
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  writeFileSync(join(bin, "package.json"), '{"type":"commonjs"}\n');
  writeFileSync(join(dir, "responses.json"), JSON.stringify(responses));
  writeFileSync(join(dir, "calls.log"), "");
  for (const command of ["railway", "curl"]) {
    const file = join(bin, command);
    writeFileSync(
      file,
      `#!/usr/bin/env node
const fs = require("node:fs");
const dir = ${JSON.stringify(dir)};
const command = ${JSON.stringify(command)};
const args = process.argv.slice(2);
const key = command === "curl" ? "curl" : args.slice(0, 2).join(" ");
fs.appendFileSync(dir + "/calls.log", command + " " + args.join(" ") + "\\n");
const responses = JSON.parse(fs.readFileSync(dir + "/responses.json", "utf8"));
const state = dir + "/count-" + key.replace(/\\W/g, "_");
const count = fs.existsSync(state) ? Number(fs.readFileSync(state, "utf8")) : 0;
fs.writeFileSync(state, String(count + 1));
const list = responses[key] ?? [{ stdout: "" }];
const response = list[Math.min(count, list.length - 1)];
if (response.sleep) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, response.sleep * 1000);
}
if (response.exit) {
  process.stderr.write("stub failure\\n");
  process.exit(response.exit);
}
process.stdout.write(
  typeof response.stdout === "string" ? response.stdout : JSON.stringify(response.stdout),
);
`,
    );
    chmodSync(file, 0o755);
  }
  return bin;
}

// The wait commands need GNU coreutils `timeout` (or `gtimeout`).
const hasTimeout = ["timeout", "gtimeout"].some(
  (command) => spawnSync("sh", ["-c", `command -v ${command}`]).status === 0,
);
const skipWait =
  skip || (!hasTimeout && "GNU coreutils timeout is not installed");

function runProvider(t, args, { responses = {}, input, env = {} } = {}) {
  const dir = scratch(t);
  const bin = stubProvider(dir, responses);
  const result = spawnSync("bash", [provider, ...args], {
    encoding: "utf8",
    input,
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      CLOUDFLARE_API_TOKEN: "token",
      CLOUDFLARE_ACCOUNT_ID: "account",
      DEPLOY_POLL_INTERVAL: "0",
      ...env,
    },
  });
  return { ...result, calls: readFileSync(join(dir, "calls.log"), "utf8") };
}

const railwayList = (id, status) => ({
  stdout: [{ id, status, createdAt: "2026-11-18T00:00:00Z" }],
});
const pagesStage = (name, status) => ({
  stdout: { result: { latest_stage: { name, status } } },
});

test(
  "provider reads the Railway deployment id from JSON output",
  { skip },
  (t) => {
    const result = runProvider(t, ["railway-id"], {
      input: 'Indexing...\n{"deploymentId":"dep-1","logsUrl":"https://x"}\n',
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), "dep-1");
    assert.equal(
      runProvider(t, ["railway-id"], { input: "no json\n" }).status,
      1,
    );
  },
);

test(
  "provider reads the Pages deployment id from Wrangler output",
  { skip },
  (t) => {
    const dir = scratch(t);
    const file = join(dir, "wrangler.ndjson");
    writeFileSync(
      file,
      '{"type":"wrangler-session","version":1}\n{"type":"pages-deploy","version":1,"deployment_id":"pages-1"}\n',
    );
    const result = runProvider(t, ["pages-id", file]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), "pages-1");
    writeFileSync(file, '{"type":"wrangler-session","version":1}\n');
    assert.equal(runProvider(t, ["pages-id", file]).status, 1);
  },
);

test("provider waits for Railway SUCCESS", { skip: skipWait }, (t) => {
  const result = runProvider(t, ["wait-railway", "api", "dep-1"], {
    responses: {
      "deployment list": [
        { stdout: [] },
        railwayList("dep-1", "BUILDING"),
        railwayList("dep-1", "DEPLOYING"),
        railwayList("dep-1", "SUCCESS"),
      ],
    },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /SUCCESS/);
  assert.match(
    result.calls,
    /railway deployment list --service api --limit 20 --json/,
  );
});

test(
  "provider fails on a failed Railway deployment and prints its logs",
  { skip: skipWait },
  (t) => {
    for (const status of [
      "FAILED",
      "CRASHED",
      "REMOVED",
      "REMOVING",
      "SKIPPED",
    ]) {
      const result = runProvider(t, ["wait-railway", "api", "dep-1"], {
        responses: { "deployment list": [railwayList("dep-1", status)] },
      });
      assert.equal(result.status, 1, status);
      assert.match(result.stdout, new RegExp(`ended with ${status}`));
      assert.match(
        result.calls,
        /railway logs dep-1 --service api --build --lines 200/,
      );
      assert.match(
        result.calls,
        /railway logs dep-1 --service api --deployment --lines 200/,
      );
    }
  },
);

test(
  "provider tolerates a few failed Railway status reads",
  { skip: skipWait },
  (t) => {
    const ok = runProvider(t, ["wait-railway", "api", "dep-1"], {
      responses: {
        "deployment list": [
          { exit: 1 },
          { exit: 1 },
          railwayList("dep-1", "SUCCESS"),
        ],
      },
    });
    assert.equal(ok.status, 0, ok.stdout + ok.stderr);
    assert.match(ok.stdout, /failure 2 of 5/);
    const failed = runProvider(t, ["wait-railway", "api", "dep-1"], {
      responses: { "deployment list": [{ exit: 1 }] },
    });
    assert.equal(failed.status, 1);
    assert.match(failed.stdout, /5 times in a row/);
  },
);

test(
  "provider gives up on a Railway deployment after the timeout",
  { skip: skipWait },
  (t) => {
    const result = runProvider(t, ["wait-railway", "api", "dep-1"], {
      responses: { "deployment list": [railwayList("dep-1", "BUILDING")] },
      env: { DEPLOY_POLL_TIMEOUT: "0" },
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /did not finish within 0 seconds/);
  },
);

test("provider waits for the Pages deploy stage", { skip: skipWait }, (t) => {
  const result = runProvider(t, ["wait-pages", "site", "pages-1"], {
    responses: {
      curl: [
        pagesStage("build", "active"),
        pagesStage("deploy", "active"),
        pagesStage("deploy", "success"),
      ],
    },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(
    result.calls,
    /curl .*accounts\/account\/pages\/projects\/site\/deployments\/pages-1/,
  );
});

test(
  "provider fails on a failed or canceled Pages deployment",
  { skip: skipWait },
  (t) => {
    for (const [name, status] of [
      ["build", "failure"],
      ["deploy", "failure"],
      ["deploy", "canceled"],
    ]) {
      const result = runProvider(t, ["wait-pages", "site", "pages-1"], {
        responses: { curl: [pagesStage(name, status)] },
      });
      assert.equal(result.status, 1, `${name} ${status}`);
      assert.match(result.stdout, new RegExp(`ended with ${name} ${status}`));
    }
  },
);

test(
  "provider rejects a Pages record without a deployment id",
  { skip },
  (t) => {
    const dir = scratch(t);
    const file = join(dir, "wrangler.ndjson");
    writeFileSync(file, '{"type":"pages-deploy","version":1}\n');
    const result = runProvider(t, ["pages-id", file]);
    assert.equal(result.status, 1);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, /returned no deployment id/);
  },
);

test(
  "provider does not wait for a status read past the deadline",
  { skip: skipWait },
  (t) => {
    const started = Date.now();
    const result = runProvider(t, ["wait-pages", "site", "pages-1"], {
      responses: { curl: [{ sleep: 5, ...pagesStage("deploy", "success") }] },
      env: { DEPLOY_POLL_TIMEOUT: "1" },
    });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stdout, /did not finish within 1 seconds/);
    assert.ok(Date.now() - started < 4000, "the slow read was not cut off");
  },
);
