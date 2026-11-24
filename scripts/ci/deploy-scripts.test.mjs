// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the shell steps of `.github/workflows/deploy.yml`:
// `check-pending-changesets.sh` (against a stubbed `gh`),
// `deploy-provider.sh` and `deploy-targets.sh` (against stubbed CLIs),
// `droplet-deploy.sh` (against stubbed `docker` and `curl`),
// `start-caddy.sh` (against a stubbed `docker`),
// `configure-docker-logs.sh` (against stubbed `docker` and `systemctl`) and
// `deploy-release.sh` and `release-recovery.sh` (against a local bare remote).
import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  statSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { main as deployPlan } from "./deploy-plan.mjs";
import { spawnProcessGroup } from "./fixture-process.mjs";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const guard = join(root, "scripts/ci/check-pending-changesets.sh");
const release = join(root, "scripts/ci/deploy-release.sh");
const recovery = join(root, "scripts/ci/release-recovery.sh");
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
// The scripts and the stubs call jq, which the Ubuntu runners provide.
const hasJq = spawnSync("sh", ["-c", "command -v jq"]).status === 0;
// droplet-deploy.sh serializes deployments with util-linux `flock`.
const hasFlock = spawnSync("sh", ["-c", "command -v flock"]).status === 0;
function skipReason() {
  if (process.platform === "win32") return "the deploy scripts run on Linux";
  if (!hasJq) return "jq is not installed";
  if (!hasFlock) return "flock is not installed";
  if (bashVersion[0] < 4 || (bashVersion[0] === 4 && bashVersion[1] < 4)) {
    return `the deploy scripts need Bash 4.4 or newer, found ${bashVersion.join(".")}`;
  }
  return false;
}
const skip = skipReason();

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

const plannedSha = "c".repeat(40);
const blobA = "a".repeat(40);
const blobNew = "b".repeat(40);

function runGuard(t, responses, blobs, runNumber = "7") {
  const dir = scratch(t);
  const bin = stubGh(dir, {
    [`repos/owner/repo/compare/${plannedSha}...main`]: [
      { body: { status: "ahead" } },
    ],
    ...responses,
  });
  const result = spawnSync("bash", [guard], {
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      CHANGESET_BLOBS: JSON.stringify(blobs),
      REPOSITORY: "owner/repo",
      WORKFLOW: "deploy.yml",
      GITHUB_RUN_NUMBER: runNumber,
      SHA: plannedSha,
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
// One page of jobs, then an empty page. More pages: pass arrays.
const jobPages = (runNumber, ...pages) => ({
  [`repos/owner/repo/actions/runs/${1000 + runNumber}/jobs`]: [
    ...pages.map((startedAt) => ({
      body: { jobs: startedAt.map((started_at) => ({ started_at })) },
    })),
    { body: { jobs: [] } },
  ],
});
const jobs = (runNumber, ...startedAt) => jobPages(runNumber, startedAt);
const changeset = (id, ...list) => ({
  // The guard lists the `.changeset` directory once; a {body: {sha}}
  // response becomes a listing that contains `<id>.md` with that blob.
  "repos/owner/repo/contents/.changeset": list.map((response) =>
    response.body
      ? { body: [{ name: `${id}.md`, type: "file", sha: response.body.sha }] }
      : response,
  ),
});

test("deploy.yml keeps pending runs in the concurrency queue", () => {
  const workflow = readFileSync(
    join(root, ".github/workflows/deploy.yml"),
    "utf8",
  );
  const block = workflow.match(/^concurrency:\n((?:  .*\n|\n)+)/m);
  assert.ok(block, "deploy.yml has a workflow-level concurrency block");
  const keys = block[1]
    .split("\n")
    .filter((line) => /^  [a-z-]+:/.test(line))
    .map((line) => line.trim());
  assert.ok(keys.includes("queue: max"));
  assert.ok(keys.includes("cancel-in-progress: false"));
  assert.ok(keys.some((key) => key.startsWith("group: ")));
  // actionlint v1.7.12 rejects `queue`; the exception must name only that
  // diagnostic and only this workflow.
  const lint = readFileSync(join(root, ".github/actionlint.yaml"), "utf8");
  assert.match(
    lint,
    /^paths:\n {2}\.github\/workflows\/deploy\.yml:\n {4}ignore:\n {6}- 'unexpected key "queue" for "concurrency" section\.\*'\n$/m,
  );
  assert.equal(lint.match(/^ {2}\.github\/workflows\//gm)?.length, 1);
});

test("guard passes a current plan", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"], [6, "completed"]),
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /The plan is current/);
});

test("guard ignores a newer run that waits in the queue", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([8, "pending"], [7, "in_progress"]),
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test("guard stops a rerun after a newer run started", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([9, "completed"], [8, "pending"], [7, "completed"]),
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
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
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
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
        ...changeset("a", { body: { sha: blobA } }),
      },
      { a: blobA },
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
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
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
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
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
        ...changeset("a", { body: { sha: blobA } }),
      },
      { a: blobA },
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
        ...changeset("a", { body: { sha: blobA } }),
      },
      { a: blobA },
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /Ignoring run #8/);
  },
);

test("guard reports a rewound main as stale", { skip }, (t) => {
  for (const compare of ["behind", "diverged"]) {
    const result = runGuard(
      t,
      {
        [`repos/owner/repo/compare/${plannedSha}...main`]: [
          { body: { status: compare } },
        ],
        ...runs([7, "in_progress"]),
        ...changeset("a", { body: { sha: blobA } }),
      },
      { a: blobA },
    );
    assert.equal(result.status, 1, compare);
    assert.match(result.stdout, /main no longer contains the planned revision/);
  }
});

test("guard reads every jobs page of a cancelled newer run", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([8, "completed", "cancelled"], [7, "in_progress"]),
      ...jobPages(8, Array(100).fill(null), ["2026-11-18T00:00:00Z"]),
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
  );
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /plan is stale: run #8/);
});

test("guard rejects a missing or malformed changeset map", { skip }, (t) => {
  for (const blobs of ["", "null", "{}", '{"a":"short"}']) {
    const dir = scratch(t);
    const bin = stubGh(dir, {
      ...runs([7, "in_progress"]),
      ...changeset("a", { error: "gh: Not Found (HTTP 404)" }),
    });
    const result = spawnSync("bash", [guard], {
      encoding: "utf8",
      env: {
        ...baseEnv,
        PATH: `${bin}${delimiter}${baseEnv.PATH}`,
        CHANGESET_BLOBS: blobs,
        REPOSITORY: "owner/repo",
        WORKFLOW: "deploy.yml",
        GITHUB_RUN_NUMBER: "7",
        SHA: plannedSha,
        GH_TOKEN: "token",
        CHECK_RETRY_DELAY: "0",
      },
    });
    assert.equal(result.status, 1, JSON.stringify(blobs));
    assert.match(result.stdout, /CHANGESET_BLOBS is not a nonempty map/);
  }
});

test("guard annotates an unset or malformed run number", { skip }, (t) => {
  for (const runNumber of [undefined, "", "abc"]) {
    const dir = scratch(t);
    const bin = stubGh(dir, {});
    const env = {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      CHANGESET_BLOBS: JSON.stringify({ a: "a".repeat(40) }),
      REPOSITORY: "owner/repo",
      WORKFLOW: "deploy.yml",
      SHA: plannedSha,
      GH_TOKEN: "token",
      CHECK_RETRY_DELAY: "0",
    };
    // baseEnv spreads process.env, so remove an inherited run number.
    delete env.GITHUB_RUN_NUMBER;
    if (runNumber !== undefined) env.GITHUB_RUN_NUMBER = runNumber;
    const result = spawnSync("bash", [guard], { encoding: "utf8", env });
    const label = `run number ${JSON.stringify(runNumber) ?? "unset"}`;
    assert.equal(result.status, 1, label);
    assert.equal(
      result.stdout,
      `::error::GITHUB_RUN_NUMBER is not a number: ${runNumber ?? ""}\n`,
      label,
    );
    assert.doesNotMatch(result.stderr, /unbound variable/, label);
  }
});

test(
  "guard lists .changeset once for many planned changesets",
  { skip },
  (t) => {
    const ids = Array.from({ length: 150 }, (_, i) => `c${i}`);
    const blobs = Object.fromEntries(ids.map((id) => [id, blobA]));
    const result = runGuard(
      t,
      {
        ...runs([7, "in_progress"]),
        "repos/owner/repo/contents/.changeset": [
          {
            body: ids.map((id) => ({
              name: `${id}.md`,
              type: "file",
              sha: blobA,
            })),
          },
        ],
      },
      blobs,
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(result.calls["repos/owner/repo/contents/.changeset"], 1);
  },
);

test("guard looks up a planned file beyond a full listing", { skip }, (t) => {
  const others = Array.from({ length: 1000 }, (_, i) => ({
    name: `other-${i}.md`,
    type: "file",
    sha: blobNew,
  }));
  for (const [file, expected] of [
    [{ body: { sha: blobA } }, 0],
    [{ error: "gh: Not Found (HTTP 404)" }, 1],
  ]) {
    const result = runGuard(
      t,
      {
        ...runs([7, "in_progress"]),
        // The specific file comes first: the stub matches by prefix.
        "repos/owner/repo/contents/.changeset/a.md": [file],
        "repos/owner/repo/contents/.changeset": [{ body: others }],
      },
      { a: blobA },
    );
    assert.equal(result.status, expected, result.stdout + result.stderr);
    assert.equal(result.calls["repos/owner/repo/contents/.changeset/a.md"], 1);
  }
});

test(
  "guard counts subdirectories when it detects a full listing",
  { skip },
  (t) => {
    const entries = [
      { name: "nested", type: "dir", sha: blobNew },
      ...Array.from({ length: 999 }, (_, i) => ({
        name: `other-${i}.md`,
        type: "file",
        sha: blobNew,
      })),
    ];
    const result = runGuard(
      t,
      {
        ...runs([7, "in_progress"]),
        "repos/owner/repo/contents/.changeset/zzz.md": [
          { body: { sha: blobA } },
        ],
        "repos/owner/repo/contents/.changeset": [{ body: entries }],
      },
      { zzz: blobA },
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
  },
);

test(
  "guard stops an upload after main changed package manifests",
  { skip },
  (t) => {
    for (const [files, message] of [
      [
        [{ filename: "src/a.ts" }, { filename: "apps/web/package.json" }],
        /main changed package manifests since the plan \(apps\/web\/package\.json\)/,
      ],
      [[{ filename: "pnpm-lock.yaml" }], /\(pnpm-lock\.yaml\)/],
      [
        [
          {
            filename: "apps/web/package.disabled",
            previous_filename: "apps/web/package.json",
          },
        ],
        /\(apps\/web\/package\.json\)/,
      ],
      [
        Array.from({ length: 300 }, (_, i) => ({ filename: `src/${i}.ts` })),
        /too many files since the plan/,
      ],
    ]) {
      const result = runGuard(
        t,
        {
          [`repos/owner/repo/compare/${plannedSha}...main`]: [
            { body: { status: "ahead", files } },
          ],
          ...runs([7, "in_progress"]),
          ...changeset("a", { body: { sha: blobA } }),
        },
        { a: blobA },
      );
      assert.equal(result.status, 1, result.stdout + result.stderr);
      assert.match(result.stdout, message);
    }
  },
);

test("guard allows unrelated changes on main", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      [`repos/owner/repo/compare/${plannedSha}...main`]: [
        { body: { status: "ahead", files: [{ filename: "docs/x.md" }] } },
      ],
      ...runs([7, "in_progress"]),
      ...changeset("a", { body: { sha: blobA } }),
    },
    { a: blobA },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
});

test("guard reports a released changeset as stale", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset("a", { error: "gh: Not Found (HTTP 404)" }),
    },
    { a: blobA },
  );
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /plan is stale: .changeset\/a.md is no longer on main/,
  );
  assert.equal(result.calls["repos/owner/repo/contents/.changeset"], 1);
});

test("guard reports an edited changeset as stale", { skip }, (t) => {
  const result = runGuard(
    t,
    {
      ...runs([7, "in_progress"]),
      ...changeset("a", { body: { sha: blobNew } }),
    },
    { a: blobA },
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
      { a: blobA },
    );
    assert.equal(result.status, 1);
    assert.doesNotMatch(result.stdout, /plan is stale/);
    assert.match(
      result.stdout,
      /Could not list .changeset on main: gh: Bad credentials \(HTTP 401\)/,
    );
    assert.equal(result.calls["repos/owner/repo/contents/.changeset"], 3);
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
        { body: { sha: blobA } },
      ),
    },
    { a: blobA },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(result.calls["repos/owner/repo/contents/.changeset"], 2);
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
function releaseFixture(t, { tide = false } = {}) {
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
  const packageRoot = tide ? "packages" : "pkgs";
  const workspace = ["packages:", `  - ${packageRoot}/*`];
  if (tide) {
    workspace.push("  - cmds/tide");
    mkdirSync(join(work, "cmds/tide"), { recursive: true });
    writeFileSync(
      join(work, "cmds/tide/package.json"),
      readFileSync(join(root, "cmds/tide/package.json")),
    );
  }
  writeFileSync(join(work, "pnpm-workspace.yaml"), workspace.join("\n") + "\n");
  writeFileSync(join(work, ".gitignore"), "node_modules\n");
  for (const name of ["a", "b", "delidev"]) {
    mkdirSync(join(work, packageRoot, name), { recursive: true });
    writeFileSync(
      join(work, packageRoot, name, "package.json"),
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

function readOutputs(file) {
  return Object.fromEntries(
    readFileSync(file, "utf8")
      .trim()
      .split("\n")
      .map((line) => [
        line.slice(0, line.indexOf("=")),
        line.slice(line.indexOf("=") + 1),
      ]),
  );
}

// Run the planner on real Changesets status output and execute the workflow's
// blob-recording step with the planner outputs.
function planRelease(work) {
  const status = join(work, "..", "status.json");
  const planOutput = join(work, "..", "plan-output");
  const blobsOutput = join(work, "..", "blobs-output");
  execFileSync("pnpm", ["exec", "changeset", "status", "--output", status], {
    cwd: work,
    env: baseEnv,
  });
  deployPlan({
    argv: [status],
    env: { GITHUB_OUTPUT: planOutput },
    repoRoot: work,
  });
  const outputs = readOutputs(planOutput);
  const workflow = readFileSync(
    join(root, ".github/workflows/deploy.yml"),
    "utf8",
  );
  const script = workflow.match(
    /^        id: blobs\n[\s\S]*?^        run: \|\n((?: {10}.*\n)+)/m,
  )?.[1];
  assert.ok(script, "workflow has a blob-recording step");
  execFileSync("bash", ["-c", script], {
    cwd: work,
    env: {
      ...baseEnv,
      CHANGESETS: outputs.changesets,
      CONSUMED_CHANGESETS: outputs.consumed_changesets,
      GITHUB_OUTPUT: blobsOutput,
    },
  });
  const maps = readOutputs(blobsOutput);
  return {
    releases: JSON.parse(outputs.releases),
    hasBinaries: outputs.has_binaries,
    blobs: JSON.parse(maps.changeset_blobs),
    consumedBlobs: JSON.parse(maps.consumed_changeset_blobs),
  };
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
    const plan = planRelease(work);
    assert.deepEqual(plan.blobs, changesetBlobs(work, sha));
    assert.deepEqual(plan.consumedBlobs, {});
    const before = remoteRefs(remote);
    const result = runRelease(work, sha, plan.releases, {
      CHANGESET_BLOBS: JSON.stringify(plan.blobs),
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /changed nothing/);
    assert.equal(remoteRefs(remote), before);
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

test("release does not restore a rewound main", { skip }, (t) => {
  const { dir, remote, work } = releaseFixture(t);
  const before = git(work, "rev-parse", "HEAD");
  const sha = addChangeset(work, "one", { a: "patch" });
  // A maintainer rewinds main to the commit before the deployed revision.
  const other = join(dir, "rewind");
  git(dir, "clone", "--quiet", "--branch", "main", remote, other);
  git(
    other,
    "push",
    "--quiet",
    "--force",
    "origin",
    `${before}:refs/heads/main`,
  );
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
  assert.equal(result.status, 1);
  assert.match(result.stdout, /no longer contains the deployed revision/);
  assert.equal(remoteGit(remote, "rev-parse", "main"), before);
  assert.equal(remoteGit(remote, "tag"), "");
});

test("release stops when main changed package manifests", { skip }, (t) => {
  const { dir, remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "patch" });
  moveMain(
    dir,
    remote,
    "pkgs/b/package.json",
    `${JSON.stringify({ name: "b", version: "0.0.0", private: true, dependencies: { a: "workspace:0.0.0" } }, null, 2)}\n`,
  );
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
  assert.equal(result.status, 1);
  assert.match(
    result.stdout,
    /main changed package manifests .*pkgs\/b\/package\.json/,
  );
  assert.equal(remoteGit(remote, "tag"), "");
});

test(
  "release rejects a missing release list before changing anything",
  { skip },
  (t) => {
    const { remote, work } = releaseFixture(t);
    const sha = addChangeset(work, "one", { a: "patch" });
    for (const releases of ["", "null", "{}"]) {
      const result = spawnSync("bash", [release], {
        cwd: work,
        encoding: "utf8",
        env: {
          ...baseEnv,
          RELEASES: releases,
          CHANGESET_BLOBS: JSON.stringify(changesetBlobs(work, sha)),
          SHA: sha,
        },
      });
      assert.equal(result.status, 1, JSON.stringify(releases));
      assert.match(result.stdout, /RELEASES is not a JSON list/);
    }
    assert.equal(remoteGit(remote, "rev-parse", "main"), sha);
    assert.equal(git(work, "status", "--porcelain"), "");
  },
);

test(
  "release stops when main renamed a package manifest away",
  { skip },
  (t) => {
    const { dir, remote, work } = releaseFixture(t);
    const sha = addChangeset(work, "one", { a: "patch" });
    const other = join(dir, "rename");
    git(dir, "clone", "--quiet", "--branch", "main", remote, other);
    git(other, "mv", "pkgs/b/package.json", "pkgs/b/package.disabled");
    commitAll(other, "rename manifest");
    git(other, "push", "--quiet", "origin", "HEAD:refs/heads/main");
    const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }]);
    assert.equal(result.status, 1);
    assert.match(
      result.stdout,
      /main changed package manifests .*pkgs\/b\/package\.json/,
    );
    assert.equal(remoteGit(remote, "tag"), "");
  },
);

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

// --- release-recovery.sh ---------------------------------------------------

const recoveryRunId = "555";

// The state of a rerun after the atomic push landed although the job failed:
// the remote holds the version commit and the tags, and the rerun checks out
// the planned revision again. `moved` moves `main` during the deployment, so
// that the release pushes a cherry-picked copy of the tagged commit.
function landedRelease(t, { moved = false, land = true, mixed = false } = {}) {
  const fixture = releaseFixture(t, { tide: mixed });
  const { dir, remote, work } = fixture;
  const name = mixed ? "tide-cli" : "a";
  if (mixed) {
    addChangeset(work, "native", { delidev: "patch" });
    addChangeset(work, "empty", {});
    addChangeset(work, "none", { a: "none" });
  }
  const sha = addChangeset(work, "one", { [name]: "patch" });
  const blobs = changesetBlobs(work, sha);
  const tag = `${name}@0.0.1`;
  const plan = mixed
    ? planRelease(work)
    : { releases: [{ name, version: "0.0.1" }], blobs, consumedBlobs: blobs };
  if (moved) moveMain(dir, remote, "moved.txt", "moved\n");
  if (land) {
    const result = runRelease(work, sha, plan.releases, {
      CHANGESET_BLOBS: JSON.stringify(plan.blobs),
      GITHUB_RUN_ID: recoveryRunId,
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    // The rerun starts from a fresh checkout of the planned revision.
    git(work, "reset", "--quiet", "--hard", sha);
    git(work, "tag", "--delete", tag);
  }
  return { ...fixture, ...plan, sha, tag };
}

// Every ref of the remote, to prove that a recovery writes nothing.
const remoteRefs = (remote) =>
  remoteGit(remote, "for-each-ref", "--format=%(refname) %(objectname)");

function runRecovery(t, state, { env = {}, responses } = {}) {
  const dir = scratch(t);
  const bin = stubGh(
    dir,
    responses ??
      runs([7, "completed", "success", 2], [6, "completed", "success"]),
  );
  const output = join(dir, "output");
  const summary = join(dir, "summary.md");
  writeFileSync(output, "");
  writeFileSync(summary, "");
  const before = remoteRefs(state.remote);
  const result = spawnSync("bash", [recovery], {
    cwd: state.work,
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      RELEASES: JSON.stringify(state.releases),
      CONSUMED_CHANGESET_BLOBS: JSON.stringify(state.consumedBlobs),
      SHA: state.sha,
      GITHUB_RUN_ID: recoveryRunId,
      GITHUB_RUN_ATTEMPT: "2",
      GITHUB_RUN_NUMBER: "7",
      REPOSITORY: "owner/repo",
      WORKFLOW: "deploy.yml",
      GH_TOKEN: "token",
      CHECK_RETRY_DELAY: "0",
      GITHUB_OUTPUT: output,
      GITHUB_STEP_SUMMARY: summary,
      ...env,
    },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  return {
    ...result,
    recovered: readFileSync(output, "utf8").trim(),
    summary: readFileSync(summary, "utf8"),
    refsUnchanged: remoteRefs(state.remote) === before,
  };
}

const identity = ["-c", "user.name=t", "-c", "user.email=t@example.invalid"];

test("release records the run of the deployment", { skip }, (t) => {
  const { remote, work } = releaseFixture(t);
  const sha = addChangeset(work, "one", { a: "patch" });
  const result = runRelease(work, sha, [{ name: "a", version: "0.0.1" }], {
    GITHUB_RUN_ID: "42",
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const message = remoteGit(remote, "log", "-1", "--format=%B", "main");
  assert.match(message, /^Deployed revision: [0-9a-f]{40}$/m);
  assert.match(message, /^Deployment run: 42$/m);
});

test("recovery accepts a release that already landed", { skip }, (t) => {
  const state = landedRelease(t);
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=true");
  assert.ok(result.refsUnchanged);
  assert.match(result.summary, /Recovered/);
  assert.match(result.summary, /`a@0.0.1`/);
  // The planned revision stays checked out and nothing was committed.
  assert.equal(git(state.work, "rev-parse", "HEAD"), state.sha);
});

test("recovery accepts a cherry-picked version commit", { skip }, (t) => {
  const state = landedRelease(t, { moved: true });
  const tagged = remoteGit(state.remote, "rev-parse", "a@0.0.1^{commit}");
  assert.notEqual(tagged, remoteGit(state.remote, "rev-parse", "main"));
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=true");
  assert.ok(result.refsUnchanged);
});

for (const moved of [false, true]) {
  const mode = moved ? "cherry-picked" : "direct";
  test(
    `recovery accepts a ${mode} Tide release with an ignored changeset`,
    { skip },
    (t) => {
      const state = landedRelease(t, { mixed: true, moved });
      const { remote, sha, tag, blobs, consumedBlobs } = state;
      const planned = changesetBlobs(state.work, sha);
      const { native, ...consumed } = planned;
      assert.ok(native);
      assert.deepEqual(Object.keys(consumed), ["empty", "none", "one"]);
      assert.deepEqual(blobs, planned);
      assert.deepEqual(consumedBlobs, consumed);
      assert.equal(state.hasBinaries, "true");
      assert.deepEqual(state.releases, [
        { name: "tide-cli", version: "0.0.1" },
      ]);
      for (const id of Object.keys(consumed)) {
        assert.equal(
          remoteGit(remote, "ls-tree", "main", `.changeset/${id}.md`),
          "",
        );
      }
      assert.equal(
        remoteGit(remote, "rev-parse", "main:.changeset/native.md"),
        native,
      );
      assert.equal(remoteGit(remote, "cat-file", "-t", tag), "tag");
      assert.equal(
        remoteGit(remote, "rev-parse", `${tag}^{commit}`) ===
          remoteGit(remote, "rev-parse", "main"),
        !moved,
      );

      const result = runRecovery(t, state);
      assert.equal(
        result.recovered,
        "recovered=true",
        result.stdout + result.stderr,
      );
      assert.ok(result.refsUnchanged);
    },
  );
}

test(
  "release rejects a changed ignored changeset when main moves",
  { skip },
  (t) => {
    const { dir, remote, work, sha, releases, blobs } = landedRelease(t, {
      mixed: true,
      land: false,
    });
    moveMain(
      dir,
      remote,
      ".changeset/native.md",
      '---\n"delidev": patch\n---\n\nChanged.\n',
    );
    const before = remoteRefs(remote);
    const result = runRelease(work, sha, releases, {
      CHANGESET_BLOBS: JSON.stringify(blobs),
    });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(
      result.stdout,
      /\.changeset\/native\.md changed or was removed on main/,
    );
    assert.equal(remoteRefs(remote), before);
  },
);

test(
  "recovery rejects a release that leaves an expected changeset pending",
  { skip },
  (t) => {
    const state = landedRelease(t, { mixed: true });
    const { work, sha } = state;
    git(work, "fetch", "--quiet", "origin", "main");
    git(work, "checkout", "--quiet", "--detach", "FETCH_HEAD");
    git(
      work,
      "restore",
      "--source",
      sha,
      "--staged",
      "--worktree",
      ".changeset/one.md",
    );
    git(work, ...identity, "commit", "--quiet", "--amend", "--no-edit");
    git(work, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/main");
    git(work, "checkout", "--quiet", "--detach", sha);

    const result = runRecovery(t, state);
    assert.equal(result.recovered, "recovered=false");
    assert.match(
      result.stdout,
      /\.changeset\/one\.md is not consumed by the version commit/,
    );
    assert.ok(result.refsUnchanged);
  },
);

test("recovery requires a nonempty, valid consumed blob map", { skip }, (t) => {
  const state = landedRelease(t);
  for (const map of [
    "",
    "{}",
    '{"one":"short"}',
    '{"../one":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}',
  ]) {
    const result = runRecovery(t, state, {
      env: {
        CONSUMED_CHANGESET_BLOBS: map,
        CHANGESET_BLOBS: JSON.stringify(state.blobs),
      },
    });
    assert.equal(result.recovered, "recovered=false");
    assert.match(
      result.stdout,
      /CONSUMED_CHANGESET_BLOBS is not a nonempty map/,
    );
    assert.ok(result.refsUnchanged);
  }
});

test("recovery proves nothing when the push never landed", { skip }, (t) => {
  const state = landedRelease(t, { land: false });
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /not exactly one|missing on the remote/);
  assert.ok(result.refsUnchanged);
});

test("recovery needs a rerun", { skip }, (t) => {
  const state = landedRelease(t);
  const result = runRecovery(t, state, { env: { GITHUB_RUN_ATTEMPT: "1" } });
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /not a rerun/);
});

test("recovery rejects a version commit of another run", { skip }, (t) => {
  const state = landedRelease(t);
  const result = runRecovery(t, state, { env: { GITHUB_RUN_ID: "556" } });
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /not exactly one/);
});

test("recovery rejects a missing tag", { skip }, (t) => {
  const state = landedRelease(t);
  remoteGit(state.remote, "tag", "--delete", "a@0.0.1");
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /tag a@0\.0\.1 is missing/);
});

test("recovery rejects a tag on the wrong commit", { skip }, (t) => {
  const state = landedRelease(t);
  // The tag keeps its message but moved to the deployed revision.
  git(
    state.remote,
    ...identity,
    "tag",
    "--force",
    "--annotate",
    "a@0.0.1",
    "--message",
    `a@0.0.1 deployed from ${state.sha}`,
    state.sha,
  );
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /does not have the deployed revision/);
});

test("recovery rejects a tag of another deployment", { skip }, (t) => {
  const state = landedRelease(t);
  const tagged = remoteGit(state.remote, "rev-parse", "a@0.0.1^{commit}");
  git(
    state.remote,
    ...identity,
    "tag",
    "--force",
    "--annotate",
    "a@0.0.1",
    "--message",
    `a@0.0.1 deployed from ${"d".repeat(40)}`,
    tagged,
  );
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /not an annotated tag of the deployment/);
});

test("recovery rejects changesets that differ from the plan", { skip }, (t) => {
  const state = landedRelease(t);
  const result = runRecovery(t, {
    ...state,
    consumedBlobs: { one: "e".repeat(40) },
  });
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /not consumed by the version commit/);
});

test("recovery rejects a changeset that exists again", { skip }, (t) => {
  const state = landedRelease(t, { mixed: true });
  // The same file content returns to main, so its blob equals the plan.
  git(state.work, "fetch", "--quiet", "origin", "main");
  git(state.work, "checkout", "--quiet", "--detach", "FETCH_HEAD");
  git(
    state.work,
    "restore",
    "--source",
    state.sha,
    "--worktree",
    ".changeset/one.md",
  );
  commitAll(state.work, "restore changeset");
  git(state.work, "push", "--quiet", "origin", "HEAD:refs/heads/main");
  git(state.work, "checkout", "--quiet", "--detach", state.sha);
  const result = runRecovery(t, state);
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /exists again on the tip of main/);
});

test(
  "recovery rejects a version commit that changes other files",
  { skip },
  (t) => {
    const state = landedRelease(t, { moved: true });
    // The commit on `main` is a copy of the tagged commit with an extra file.
    const other = join(state.dir, "forge");
    git(state.dir, "clone", "--quiet", "--branch", "main", state.remote, other);
    const version = git(other, "rev-parse", "HEAD");
    git(other, "reset", "--quiet", "--hard", `${version}~1`);
    git(other, "cherry-pick", "--no-commit", version);
    writeFileSync(join(other, "extra.txt"), "extra\n");
    git(other, "add", "-A");
    git(other, ...identity, "commit", "--quiet", "--no-verify", "-C", version);
    git(other, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/main");
    const result = runRecovery(t, state);
    assert.equal(result.recovered, "recovered=false");
    assert.match(result.stdout, /change different files/);
  },
);

test("recovery rejects a superseding deployment", { skip }, (t) => {
  const state = landedRelease(t);
  const result = runRecovery(t, state, {
    responses: runs(
      [8, "completed", "success"],
      [7, "completed", "success", 2],
    ),
  });
  assert.equal(result.recovered, "recovered=false");
  assert.match(result.stdout, /run #8 of deploy\.yml started after this run/);
  assert.match(result.stdout, /newer deployment run exists/);
  assert.ok(result.refsUnchanged);
});

test("deploy.yml tries the recovery before the stale-plan check", () => {
  const workflow = readFileSync(
    join(root, ".github/workflows/deploy.yml"),
    "utf8",
  );
  const job = workflow.match(/^ {2}release:\n(?:(?: {4}.*)?\n)+/m)?.[0] ?? "";
  const steps = job.split(/^ {6}- /m).slice(1);
  const index = (needle) => steps.findIndex((step) => step.includes(needle));
  const recover = index("scripts/ci/release-recovery.sh");
  assert.ok(recover >= 0);
  assert.ok(recover < index("scripts/ci/check-pending-changesets.sh"));
  assert.match(steps[recover], /id: recover/);
  assert.match(steps[recover], /github\.run_attempt > 1/);
  assert.ok(
    steps[recover].includes(
      "CONSUMED_CHANGESET_BLOBS: ${{ needs.plan.outputs.consumed_changeset_blobs }}",
    ),
  );
  assert.ok(
    steps[index("scripts/ci/check-pending-changesets.sh")].includes(
      "CHANGESET_BLOBS: ${{ needs.plan.outputs.changeset_blobs }}",
    ),
  );
  assert.ok(
    steps[index("scripts/ci/deploy-release.sh")].includes(
      "CHANGESET_BLOBS: ${{ needs.plan.outputs.changeset_blobs }}",
    ),
  );
  const publish =
    workflow.match(/^ {2}publish-binaries:\n(?:(?: {4}.*)?\n)+/m)?.[0] ?? "";
  assert.match(
    publish,
    /needs\.release\.result == 'success' && needs\.plan\.outputs\.has_binaries == 'true'/,
  );
  // Every step after the recovery is skipped when the release already landed.
  for (const step of steps.slice(recover + 1)) {
    assert.match(step, /steps\.recover\.outputs\.recovered != 'true'/);
  }
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

// Stubs `curl`. Responses are keyed by `curl`. Each response is used once and
// the last one repeats; {exit} makes the call fail.
function stubProvider(dir, responses) {
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  writeFileSync(join(bin, "package.json"), '{"type":"commonjs"}\n');
  writeFileSync(join(dir, "responses.json"), JSON.stringify(responses));
  writeFileSync(join(dir, "calls.log"), "");
  for (const command of ["curl"]) {
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

const pagesStage = (name, status, environment = "production") => ({
  stdout: { result: { environment, latest_stage: { name, status } } },
});

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

const providerUsage = /usage: .*pages-id <file> \| wait-pages <project> <id>/;

for (const args of [
  ["pages-id"],
  ["read-pages"],
  ["read-pages", "proj"],
  ["wait-pages"],
  ["wait-pages", "proj"],
]) {
  test(
    `provider prints the usage line for \`${args.join(" ")}\``,
    { skip },
    (t) => {
      const result = runProvider(t, args);
      assert.equal(result.status, 2, result.stderr);
      assert.match(result.stderr, providerUsage);
      assert.doesNotMatch(result.stderr, /unbound variable/);
      // The usage check stops the script before any provider CLI call.
      assert.equal(result.calls, "");
    },
  );
}

test(
  "provider reports a missing argument before a missing timeout command",
  { skip },
  (t) => {
    // A PATH that holds only `bash` hides both `timeout` and `gtimeout`.
    const bin = join(scratch(t), "bash-only");
    mkdirSync(bin);
    const bash = spawnSync("sh", ["-c", "command -v bash"], {
      encoding: "utf8",
    }).stdout.trim();
    symlinkSync(bash, join(bin, "bash"));
    const result = runProvider(t, ["wait-pages"], { env: { PATH: bin } });
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, providerUsage);
    assert.doesNotMatch(result.stderr, /timeout/);
  },
);

test("provider keeps its other bad-usage and error exits", { skip }, (t) => {
  // The Railway commands no longer exist.
  for (const args of [
    [],
    ["bogus"],
    ["railway-id"],
    ["wait-railway", "a", "b"],
  ]) {
    const result = runProvider(t, args);
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, providerUsage);
  }
  // An empty value is an argument, not a missing one.
  const emptyFile = runProvider(t, ["pages-id", ""]);
  assert.equal(emptyFile.status, 1);
  assert.match(
    emptyFile.stderr,
    /::error::wrangler pages deploy returned no deployment id\./,
  );
});

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
      assert.match(
        result.stdout,
        new RegExp(`ended with production ${name} ${status}`),
      );
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
  "provider reports a missing or directory Wrangler output path without a jq error",
  { skip },
  (t) => {
    const dir = scratch(t);
    for (const path of [join(dir, "missing.ndjson"), dir]) {
      const result = runProvider(t, ["pages-id", path]);
      assert.equal(result.status, 1);
      assert.equal(result.stdout, "");
      assert.match(result.stderr, /returned no deployment id/);
      assert.doesNotMatch(result.stderr, /jq:/);
    }
  },
);

test(
  "provider reports a Wrangler output file with only non-JSON lines",
  { skip },
  (t) => {
    const dir = scratch(t);
    const file = join(dir, "wrangler.ndjson");
    writeFileSync(file, "junk\n");
    const result = runProvider(t, ["pages-id", file]);
    assert.equal(result.status, 1);
    assert.equal(result.stdout, "");
    assert.match(result.stderr, /returned no deployment id/);
    assert.doesNotMatch(result.stderr, /jq:/);
  },
);

test(
  "provider skips non-JSON lines around a Pages deploy record",
  { skip },
  (t) => {
    const dir = scratch(t);
    const file = join(dir, "wrangler.ndjson");
    writeFileSync(
      file,
      'junk\n{"type":"pages-deploy","version":1,"deployment_id":"pages-1"}\n',
    );
    const result = runProvider(t, ["pages-id", file]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), "pages-1");
  },
);

test(
  "provider tolerates a few failed Pages status reads",
  { skip: skipWait },
  (t) => {
    const ok = runProvider(t, ["wait-pages", "site", "pages-1"], {
      responses: {
        curl: [{ exit: 22 }, { exit: 22 }, pagesStage("deploy", "success")],
      },
    });
    assert.equal(ok.status, 0, ok.stdout + ok.stderr);
    assert.match(ok.stdout, /failure 2 of 5/);
    const failed = runProvider(t, ["wait-pages", "site", "pages-1"], {
      responses: { curl: [{ exit: 22 }] },
    });
    assert.equal(failed.status, 1);
    assert.match(failed.stdout, /5 times in a row/);
  },
);

test(
  "provider gives up on a Pages deployment after the timeout",
  { skip: skipWait },
  (t) => {
    const result = runProvider(t, ["wait-pages", "site", "pages-1"], {
      responses: { curl: [pagesStage("deploy", "active")] },
      env: { DEPLOY_POLL_TIMEOUT: "0" },
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /did not finish within 0 seconds/);
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

// --- deploy-targets.sh -----------------------------------------------------

const targets = join(root, "scripts/ci/deploy-targets.sh");

// Writes an executable shell stub that appends its name and arguments to
// `calls.log` and then runs `body`.
function shellStub(bin, name, body = "") {
  const file = join(bin, name);
  writeFileSync(
    file,
    `#!/usr/bin/env bash\necho "${name} $*" >> "$STUB_DIR/calls.log"\n${body}\n`,
  );
  chmodSync(file, 0o755);
  return file;
}

const digest = `sha256:${"d".repeat(64)}`;

// Runs deploy-targets.sh with stubbed `pnpm`, `docker`, `ssh`, guard and
// provider. The guard fails once `withdrawn` exists, as when a changeset is
// withdrawn on main. The `ssh` stub saves the remote script from standard
// input to `remote-<n>.sh`. The script appends its job summary to `summary.md`
// in the fixture directory, never to the `GITHUB_STEP_SUMMARY` file that this
// process may inherit from a CI job; the content is returned as `summary`.
function runTargets(
  t,
  command,
  { env = {}, pnpmBody = "", dockerBody = "", sshBody = "" } = {},
) {
  const dir = scratch(t);
  const bin = join(dir, "bin");
  mkdirSync(bin);
  writeFileSync(join(dir, "calls.log"), "");
  shellStub(bin, "pnpm", pnpmBody);
  shellStub(
    bin,
    "docker",
    dockerBody ||
      `[[ "$1 $2" != "buildx imagetools" ]] || echo '{"digest":"${digest}"}'`,
  );
  shellStub(
    bin,
    "ssh",
    `n=$(ls "$STUB_DIR" | grep -c '^remote-' || true); cat > "$STUB_DIR/remote-$n.sh"\n${sshBody}`,
  );
  writeFileSync(join(dir, "ssh_config"), "Host droplet\n");
  const guardStub = shellStub(
    bin,
    "guard",
    '[[ ! -e "$STUB_DIR/withdrawn" ]] || { echo "::error::The plan is stale"; exit 1; }',
  );
  const providerStub = shellStub(
    bin,
    "provider",
    'case "$1" in pages-id) echo pages ;; esac',
  );
  mkdirSync(join(dir, "servers"));
  // Site outputs, relative to the working directory, as after a build.
  for (const site of ["docs", "site"]) {
    mkdirSync(join(dir, "apps", site, "dist"), { recursive: true });
  }
  for (const name of ["api", "worker"]) {
    const context = join(dir, `ctx-${name}`);
    mkdirSync(context);
    writeFileSync(join(context, "Dockerfile"), "FROM scratch\n");
    execFileSync("tar", [
      "-czf",
      join(dir, "servers", `${name}.tar.gz`),
      "-C",
      context,
      ".",
    ]);
  }
  const summary = join(dir, "summary.md");
  writeFileSync(summary, "");
  const result = spawnSync("bash", [targets, command], {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      GITHUB_STEP_SUMMARY: summary,
      STUB_DIR: dir,
      RUNNER_TEMP: dir,
      SHA: "a".repeat(40),
      DEPLOY_GUARD: guardStub,
      DEPLOY_PROVIDER: providerStub,
      DROPLET_REGISTRY: "registry.digitalocean.com/delino",
      DROPLET_SSH_CONFIG: join(dir, "ssh_config"),
      SERVERS: JSON.stringify([
        {
          name: "api",
          version: "1.0.0",
          service: "api",
          hostname: "api.example.com",
          port: "8080",
          upstreamProtocol: "h2c",
          udpPorts: "3478",
        },
        {
          name: "worker",
          version: "1.0.0",
          service: "worker-svc",
          hostname: "worker.example.com",
          port: "9000",
          upstreamProtocol: "http1",
          udpPorts: "none",
        },
      ]),
      SITES: JSON.stringify([
        {
          name: "docs",
          version: "1.0.0",
          project: "docs",
          output: "apps/docs/dist",
        },
        {
          name: "site",
          version: "1.0.0",
          project: "site",
          output: "apps/site/dist",
        },
      ]),
      ...env,
    },
  });
  return {
    ...result,
    dir,
    summary: readFileSync(summary, "utf8"),
    calls: readFileSync(join(dir, "calls.log"), "utf8").trim().split("\n"),
  };
}

test("targets check the plan before each site upload", { skip }, (t) => {
  const result = runTargets(t, "static-sites");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  // Command, subcommand and, for uploads and waits, the target.
  const order = result.calls.map((call) => {
    const words = call.split(" ").filter(Boolean);
    const keep = words[0] === "pnpm" || words[1] === "wait-pages" ? 3 : 2;
    return words.slice(0, keep).join(" ");
  });
  assert.deepEqual(order, [
    "guard",
    "pnpm exec wrangler",
    "provider pages-id",
    "provider wait-pages docs",
    "guard",
    "pnpm exec wrangler",
    "provider pages-id",
    "provider wait-pages site",
  ]);
});

test("targets deploy site outputs from SITES_ROOT", { skip }, (t) => {
  const root = scratch(t);
  for (const site of ["docs", "site"]) {
    mkdirSync(join(root, "apps", site, "dist"), { recursive: true });
  }
  const result = runTargets(t, "static-sites", { env: { SITES_ROOT: root } });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const uploads = result.calls.filter((call) =>
    call.startsWith("pnpm exec wrangler"),
  );
  assert.equal(uploads.length, 2);
  assert.ok(
    uploads[0].includes(`pages deploy ${root}/apps/docs/dist `),
    uploads[0],
  );
});

test("targets write the job summary to a fixture-local file", { skip }, (t) => {
  const sites = runTargets(t, "static-sites");
  assert.equal(sites.status, 0, sites.stdout + sites.stderr);
  assert.equal(
    sites.summary,
    [
      "## Static sites",
      "- docs@1.0.0: Cloudflare Pages project `docs`, deployment `pages`",
      "- site@1.0.0: Cloudflare Pages project `site`, deployment `pages`",
      "",
    ].join("\n"),
  );
  const servers = runTargets(t, "servers");
  assert.equal(servers.status, 0, servers.stdout + servers.stderr);
  const registry = "registry.digitalocean.com/delino";
  assert.equal(
    servers.summary,
    [
      "## Servers",
      `- api@1.0.0: Droplet service \`api\` at \`https://api.example.com\`, image \`${registry}/api@${digest}\``,
      `- worker@1.0.0: Droplet service \`worker-svc\` at \`https://worker.example.com\`, image \`${registry}/worker-svc@${digest}\``,
      "",
    ].join("\n"),
  );
});

// Re-runs the target tests that reach `deploy-targets.sh` in a child Node test
// process whose `GITHUB_STEP_SUMMARY` names a sentinel file, as in a CI job.
// The child must leave the sentinel byte-for-byte unchanged.
test(
  "targets tests leave an inherited GITHUB_STEP_SUMMARY file unchanged",
  { skip },
  (t) => {
    const dir = scratch(t);
    const sentinel = join(dir, "job-summary.md");
    const content = "## Job summary of the surrounding CI job\n";
    writeFileSync(sentinel, content);
    // The child must not join the parent's test run, and must not recurse
    // into this test.
    const env = { ...process.env, GITHUB_STEP_SUMMARY: sentinel };
    delete env.NODE_TEST_CONTEXT;
    const names = [
      "targets check the plan before each site upload",
      "targets push each server image and deploy it by digest over SSH",
      "targets fail when a site build or its output is missing",
    ];
    const result = spawnSync(
      process.execPath,
      [
        "--test",
        "--test-reporter=tap",
        ...names.map((name) => `--test-name-pattern=^${name}$`),
        fileURLToPath(import.meta.url),
      ],
      { cwd: root, encoding: "utf8", env },
    );
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /# pass 3\b/);
    assert.equal(readFileSync(sentinel, "utf8"), content);
  },
);

test("targets build sites without uploading them", { skip }, (t) => {
  const result = runTargets(t, "build-static-sites");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.calls, [
    "pnpm exec turbo run build --filter=docs",
    "pnpm exec turbo run build --filter=site",
  ]);
});

// Sites whose `deploy.buildEnv` maps build variables to repository variables.
const sitesWithBuildEnv = JSON.stringify([
  {
    name: "docs",
    version: "1.0.0",
    project: "docs",
    output: "apps/docs/dist",
    buildEnv: {
      PUBLIC_LOGTO_ENDPOINT: "LOGTO_ENDPOINT",
      PUBLIC_LOGTO_APP_ID: "DOCS_LOGTO_APP_ID",
    },
  },
  {
    name: "site",
    version: "1.0.0",
    project: "site",
    output: "apps/site/dist",
    buildEnv: {
      PUBLIC_LOGTO_ENDPOINT: "LOGTO_ENDPOINT",
      PUBLIC_LOGTO_APP_ID: "SITE_LOGTO_APP_ID",
    },
  },
]);

// Records the build variables that each `pnpm` call received.
const pnpmEnvBody =
  'printf "%s|%s|%s\\n" "${PUBLIC_LOGTO_ENDPOINT-unset}" "${PUBLIC_LOGTO_APP_ID-unset}" "${PUBLIC_OTHER-unset}" >> "$STUB_DIR/env.log"';

test("targets build each site with its mapped variables", { skip }, (t) => {
  const result = runTargets(t, "build-static-sites", {
    env: {
      SITES: sitesWithBuildEnv,
      PUBLIC_OTHER: "from runner",
      BUILD_VARIABLES: JSON.stringify({
        // GitHub variable names are case-insensitive.
        logto_endpoint: "https://auth.example.com",
        DOCS_LOGTO_APP_ID: "docs app\nwith a line feed",
        SITE_LOGTO_APP_ID: "site-app",
        UNUSED: "x",
      }),
    },
    pnpmBody: pnpmEnvBody,
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.calls, [
    "pnpm exec turbo run build --filter=docs",
    "pnpm exec turbo run build --filter=site",
  ]);
  assert.equal(
    readFileSync(join(result.dir, "env.log"), "utf8"),
    "https://auth.example.com|docs app\nwith a line feed|from runner\n" +
      "https://auth.example.com|site-app|from runner\n",
  );
  // Names only: the log does not repeat the values.
  assert.match(
    result.stdout,
    /Build variables: PUBLIC_LOGTO_ENDPOINT,PUBLIC_LOGTO_APP_ID/,
  );
  assert.doesNotMatch(result.stdout, /site-app/);
  assert.deepEqual(
    readdirSync(result.dir).filter((f) => f.startsWith("build-env-")),
    [],
  );
});

test(
  "targets fail before a site build when a mapped variable is missing",
  { skip },
  (t) => {
    for (const variables of [
      undefined,
      "",
      JSON.stringify({ LOGTO_ENDPOINT: "https://auth.example.com" }),
      JSON.stringify({
        LOGTO_ENDPOINT: "https://auth.example.com",
        DOCS_LOGTO_APP_ID: "",
      }),
      // Blank values: the consoles trim them, so they count as unset.
      ...[" ", "\t", " \t ", "\r\n", "\u00a0", "\ufeff", "\u3000"].map(
        (blank) =>
          JSON.stringify({
            LOGTO_ENDPOINT: "https://auth.example.com",
            DOCS_LOGTO_APP_ID: blank,
          }),
      ),
      JSON.stringify({
        LOGTO_ENDPOINT: "https://auth.example.com",
        DOCS_LOGTO_APP_ID: "a\u0000b",
      }),
    ]) {
      const result = runTargets(t, "build-static-sites", {
        env: {
          SITES: sitesWithBuildEnv,
          ...(variables === undefined ? {} : { BUILD_VARIABLES: variables }),
        },
        pnpmBody: pnpmEnvBody,
      });
      assert.equal(result.status, 1, variables);
      assert.match(
        result.stderr,
        /::error::The build of docs needs repository variables that are not set, are blank or contain NUL: .*PUBLIC_LOGTO_APP_ID \(repository variable DOCS_LOGTO_APP_ID\)/,
        variables,
      );
      assert.deepEqual(result.calls, [""], variables);
    }
  },
);

test(
  "targets reject BUILD_VARIABLES that is not an object of strings",
  { skip },
  (t) => {
    for (const variables of ["[]", "not json", '{"A":1}']) {
      const result = runTargets(t, "build-static-sites", {
        env: { BUILD_VARIABLES: variables },
      });
      assert.equal(result.status, 1, variables);
      assert.match(result.stderr, /BUILD_VARIABLES is not a JSON object/);
      assert.deepEqual(result.calls, [""], variables);
    }
  },
);

test(
  "targets fail when a site build or its output is missing",
  { skip },
  (t) => {
    const dir = scratch(t);
    // `static-sites` without a prior build: the outputs do not exist.
    const result = spawnSync("bash", [targets, "static-sites"], {
      cwd: dir,
      encoding: "utf8",
      env: {
        ...baseEnv,
        SITES: JSON.stringify([
          {
            name: "docs",
            version: "1.0.0",
            project: "docs",
            output: "apps/docs/dist",
          },
        ]),
        DEPLOY_GUARD: "/bin/false",
        GITHUB_STEP_SUMMARY: join(dir, "summary.md"),
      },
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /was not built; run build-static-sites first/);
  },
);

test(
  "targets stop the next site upload when the plan goes stale",
  { skip },
  (t) => {
    const result = runTargets(t, "static-sites", {
      pnpmBody: '[[ "$2" != wrangler ]] || touch "$STUB_DIR/withdrawn"',
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /The plan is stale/);
    const uploads = result.calls.filter((call) =>
      call.startsWith("pnpm exec wrangler"),
    );
    assert.equal(uploads.length, 1);
    assert.match(uploads[0], /--project-name docs/);
  },
);

test(
  "targets stop the next server upload when the plan goes stale",
  { skip },
  (t) => {
    const result = runTargets(t, "servers", {
      sshBody: 'touch "$STUB_DIR/withdrawn"',
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /The plan is stale/);
    const deploys = result.calls.filter((call) => call.startsWith("ssh "));
    assert.equal(deploys.length, 1);
    assert.match(deploys[0], / api registry/);
  },
);

test(
  "targets push each server image and deploy it by digest over SSH",
  { skip },
  (t) => {
    const result = runTargets(t, "servers");
    assert.equal(result.status, 0, result.stdout + result.stderr);
    const sha = "a".repeat(40);
    const registry = "registry.digitalocean.com/delino";
    assert.deepEqual(result.calls, [
      `docker load --quiet --input ${result.dir}/servers/api.tar.gz`,
      `docker tag api:${sha} ${registry}/api:${sha}`,
      `docker push --quiet ${registry}/api:${sha}`,
      `docker buildx imagetools inspect ${registry}/api:${sha} --format {{json .Manifest}}`,
      "guard ",
      `ssh -F ${result.dir}/ssh_config droplet bash -s -- api ${registry}/api@${digest} 8080 api.example.com h2c 3478`,
      `docker load --quiet --input ${result.dir}/servers/worker.tar.gz`,
      `docker tag worker:${sha} ${registry}/worker-svc:${sha}`,
      `docker push --quiet ${registry}/worker-svc:${sha}`,
      `docker buildx imagetools inspect ${registry}/worker-svc:${sha} --format {{json .Manifest}}`,
      "guard ",
      `ssh -F ${result.dir}/ssh_config droplet bash -s -- worker-svc ${registry}/worker-svc@${digest} 9000 worker.example.com http1 none`,
    ]);
    // The remote side receives the deploy script on standard input.
    assert.equal(
      readFileSync(join(result.dir, "remote-0.sh"), "utf8"),
      readFileSync(join(root, "scripts/ci/droplet-deploy.sh"), "utf8"),
    );
  },
);

test(
  "targets stop when the registry returns no digest or the remote deploy fails",
  { skip },
  (t) => {
    const noDigest = runTargets(t, "servers", {
      dockerBody: '[[ "$1 $2" != "buildx imagetools" ]] || echo "{}"',
    });
    assert.equal(noDigest.status, 1);
    assert.match(noDigest.stderr, /returned no digest/);
    assert.ok(!noDigest.calls.some((call) => call.startsWith("ssh ")));
    const failed = runTargets(t, "servers", { sshBody: "exit 1" });
    assert.equal(failed.status, 1);
    assert.equal(
      failed.calls.filter((call) => call.startsWith("ssh ")).length,
      1,
    );
  },
);

test(
  "targets require the registry and the SSH configuration for servers",
  { skip },
  (t) => {
    for (const [env, message] of [
      [{ DROPLET_REGISTRY: "" }, /DROPLET_REGISTRY is not/],
      [
        { DROPLET_REGISTRY: "registry.digitalocean.com" },
        /DROPLET_REGISTRY is not/,
      ],
      [{ DROPLET_REGISTRY: "Registry/x y" }, /DROPLET_REGISTRY is not/],
      [{ DROPLET_SSH_CONFIG: "" }, /DROPLET_SSH_CONFIG is not/],
      [{ DROPLET_SSH_CONFIG: "/nonexistent" }, /DROPLET_SSH_CONFIG is not/],
    ]) {
      const result = runTargets(t, "servers", { env });
      assert.equal(result.status, 1, JSON.stringify(env));
      assert.match(result.stderr, message);
      assert.deepEqual(result.calls, [""]);
    }
  },
);

test(
  "provider fails on a Pages preview deployment",
  { skip: skipWait },
  (t) => {
    const result = runProvider(t, ["wait-pages", "site", "pages-1"], {
      responses: { curl: [pagesStage("deploy", "success", "preview")] },
    });
    assert.equal(result.status, 1);
    assert.match(result.stdout, /ended with preview deploy success/);
  },
);

test("targets reject an empty or malformed target list", { skip }, (t) => {
  for (const [command, env] of [
    ["static-sites", { SITES: "" }],
    ["static-sites", { SITES: "[]" }],
    ["servers", { SERVERS: "not json" }],
    ["servers", { SERVERS: '[{"name":"api"}]' }],
    [
      "servers",
      {
        SERVERS:
          '[{"name":"api","version":"1.0.0","service":"api","hostname":"a.example.com"}]',
      },
    ],
  ]) {
    const result = runTargets(t, command, { env });
    assert.equal(result.status, 1, `${command} ${JSON.stringify(env)}`);
    assert.match(result.stderr, /is not a nonempty list of deployables/);
    assert.deepEqual(result.calls, [""]);
  }
});

// --- droplet-deploy.sh -----------------------------------------------------

const dropletDeploy = join(root, "scripts/ci/droplet-deploy.sh");
const dropletDeploymentUuid = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b";
const dropletImage = `registry.digitalocean.com/delino/api@sha256:${"e".repeat(64)}`;

// Runs droplet-deploy.sh the way deploy-targets.sh does (`bash -s` with the
// script on standard input) against stubbed `docker` and `curl`. `old` lists
// the IDs of running containers of the service. Stub behavior:
//   STUB_STATUS   `docker inspect` container status (default running).
//   STUB_HEALTHY  `curl` completes a request when 1 (default 1); otherwise it
//                 fails with a transport error and no status.
//   STUB_HTTP_CODE  HTTP status that a completed `curl` request prints (default 200).
//   STUB_PS_EXIT exit code of `docker ps` (default 0).
//   STUB_RELOAD   exit code of `docker exec <proxy> caddy reload` (default 0).
//   STUB_LOGS     exit code of `docker logs` (default 0).
//   STUB_STOP_FAIL  space-separated container IDs that `docker stop` fails for.
//   STUB_RM_FAIL    space-separated container IDs that `docker rm` fails for
//                 (`docker rm --force` of a replacement still succeeds).
//   STUB_START_FAIL glob of the container names that `docker start` fails for.
//   STUB_OLD_IMAGES space-separated `<container ID>=<image>` pairs: the image
//                 that `docker inspect --format {{.Config.Image}}` prints for
//                 an old container (default: `olderImage`).
//   STUB_INSPECT_IMAGE_FAIL space-separated container IDs whose image
//                 `docker inspect` cannot read.
//   STUB_IMAGE_RM_FAIL space-separated image references that `docker image rm`
//                 fails for.
//   STUB_RUN_EXIT   exit code of `docker run` (default 0), printed with a
//                 Docker diagnostic when not 0.
//   STUB_CREATE_EXIT exit code of `docker create` (default 0), like STUB_RUN_EXIT.
//   STUB_BLOCK_ON  glob that a `docker` call (its arguments joined by spaces)
//                 must match to block until `<scratch>/release-docker` exists,
//                 so that a test can send a signal while the call runs.
// `stubs` adds stub commands (name -> shell body) that shadow system tools, and
// `prepare(deployRoot)` changes the scratch state before the run.
function setupDroplet(
  t,
  args = ["api", dropletImage, "8080", "api.example.com", "http1", "none"],
  {
    old = [],
    envFile = "LOG_LEVEL=info\n",
    route,
    state,
    previous,
    stubs = {},
    prepare,
  } = {},
) {
  const dir = scratch(t);
  const bin = join(dir, "bin");
  const deployRoot = join(dir, "srv");
  mkdirSync(bin);
  mkdirSync(join(deployRoot, "env"), { recursive: true });
  mkdirSync(join(deployRoot, "caddy", "sites"), { recursive: true });
  writeFileSync(join(dir, "calls.log"), "");
  // The deployment identifier that droplet-deploy.sh reads (a fixed UUID, and
  // a file that exists on every platform).
  writeFileSync(join(dir, "id-source"), `${dropletDeploymentUuid}\n`);
  // `null` leaves the env file out.
  if (envFile !== null) {
    writeFileSync(join(deployRoot, "env", `${args[0]}.env`), envFile);
  }
  if (state !== undefined || previous !== undefined) {
    mkdirSync(join(deployRoot, "state"));
  }
  if (state !== undefined) {
    writeFileSync(join(deployRoot, "state", "api.current"), state);
  }
  if (previous !== undefined) {
    writeFileSync(join(deployRoot, "state", "api.previous"), previous);
  }
  if (route !== undefined) {
    writeFileSync(join(deployRoot, "caddy", "sites", "api.caddy"), route);
  }
  shellStub(
    bin,
    "docker",
    `if [[ -n "\${STUB_BLOCK_ON:-}" && "$*" == \$STUB_BLOCK_ON ]]; then
  # The loop also ends when the scratch directory is gone, so that a stub
  # of a test that failed does not outlive it.
  while [[ -d "$STUB_DIR" && ! -e "$STUB_DIR/release-docker" ]]; do sleep 0.05; done
fi
case "$1" in
  ps)
    [[ "\${STUB_PS_EXIT:-0}" == 0 ]] || exit "\$STUB_PS_EXIT"
    # The name check before the start: no container has the new name.
    [[ "$*" != *"--filter name="* ]] || exit 0
    printf '%s\\n' ${old.map((id) => `'${id}'`).join(" ")} | grep . || true ;;
  inspect)
    case "$3" in
      *State.Status*) echo "\${STUB_STATUS:-running}" ;;
      *Config.Image*)
        [[ " \${STUB_INSPECT_IMAGE_FAIL:-} " != *" \$4 "* ]] || exit 1
        for pair in \${STUB_OLD_IMAGES:-}; do
          [[ "\${pair%%=*}" != "\$4" ]] || { echo "\${pair#*=}"; exit 0; }
        done
        echo "${olderImage}" ;;
      *) echo 172.18.0.5 ;;
    esac ;;
  run|create)
    code="\${STUB_RUN_EXIT:-0}"
    [[ "\$1" == run ]] || code="\${STUB_CREATE_EXIT:-0}"
    [[ "\$code" == 0 ]] || echo "docker: Error response from daemon: OCI runtime create failed" >&2
    exit "\$code" ;;
  exec) exit "\${STUB_RELOAD:-0}" ;;
  stop) [[ " \${STUB_STOP_FAIL:-} " != *" \$4 "* ]] || exit 1 ;;
  start) [[ -z "\${STUB_START_FAIL:-}" || "\$2" != \$STUB_START_FAIL ]] || exit 1 ;;
  image) [[ "\$2" != rm || " \${STUB_IMAGE_RM_FAIL:-} " != *" \$3 "* ]] || exit 1 ;;
  rm) [[ "\$2" == --force || " \${STUB_RM_FAIL:-} " != *" \$2 "* ]] || exit 1 ;;
  logs) echo "server log line"; exit "\${STUB_LOGS:-0}" ;;
esac`,
  );
  shellStub(
    bin,
    "curl",
    '[[ "${STUB_HEALTHY:-1}" == 1 ]] || exit 7; printf "%s" "${STUB_HTTP_CODE:-200}"',
  );
  for (const [name, body] of Object.entries(stubs)) {
    shellStub(bin, name, body);
  }
  prepare?.(deployRoot);
  const read = (path) => {
    try {
      return readFileSync(join(deployRoot, path), "utf8");
    } catch {
      return undefined;
    }
  };
  const calls = () =>
    readFileSync(join(dir, "calls.log"), "utf8").trim().split("\n");
  const spawnEnv = (env = {}) => ({
    ...baseEnv,
    PATH: `${bin}${delimiter}${baseEnv.PATH}`,
    STUB_DIR: dir,
    DROPLET_ROOT: deployRoot,
    DROPLET_ID_SOURCE: join(dir, "id-source"),
    DROPLET_HEALTH_TIMEOUT: "3",
    ...env,
  });
  return { dir, bin, deployRoot, read, calls, spawnEnv };
}

function runDroplet(
  t,
  args = ["api", dropletImage, "8080", "api.example.com", "http1", "none"],
  { env = {}, ...options } = {},
) {
  const setup = setupDroplet(t, args, options);
  const result = spawnSync("bash", ["-s", "--", ...args], {
    encoding: "utf8",
    input: readFileSync(dropletDeploy, "utf8"),
    env: setup.spawnEnv(env),
  });
  return {
    ...result,
    read: setup.read,
    calls: setup.calls(),
    deployRoot: setup.deployRoot,
  };
}

const commands = (calls) =>
  calls.map((call) => call.split(" ").slice(0, 2).join(" "));

test(
  "droplet deploy starts, checks, routes and then stops the old container",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, { old: ["old1"] });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(commands(result.calls), [
      "docker pull",
      "docker ps",
      "docker ps",
      "docker run",
      "docker inspect",
      "docker inspect",
      "curl --silent",
      "docker exec",
      "docker inspect",
      "docker stop",
      "docker rm",
      "docker image",
      "docker image",
    ]);
    const run = result.calls.find((call) => call.startsWith("docker run"));
    // The replacement joins a unique alias, never the alias of the running
    // container, so the live route cannot reach it before the health check.
    const alias = run.match(/--network-alias (\S+)/)[1];
    assert.equal(alias, `api-${dropletDeploymentUuid.replaceAll("-", "")}`);
    assert.ok(!/--network-alias api /.test(run), run);
    for (const part of [
      `--network delino --network-alias ${alias}`,
      "--label io.delino.service=api",
      "--restart unless-stopped",
      "/srv/env/api.env",
      "--env ADDR=:8080",
      dropletImage,
    ]) {
      assert.ok(run.includes(part), `${part} in ${run}`);
    }
    assert.match(
      result.calls.find((call) => call.startsWith("curl")),
      /http:\/\/172\.18\.0\.5:8080\/healthz/,
    );
    assert.match(
      result.calls.find((call) => call.startsWith("docker exec")),
      /^docker exec caddy caddy reload --config \/etc\/caddy\/Caddyfile --adapter caddyfile$/,
    );
    assert.match(
      result.calls.find((call) => call.startsWith("docker stop")),
      / old1$/,
    );
    const route = result.read("caddy/sites/api.caddy");
    assert.match(route, /^api\.example\.com \{$/m);
    assert.equal(route.includes(`reverse_proxy ${alias}:8080 {`), true, route);
    assert.ok(!/reverse_proxy api:/.test(route), route);
    assert.equal(result.read("state/api.current"), `${dropletImage}\n`);
  },
);

test(
  "droplet deploy switches the route to the new alias on every deployment",
  { skip },
  (t) => {
    const first = runDroplet(t);
    assert.equal(first.status, 0, first.stderr);
    const route = first.read("caddy/sites/api.caddy");
    // A second run of the same image, in a fresh root with the route of the
    // first run, still reloads the proxy: every deployment has its own alias.
    const second = runDroplet(t, undefined, { old: ["old1"], route });
    assert.equal(second.status, 0, second.stderr);
    assert.ok(second.calls.some((call) => call.startsWith("docker exec")));
    assert.equal(second.read("state/api.current"), `${dropletImage}\n`);
  },
);

test(
  "droplet deploy keeps the alias of a long service name a valid DNS label",
  { skip },
  (t) => {
    // The service name may have 63 characters, and cutting the name can leave a
    // trailing hyphen.
    const services = [
      "a".repeat(63),
      `${"b".repeat(34)}-${"c".repeat(28)}`,
      "d".repeat(36),
      "api",
    ];
    for (const service of services) {
      const result = runDroplet(
        t,
        [service, dropletImage, "8080", "api.example.com", "http1", "none"],
        { old: ["old1"] },
      );
      assert.equal(result.status, 0, `${service}: ${result.stderr}`);
      const run = result.calls.find((call) => call.startsWith("docker run"));
      const alias = run.match(/--network-alias (\S+)/)[1];
      assert.match(alias, /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/, service);
      assert.ok(
        alias.endsWith(`-${dropletDeploymentUuid.replaceAll("-", "")}`),
      );
      assert.ok(!alias.includes("--"), alias);
      assert.ok(alias.startsWith(service.slice(0, 30).replace(/-+$/, "")));
      assert.ok(
        result
          .read(`caddy/sites/${service}.caddy`)
          .includes(`reverse_proxy ${alias}:8080 {`),
      );
    }
  },
);

test(
  "droplet deploy keeps the previous image for a rollback",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, {
      state: `registry.digitalocean.com/delino/api@sha256:${"0".repeat(64)}\n`,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.read("state/api.current"), `${dropletImage}\n`);
    assert.equal(
      result.read("state/api.previous"),
      `registry.digitalocean.com/delino/api@sha256:${"0".repeat(64)}\n`,
    );
  },
);

test(
  "droplet deploy proxies h2c to a server with the h2c upstream protocol",
  { skip },
  (t) => {
    const http1 = runDroplet(t);
    assert.equal(http1.status, 0, http1.stderr);
    assert.ok(!http1.read("caddy/sites/api.caddy").includes("transport"));
    const result = runDroplet(t, [
      "api",
      dropletImage,
      "8080",
      "api.example.com",
      "h2c",
      "none",
    ]);
    assert.equal(result.status, 0, result.stderr);
    const run = result.calls.find((call) => call.startsWith("docker run"));
    const alias = run.match(/--network-alias (\S+)/)[1];
    assert.equal(
      result.read("caddy/sites/api.caddy"),
      [
        "# Written by scripts/ci/droplet-deploy.sh for api. Do not edit.",
        "api.example.com {",
        `\treverse_proxy ${alias}:8080 {`,
        "\t\tlb_try_duration 10s",
        "\t\ttransport http {",
        "\t\t\tversions h2c",
        "\t\t}",
        "\t}",
        "}",
        "",
      ].join("\n"),
    );
  },
);

const udpArgs = [
  "api",
  dropletImage,
  "8080",
  "api.example.com",
  "h2c",
  "3478,5349",
];

test(
  "droplet deploy stops the old container before it starts a replacement with UDP ports",
  { skip },
  (t) => {
    const result = runDroplet(t, udpArgs, { old: ["old1"] });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(commands(result.calls), [
      "docker pull",
      "docker ps",
      "docker ps",
      "docker ps",
      "docker create",
      "docker stop",
      "docker start",
      "docker inspect",
      "docker inspect",
      "curl --silent",
      "docker exec",
      "docker inspect",
      "docker stop",
      "docker rm",
      "docker image",
      "docker image",
    ]);
    const running = result.calls.filter((call) => call.startsWith("docker ps"));
    assert.ok(running[0].includes("--all"), running[0]);
    assert.ok(running[1].includes("--filter name=^/api-"), running[1]);
    assert.ok(!running[2].includes("--all"), running[2]);
    const create = result.calls.find((call) =>
      call.startsWith("docker create"),
    );
    for (const part of [
      "--publish 3478:3478/udp --publish 5349:5349/udp",
      "--label io.delino.service=api",
      "--restart unless-stopped",
      "--env ADDR=:8080",
      dropletImage,
    ]) {
      assert.ok(create.includes(part), `${part} in ${create}`);
    }
    const name = create.match(/--name (\S+)/)[1];
    assert.match(
      result.calls.find((call) => call.startsWith("docker stop")),
      / old1$/,
    );
    assert.equal(
      result.calls.find((call) => call.startsWith("docker start")),
      `docker start ${name}`,
    );
    assert.equal(result.read("state/api.current"), `${dropletImage}\n`);
  },
);

test(
  "droplet deploy starts the old container again when a replacement with UDP ports fails",
  { skip },
  (t) => {
    const route = "# old route\n";
    for (const [label, env, message] of [
      [
        "health",
        { STUB_HEALTHY: "0" },
        /did not start or did not pass the health check; the previous containers were started again/,
      ],
      [
        "start",
        { STUB_START_FAIL: "api-*" },
        /did not start or did not pass the health check/,
      ],
      [
        "route",
        { STUB_RELOAD: "1" },
        /route of api was not switched; the previous containers were started again/,
      ],
    ]) {
      const result = runDroplet(t, udpArgs, {
        old: ["old1"],
        route,
        state: `${olderImage}\n`,
        env,
      });
      assert.equal(result.status, 1, label);
      assert.match(result.stderr, message, label);
      const tail = result.calls.slice(-2);
      assert.match(tail[0], /^docker rm --force api-/, label);
      assert.equal(tail[1], "docker start old1", label);
      assert.equal(result.read("caddy/sites/api.caddy"), route, label);
      assert.equal(result.read("state/api.current"), `${olderImage}\n`, label);
    }
    const stopFailed = runDroplet(t, udpArgs, {
      old: ["old1"],
      env: { STUB_STOP_FAIL: "old1" },
    });
    assert.equal(stopFailed.status, 1);
    assert.match(
      stopFailed.stderr,
      /docker stop failed for the previous container old1/,
    );
    assert.deepEqual(commands(stopFailed.calls).slice(-3), [
      "docker stop",
      "docker rm",
      "docker start",
    ]);
    assert.equal(stopFailed.calls.at(-1), "docker start old1");
    assert.ok(
      !stopFailed.calls.some((call) => /^docker start api-/.test(call)),
    );
    // A previous container that does not start again is named.
    const restartFailed = runDroplet(t, udpArgs, {
      old: ["old1"],
      env: { STUB_HEALTHY: "0", STUB_START_FAIL: "old1" },
    });
    assert.equal(restartFailed.status, 1);
    assert.match(
      restartFailed.stderr,
      /docker start failed for the previous container old1/,
    );
  },
);

// A signal that ends a deployment with UDP ports between the first stop and
// the route switch must leave the service as it was before the deployment.
// The `curl` stub blocks the health check (STUB_BLOCK) so that the signal
// arrives while the replacement waits for it; Bash runs its trap when that
// foreground command returns, so the test releases the check after the signal.
test(
  "droplet deploy starts the old container again when a signal ends a replacement with UDP ports before the route switch",
  { skip },
  async (t) => {
    const route = "# old route\n";
    for (const [signal, status] of [
      ["SIGTERM", 143],
      ["SIGINT", 130],
      ["SIGHUP", 129],
    ]) {
      const setup = setupDroplet(t, udpArgs, {
        old: ["old1", "old2"],
        route,
        state: `${olderImage}\n`,
        stubs: { curl: blockingCurl },
      });
      const run = startDroplet(t, setup, udpArgs, { STUB_BLOCK: "1" });
      await waitFor(
        () => setup.calls().some((call) => call.startsWith("curl")),
        `${signal}: the health check of the replacement`,
      );
      run.child.kill(signal);
      writeFileSync(join(setup.dir, "release"), "");
      const result = await run.done;
      assert.equal(result.status, status, `${signal}: ${result.stderr}`);
      assert.match(
        result.stderr,
        new RegExp(`received ${signal} before the route`),
        signal,
      );
      const calls = setup.calls();
      assert.deepEqual(calls.slice(-3), [
        calls.at(-3),
        "docker start old1",
        "docker start old2",
      ]);
      assert.match(calls.at(-3), /^docker rm --force api-/, signal);
      assert.ok(!calls.some((call) => call.startsWith("docker exec")), signal);
      assert.equal(setup.read("caddy/sites/api.caddy"), route, signal);
      assert.equal(setup.read("state/api.current"), `${olderImage}\n`, signal);
    }
  },
);

test(
  "droplet deploy names a previous container that does not start again after a signal",
  { skip },
  async (t) => {
    const setup = setupDroplet(t, udpArgs, {
      old: ["old1"],
      stubs: { curl: blockingCurl },
    });
    const run = startDroplet(t, setup, udpArgs, {
      STUB_BLOCK: "1",
      STUB_START_FAIL: "old1",
    });
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("curl")),
      "the health check of the replacement",
    );
    run.child.kill("SIGTERM");
    writeFileSync(join(setup.dir, "release"), "");
    const result = await run.done;
    assert.equal(result.status, 143, result.stderr);
    assert.match(
      result.stderr,
      /docker start failed for the previous container old1; old1 does not serve/,
    );
  },
);

test(
  "droplet deploy keeps the replacement when a signal arrives during the route switch",
  { skip },
  async (t) => {
    const route = "# old route\n";
    const setup = setupDroplet(t, udpArgs, {
      old: ["old1"],
      route,
      state: `${olderImage}\n`,
    });
    const run = startDroplet(t, setup, udpArgs, {
      STUB_BLOCK_ON: "exec *",
    });
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("docker exec")),
      "the proxy reload",
    );
    run.child.kill("SIGTERM");
    writeFileSync(join(setup.dir, "release-docker"), "");
    const result = await run.done;
    assert.equal(result.status, 143, result.stderr);
    assert.match(result.stderr, /received SIGTERM during the route switch/);
    const calls = setup.calls();
    // The route names the replacement, which stays; the previous containers
    // stay stopped and the state files name the replacement.
    assert.notEqual(setup.read("caddy/sites/api.caddy"), route);
    assert.equal(setup.read("state/api.current"), `${dropletImage}\n`);
    assert.equal(setup.read("state/api.previous"), `${olderImage}\n`);
    assert.ok(!calls.some((call) => call.startsWith("docker rm --force")));
    assert.ok(!calls.some((call) => call === "docker start old1"));
  },
);

test(
  "droplet deploy restores the previous containers when a signal arrives during a failed route switch",
  { skip },
  async (t) => {
    const route = "# old route\n";
    const setup = setupDroplet(t, udpArgs, {
      old: ["old1"],
      route,
      state: `${olderImage}\n`,
    });
    const run = startDroplet(t, setup, udpArgs, {
      STUB_BLOCK_ON: "exec *",
      STUB_RELOAD: "1",
    });
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("docker exec")),
      "the proxy reload",
    );
    run.child.kill("SIGTERM");
    writeFileSync(join(setup.dir, "release-docker"), "");
    const result = await run.done;
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /previous containers were started again/);
    assert.equal(setup.calls().at(-1), "docker start old1");
    assert.equal(setup.read("caddy/sites/api.caddy"), route);
    assert.equal(setup.read("state/api.current"), `${olderImage}\n`);
  },
);

test(
  "droplet deploy does not start the old container again when a signal arrives after the route switch",
  { skip },
  async (t) => {
    const setup = setupDroplet(t, udpArgs, {
      old: ["old1"],
      state: `${olderImage}\n`,
    });
    // `docker rm old1` is the cleanup after the route switch.
    const run = startDroplet(t, setup, udpArgs, { STUB_BLOCK_ON: "rm old1" });
    await waitFor(
      () => setup.calls().some((call) => call === "docker rm old1"),
      "the cleanup of the previous container",
    );
    run.child.kill("SIGTERM");
    // Without a trap the signal ends the script at once, but the blocked stub
    // keeps the output pipes open, so the stub is released shortly after the
    // signal. A script that ignored the signal would then finish normally and
    // not end by the signal.
    await sleep(300);
    writeFileSync(join(setup.dir, "release-docker"), "");
    const result = await run.done;
    assert.equal(result.status, null);
    const calls = setup.calls();
    assert.ok(!calls.some((call) => call === "docker start old1"));
    assert.ok(!calls.some((call) => call.startsWith("docker rm --force")));
    assert.equal(setup.read("state/api.current"), `${dropletImage}\n`);
  },
);

const olderImage = `registry.digitalocean.com/delino/api@sha256:${"0".repeat(64)}`;
const oldestImage = `registry.digitalocean.com/delino/api@sha256:${"1".repeat(64)}`;

test(
  "droplet deploy records the image and fails when the cleanup of an old container fails",
  { skip },
  (t) => {
    const cases = [
      {
        name: "stop",
        env: { STUB_STOP_FAIL: "old1" },
        failed: "docker stop old1",
        message: /docker stop failed for the previous container old1/,
      },
      {
        name: "rm",
        env: { STUB_RM_FAIL: "old1" },
        failed: "docker rm old1",
        message: /docker rm failed for the previous container old1/,
      },
    ];
    for (const { name, env, failed, message } of cases) {
      const result = runDroplet(t, undefined, {
        old: ["old1", "old2"],
        state: `${olderImage}\n`,
        previous: `${oldestImage}\n`,
        env,
      });
      assert.equal(result.status, 1, `${name}: ${result.stderr}`);
      assert.match(result.stderr, message, name);
      assert.ok(result.stderr.includes(`(${failed})`), result.stderr);
      assert.match(
        result.stderr,
        /replacement api-\S+ already serves api/,
        name,
      );
      // The route serves the replacement, so the state names it, and the
      // image that served before it is the one to roll back to.
      assert.equal(result.read("state/api.current"), `${dropletImage}\n`, name);
      assert.equal(result.read("state/api.previous"), `${olderImage}\n`, name);
      assert.match(result.read("caddy/sites/api.caddy"), /reverse_proxy api-/);
      // The failure of one container does not skip the next one.
      assert.ok(
        result.calls.some((call) =>
          call.startsWith("docker stop --time 30 old2"),
        ),
        name,
      );
      assert.ok(
        result.calls.some((call) => call === "docker rm old2"),
        name,
      );
      assert.equal(
        result.calls.some((call) => call === "docker rm old1"),
        name === "rm",
        name,
      );
    }
  },
);

const imageCalls = (calls) =>
  calls.filter((call) => call.startsWith("docker image"));

test(
  "droplet deploy removes the image of a removed old container after the removal",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, { old: ["old1"] });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    // The image is read before the container goes, removed after it, and the
    // prune of dangling images still runs last.
    assert.deepEqual(result.calls.slice(-5), [
      "docker inspect --format {{.Config.Image}} old1",
      "docker stop --time 30 old1",
      "docker rm old1",
      `docker image rm ${olderImage}`,
      "docker image prune --force",
    ]);
  },
);

test(
  "droplet deploy removes no image when the old container ran the new image",
  { skip },
  (t) => {
    // A retry of the same image after a failed cleanup.
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      env: { STUB_OLD_IMAGES: `old1=${dropletImage}` },
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(imageCalls(result.calls), ["docker image prune --force"]);
  },
);

test("droplet deploy removes each distinct old image once", { skip }, (t) => {
  const result = runDroplet(t, undefined, {
    old: ["old1", "old2", "old3"],
    env: {
      STUB_OLD_IMAGES: `old1=${olderImage} old2=${oldestImage} old3=${olderImage}`,
    },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  // old3 shares the image of old1, so the image goes after the last
  // container that used it, and only once.
  assert.deepEqual(imageCalls(result.calls), [
    `docker image rm ${olderImage}`,
    `docker image rm ${oldestImage}`,
    "docker image prune --force",
  ]);
  const lastRm = result.calls.lastIndexOf("docker rm old3");
  assert.ok(
    lastRm > 0 &&
      lastRm < result.calls.indexOf(`docker image rm ${olderImage}`),
  );
});

test(
  "droplet deploy still succeeds and prunes when the removal of an old image fails",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      state: `${olderImage}\n`,
      env: { STUB_IMAGE_RM_FAIL: olderImage },
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.ok(
      result.stderr.includes(
        `docker image rm failed for the previous image ${olderImage}`,
      ),
      result.stderr,
    );
    assert.ok(!result.stderr.includes("::error::"), result.stderr);
    assert.equal(result.calls.at(-1), "docker image prune --force");
    assert.equal(result.read("state/api.current"), `${dropletImage}\n`);
    assert.match(
      result.stdout,
      /api runs .* behind https:\/\/api\.example\.com/,
    );
  },
);

test(
  "droplet deploy keeps the image of an old container that it could not remove or inspect",
  { skip },
  (t) => {
    // old1 stays (its removal fails), so its image stays; old2 shares that
    // image. old3 has an unreadable image and old4 has its own image.
    const result = runDroplet(t, undefined, {
      old: ["old1", "old2", "old3", "old4"],
      env: {
        STUB_RM_FAIL: "old1",
        STUB_INSPECT_IMAGE_FAIL: "old3",
        STUB_OLD_IMAGES: `old1=${olderImage} old2=${olderImage} old4=${oldestImage}`,
      },
    });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(
      result.stderr,
      /docker rm failed for the previous container old1/,
    );
    assert.match(
      result.stderr,
      /Could not read the image of the previous container old3/,
    );
    assert.deepEqual(imageCalls(result.calls), [
      `docker image rm ${oldestImage}`,
      "docker image prune --force",
    ]);
    assert.ok(result.calls.includes("docker rm old3"));
  },
);

test(
  "droplet deploy removes the old image of a service with UDP ports",
  { skip },
  (t) => {
    // The previous containers are already stopped when the cleanup starts.
    const result = runDroplet(t, udpArgs, { old: ["old1"] });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.deepEqual(imageCalls(result.calls), [
      `docker image rm ${olderImage}`,
      "docker image prune --force",
    ]);
    assert.ok(
      result.calls.indexOf(`docker image rm ${olderImage}`) >
        result.calls.indexOf("docker rm old1"),
    );
  },
);

test(
  "droplet deploy keeps the previous image after an interrupted state write",
  { skip },
  (t) => {
    // An interrupted earlier attempt left `current` empty or missing while
    // `previous` still names the image that served before it. Recording the
    // new image must not replace that rollback image with an empty file.
    for (const [name, state] of [
      ["empty current", ""],
      ["missing current", undefined],
    ]) {
      const result = runDroplet(t, undefined, {
        state,
        previous: `${olderImage}\n`,
      });
      assert.equal(result.status, 0, `${name}: ${result.stderr}`);
      assert.equal(result.read("state/api.current"), `${dropletImage}\n`, name);
      assert.equal(result.read("state/api.previous"), `${olderImage}\n`, name);
      // The temporary files of the atomic writes do not stay behind.
      assert.deepEqual(
        readdirSync(join(result.deployRoot, "state"))
          .filter((file) => file !== "api.lock" && file !== ".proxy.lock")
          .sort(),
        ["api.current", "api.previous"],
        name,
      );
    }
  },
);

test(
  "droplet deploy keeps the previous image when a retry finishes a failed cleanup",
  { skip },
  (t) => {
    // The first attempt switched the route and recorded the image, but could
    // not stop the old container. The retry deploys the same image again.
    const result = runDroplet(t, undefined, {
      old: ["old1", "old2"],
      state: `${dropletImage}\n`,
      previous: `${olderImage}\n`,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.read("state/api.current"), `${dropletImage}\n`);
    assert.equal(result.read("state/api.previous"), `${olderImage}\n`);
    for (const id of ["old1", "old2"]) {
      assert.ok(
        result.calls.some((call) => call === `docker stop --time 30 ${id}`),
        id,
      );
      assert.ok(
        result.calls.some((call) => call === `docker rm ${id}`),
        id,
      );
    }
  },
);

test(
  "droplet deploy keeps both state files when the health check or the proxy reload fails",
  { skip },
  (t) => {
    for (const env of [{ STUB_HEALTHY: "0" }, { STUB_RELOAD: "1" }]) {
      const result = runDroplet(t, undefined, {
        old: ["old1"],
        state: `${olderImage}\n`,
        previous: `${oldestImage}\n`,
        env,
      });
      const label = JSON.stringify(env);
      assert.equal(result.status, 1, label);
      assert.equal(result.read("state/api.current"), `${olderImage}\n`, label);
      assert.equal(
        result.read("state/api.previous"),
        `${oldestImage}\n`,
        label,
      );
      assert.ok(!result.calls.some((call) => call.startsWith("docker stop")));
    }
  },
);

test(
  "droplet deploy fails before it starts a container when the container list cannot be read",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      env: { STUB_PS_EXIT: "1" },
    });
    assert.equal(result.status, 1);
    assert.match(
      result.stderr,
      /::error::could not list the existing containers/,
    );
    assert.deepEqual(commands(result.calls), ["docker pull", "docker ps"]);
  },
);

test(
  "droplet deploy removes an unhealthy container and keeps the old one",
  { skip },
  (t) => {
    for (const env of [{ STUB_HEALTHY: "0" }, { STUB_STATUS: "exited" }]) {
      const result = runDroplet(t, undefined, { old: ["old1"], env });
      const label = JSON.stringify(env);
      assert.equal(result.status, 1, label);
      assert.match(result.stderr, /did not pass the health check/, label);
      assert.match(result.stdout, /server log line/, label);
      assert.ok(
        result.calls.some((call) => call.startsWith("docker rm --force api-")),
        label,
      );
      assert.ok(
        !result.calls.some((call) => call.startsWith("docker stop")),
        label,
      );
      assert.equal(result.read("caddy/sites/api.caddy"), undefined, label);
      assert.equal(result.read("state/api.current"), undefined, label);
      // The route is not written, so the live route still targets only the
      // current container.
      assert.ok(
        !result.calls.some((call) => call.startsWith("docker exec")),
        label,
      );
    }
  },
);

test(
  "droplet deploy removes the replacement and keeps everything else when docker run fails",
  { skip },
  (t) => {
    const route = "# old route\n";
    const options = {
      old: ["old1"],
      route,
      state: `${olderImage}\n`,
      previous: `${oldestImage}\n`,
    };
    for (const [label, env] of [
      ["run fails", { STUB_RUN_EXIT: "125" }],
      // A failed log read or removal must not hide the start failure.
      [
        "cleanup fails",
        { STUB_RUN_EXIT: "125", STUB_LOGS: "1", STUB_RM_FAIL: "old1" },
      ],
    ]) {
      const result = runDroplet(t, undefined, { ...options, env });
      assert.equal(result.status, 1, `${label}: ${result.stderr}`);
      assert.match(
        result.stderr,
        /::error::api-\d{14}-e{12}-[0-9a-f]{32} did not start; the previous container keeps serving\./,
        label,
      );
      // Docker's own diagnostic stays visible.
      assert.match(result.stderr, /OCI runtime create failed/, label);
      assert.deepEqual(
        commands(result.calls),
        [
          "docker pull",
          "docker ps",
          "docker ps",
          "docker run",
          "docker logs",
          "docker rm",
        ],
        label,
      );
      assert.match(result.calls.at(-1), /^docker rm --force api-/, label);
      assert.equal(result.read("caddy/sites/api.caddy"), route, label);
      assert.equal(result.read("state/api.current"), `${olderImage}\n`, label);
      assert.equal(
        result.read("state/api.previous"),
        `${oldestImage}\n`,
        label,
      );
    }
  },
);

test(
  "droplet deploy removes the replacement and stops nothing when docker create fails",
  { skip },
  (t) => {
    const route = "# old route\n";
    const result = runDroplet(t, udpArgs, {
      old: ["old1"],
      route,
      state: `${olderImage}\n`,
      env: { STUB_CREATE_EXIT: "125" },
    });
    assert.equal(result.status, 1, result.stderr);
    assert.match(
      result.stderr,
      /::error::api-\d{14}-e{12}-[0-9a-f]{32} was not created; the previous containers keep serving\./,
    );
    assert.deepEqual(commands(result.calls), [
      "docker pull",
      "docker ps",
      "docker ps",
      "docker ps",
      "docker create",
      "docker logs",
      "docker rm",
    ]);
    assert.match(result.calls.at(-1), /^docker rm --force api-/);
    assert.equal(result.read("caddy/sites/api.caddy"), route);
    assert.equal(result.read("state/api.current"), `${olderImage}\n`);
  },
);

test("droplet deploy accepts only a 2xx health status", { skip }, (t) => {
  for (const code of ["200", "204"]) {
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      env: { STUB_HTTP_CODE: code },
    });
    assert.equal(result.status, 0, code + result.stdout + result.stderr);
    assert.ok(result.calls.some((call) => call.startsWith("docker stop")));
  }
  for (const code of ["302", "307", "404", "500", "000"]) {
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      env: { STUB_HTTP_CODE: code },
    });
    assert.equal(result.status, 1, code);
    assert.match(result.stderr, /did not pass the health check/, code);
    assert.ok(
      result.calls.some((call) => call.startsWith("docker rm --force api-")),
      code,
    );
    assert.ok(
      !result.calls.some((call) => call.startsWith("docker stop")),
      code,
    );
    assert.equal(result.read("caddy/sites/api.caddy"), undefined, code);
    assert.equal(result.read("state/api.current"), undefined, code);
  }
});

test(
  "droplet deploy restores the route when the proxy rejects it",
  { skip },
  (t) => {
    const previous = "# old route\nold.example.com {\n}\n";
    const result = runDroplet(t, undefined, {
      old: ["old1"],
      route: previous,
      env: { STUB_RELOAD: "1" },
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /proxy rejected the route/);
    assert.match(result.stderr, /route of api was not switched/);
    assert.equal(result.read("caddy/sites/api.caddy"), previous);
    assert.equal(result.read("caddy/sites/api.caddy.new"), undefined);
    assert.ok(
      result.calls.some((call) => call.startsWith("docker rm --force api-")),
    );
    assert.ok(!result.calls.some((call) => call.startsWith("docker stop")));
    // Without a previous route, the new route file is removed.
    const first = runDroplet(t, undefined, { env: { STUB_RELOAD: "1" } });
    assert.equal(first.status, 1);
    assert.equal(first.read("caddy/sites/api.caddy"), undefined);
  },
);

test(
  "droplet deploy fails and keeps the route when a route step fails",
  { skip },
  (t) => {
    const previous = "# old route\nold.example.com {\n}\n";
    const current = `registry.digitalocean.com/delino/api@sha256:${"0".repeat(64)}\n`;
    const cases = [
      {
        name: "directory",
        message: /could not create the directory of the route file/,
        stubs: {
          mkdir: `case "\${@: -1}" in */caddy/sites) exit 1 ;; esac\nexec /bin/mkdir "$@"`,
        },
      },
      {
        name: "backup",
        message: /could not back up the route file/,
        stubs: { cp: "exit 1" },
      },
      {
        name: "write",
        message: /could not write the new route/,
        // A directory in the place of the temporary file makes the write fail
        // also when the tests run as root.
        prepare: (root) => mkdirSync(join(root, "caddy/sites/api.caddy.new")),
      },
      {
        name: "install",
        message: /could not install the new route/,
        stubs: { mv: "exit 1" },
      },
    ];
    for (const { name, message, stubs, prepare } of cases) {
      const result = runDroplet(t, undefined, {
        old: ["old1"],
        route: previous,
        state: current,
        stubs,
        prepare,
      });
      assert.equal(result.status, 1, `${name}: ${result.stderr}`);
      assert.match(result.stderr, message, name);
      assert.match(result.stderr, /route of api was not switched/, name);
      // The proxy still has the original route and nothing reloaded it.
      assert.equal(result.read("caddy/sites/api.caddy"), previous, name);
      assert.ok(
        !result.calls.some((call) => call.startsWith("docker exec")),
        name,
      );
      // The old container keeps serving and the replacement is removed.
      assert.ok(
        !result.calls.some((call) => call.startsWith("docker stop")),
        name,
      );
      assert.ok(
        result.calls.some((call) => call.startsWith("docker rm --force api-")),
        name,
      );
      assert.match(result.stdout, /Last log lines of api-/, name);
      assert.equal(result.read("state/api.current"), current, name);
      assert.equal(result.read("state/api.previous"), undefined, name);
    }
  },
);

test(
  "droplet deploy prints the replacement logs before it removes it when the route fails",
  { skip },
  (t) => {
    // A failing `docker logs` must neither stop the cleanup nor change the
    // verdict.
    for (const logs of ["0", "1"]) {
      const result = runDroplet(t, undefined, {
        old: ["old1"],
        env: { STUB_RELOAD: "1", STUB_LOGS: logs },
      });
      assert.equal(result.status, 1, logs);
      assert.match(result.stdout, /Last log lines of api-/, logs);
      assert.match(result.stdout, /server log line/, logs);
      const logsAt = result.calls.findIndex((call) =>
        /^docker logs --tail 200 api-/.test(call),
      );
      const removeAt = result.calls.findIndex((call) =>
        call.startsWith("docker rm --force api-"),
      );
      assert.ok(logsAt >= 0, logs);
      assert.ok(removeAt > logsAt, `${logs}: logs before rm`);
      assert.ok(!result.calls.some((call) => call.startsWith("docker stop")));
    }
  },
);

// Starts droplet-deploy.sh without waiting for it, for the fixtures that run
// several deployments at once. The promise resolves with the exit status and
// the output; the process and its process group are killed when the test ends,
// so that a blocked stub does not keep the test runner alive after a failure.
function startDroplet(t, setup, args, env = {}) {
  const { child, done } = spawnProcessGroup(t, "bash", ["-s", "--", ...args], {
    env: setup.spawnEnv(env),
  });
  child.stdin.end(readFileSync(dropletDeploy, "utf8"));
  return { child, done };
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor(condition, what) {
  for (let waited = 0; waited < 15000; waited += 50) {
    if (condition()) return;
    await sleep(50);
  }
  assert.fail(`timed out waiting for ${what}`);
}

// The `curl` of a run with STUB_BLOCK set does not answer until `release`
// exists, so that the run holds its deployment lock for as long as the test
// needs.
const blockingCurl = `if [[ -n "\${STUB_BLOCK:-}" ]]; then
  while [[ ! -e "$STUB_DIR/release" ]]; do sleep 0.05; done
fi
printf 200`;

const concurrentImage = `registry.digitalocean.com/delino/api@sha256:${"f".repeat(64)}`;

test(
  "droplet deploy serializes concurrent deployments of one service",
  { skip },
  async (t) => {
    const setup = setupDroplet(t, undefined, {
      old: ["old1"],
      stubs: { curl: blockingCurl },
    });
    const first = startDroplet(
      t,
      setup,
      ["api", dropletImage, "8080", "api.example.com", "http1", "none"],
      { STUB_BLOCK: "1" },
    );
    // The first run holds the lock once it waits for its health check.
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("curl")),
      "the first deployment to reach its health check",
    );
    // The second run is the direct rollback of the documentation: another
    // image of the same service while the first deployment still runs.
    const second = startDroplet(t, setup, [
      "api",
      concurrentImage,
      "8080",
      "api.example.com",
      "http1",
      "none",
    ]);
    await sleep(750);
    assert.deepEqual(
      setup.calls().filter((call) => call.includes(concurrentImage)),
      [],
      "the second deployment must wait for the lock",
    );
    assert.equal(setup.read("state/api.current"), undefined);
    writeFileSync(join(setup.dir, "release"), "");
    const [firstResult, secondResult] = await Promise.all([
      first.done,
      second.done,
    ]);
    assert.equal(firstResult.status, 0, firstResult.stderr);
    assert.equal(secondResult.status, 0, secondResult.stderr);
    const calls = setup.calls();
    const secondStart = calls.findIndex((call) =>
      call.includes(concurrentImage),
    );
    const firstEnd = calls.findIndex((call) =>
      call.startsWith("docker image prune"),
    );
    // The first run ends (its last call is `docker image prune`) before the
    // second run makes its first call, so the second run never removes a
    // container or rewrites the route files in the middle of the first run.
    assert.ok(firstEnd >= 0 && secondStart > firstEnd, calls.join("\n"));
    assert.equal(
      calls.filter((call) => call.startsWith("docker image prune")).length,
      2,
    );
    assert.equal(setup.read("state/api.current"), `${concurrentImage}\n`);
    assert.equal(setup.read("state/api.previous"), `${dropletImage}\n`);
    assert.equal(setup.read("caddy/sites/api.caddy.new"), undefined);
    assert.equal(
      setup.read("caddy/sites/api.caddy.previous") !== undefined,
      true,
    );
  },
);

test(
  "droplet deploy gives up without a change when the lock stays held",
  { skip },
  async (t) => {
    const setup = setupDroplet(t, undefined, {
      old: ["old1"],
      stubs: { curl: blockingCurl },
    });
    const first = startDroplet(
      t,
      setup,
      ["api", dropletImage, "8080", "api.example.com", "http1", "none"],
      { STUB_BLOCK: "1" },
    );
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("curl")),
      "the first deployment to reach its health check",
    );
    const before = setup.calls();
    const second = await startDroplet(
      t,
      setup,
      ["api", concurrentImage, "8080", "api.example.com", "http1", "none"],
      { DROPLET_LOCK_TIMEOUT: "1" },
    ).done;
    assert.equal(second.status, 1);
    assert.match(
      second.stderr,
      /another deployment of api held .*api\.lock for 1 seconds; nothing was changed/,
    );
    assert.deepEqual(setup.calls(), before);
    writeFileSync(join(setup.dir, "release"), "");
    assert.equal((await first.done).status, 0);
    assert.equal(setup.read("state/api.current"), `${dropletImage}\n`);
  },
);

test(
  "droplet deploy does not make deployments of different services wait",
  { skip },
  async (t) => {
    const setup = setupDroplet(t, undefined, {
      old: ["old1"],
      stubs: { curl: blockingCurl },
      prepare: (deployRoot) =>
        writeFileSync(
          join(deployRoot, "env", "worker.env"),
          "LOG_LEVEL=info\n",
        ),
    });
    const first = startDroplet(
      t,
      setup,
      ["api", dropletImage, "8080", "api.example.com", "http1", "none"],
      { STUB_BLOCK: "1" },
    );
    await waitFor(
      () => setup.calls().some((call) => call.startsWith("curl")),
      "the first deployment to reach its health check",
    );
    const other = await startDroplet(t, setup, [
      "worker",
      concurrentImage,
      "9000",
      "worker.example.com",
      "http1",
      "none",
    ]).done;
    assert.equal(other.status, 0, other.stderr);
    assert.equal(setup.read("state/worker.current"), `${concurrentImage}\n`);
    writeFileSync(join(setup.dir, "release"), "");
    assert.equal((await first.done).status, 0);
  },
);

test(
  "droplet deploy fails before any change when the lock file cannot be opened",
  { skip },
  (t) => {
    const result = runDroplet(t, undefined, {
      // A directory in the place of the lock file.
      prepare: (deployRoot) => {
        mkdirSync(join(deployRoot, "state", "api.lock"), { recursive: true });
      },
    });
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /could not open the lock file .*api\.lock/);
    assert.deepEqual(result.calls, [""]);
  },
);

test(
  "droplet deploy rejects bad arguments and configuration before any change",
  { skip },
  (t) => {
    const image = dropletImage;
    for (const [args, options, message] of [
      [["api", image, "8080"], {}, /usage: droplet-deploy\.sh/],
      [
        ["api", image, "8080", "api.example.com"],
        {},
        /usage: droplet-deploy\.sh/,
      ],
      [
        ["api", image, "8080", "api.example.com", "h2", "none"],
        {},
        /upstream protocol/,
      ],
      [["api", image, "8080", "api.example.com", "http1", ""], {}, /UDP ports/],
      [
        ["api", image, "8080", "api.example.com", "http1", "3478,"],
        {},
        /UDP ports/,
      ],
      [
        ["api", image, "8080", "api.example.com", "http1", "443"],
        {},
        /UDP port '443'/,
      ],
      [
        ["api", image, "8080", "api.example.com", "http1", "3478,70000"],
        {},
        /UDP port '70000'/,
      ],
      [
        ["api", image, "8080", "api.example.com", "http1", "3478,3478"],
        {},
        /UDP port 3478 is listed twice/,
      ],
      [
        ["API", image, "8080", "api.example.com", "http1", "none"],
        {},
        /service name/,
      ],
      [
        ["api;x", image, "8080", "api.example.com", "http1", "none"],
        {},
        /service name/,
      ],
      [
        [
          "api",
          "registry/api:latest",
          "8080",
          "api.example.com",
          "http1",
          "none",
        ],
        {},
        /pinned by digest/,
      ],
      [["api", image, "80", "api.example.com", "http1", "none"], {}, /port/],
      [["api", image, "70000", "api.example.com", "http1", "none"], {}, /port/],
      [["api", image, "8080", "localhost", "http1", "none"], {}, /host name/],
      [
        ["api", image, "8080", "a.example.com;x", "http1", "none"],
        {},
        /host name/,
      ],
      [
        [
          "api",
          image,
          "8080",
          `${"a".repeat(64)}.example.com`,
          "http1",
          "none",
        ],
        {},
        /host name/,
      ],
      [
        ["api", image, "8080", `api.${"a".repeat(64)}.com`, "http1", "none"],
        {},
        /host name/,
      ],
      [
        ["api", image, "8080", "api.example.com", "http1", "none"],
        { envFile: null },
        /api\.env does not exist/,
      ],
      [
        ["api", image, "8080", "api.example.com", "http1", "none"],
        { envFile: "ADDR=:1\n" },
        /sets ADDR/,
      ],
    ]) {
      const result = runDroplet(t, args, options);
      const label = JSON.stringify([args, options]);
      assert.notEqual(result.status, 0, label);
      assert.match(result.stderr, message, label);
      assert.deepEqual(result.calls, [""], label);
    }
    // A 63-character label is the longest valid one and passes validation.
    const valid = runDroplet(
      t,
      ["api", image, "8080", `${"a".repeat(63)}.example.com`, "http1", "none"],
      {},
    );
    assert.doesNotMatch(valid.stderr, /host name/);
  },
);

// --- start-caddy.sh --------------------------------------------------------

const startCaddy = join(root, "scripts/droplet/start-caddy.sh");
const caddyImage = `caddy:2.11.9@sha256:${"c".repeat(64)}`;

// Runs start-caddy.sh against a stubbed `docker`. Stub behavior:
//   STUB_INSPECT  the output of `docker inspect` for the `caddy` container
//                 (`<image> <running>`); `docker inspect` fails when unset,
//                 as for a missing container.
//   STUB_FAIL     the `docker` subcommand that prints a daemon error and
//                 fails (for example `start`).
//   STUB_NO_IMAGE set when the pinned image is not local: `docker image
//                 inspect` fails, as for an image that was never pulled.
//   STUB_STATE    the container state that the readiness check reads
//                 (default `running`); STUB_STATE_GONE makes that read fail.
//   STUB_WGET_FAILS  the number of readiness probes that fail before one
//                 succeeds, or `always`.
// The readiness wait runs with a short deadline and interval.
function runCaddy(
  t,
  {
    inspect,
    fail,
    noImage,
    args,
    state,
    gone,
    wgetFails,
    simulateClock,
    env: extra,
  } = {},
) {
  const dir = scratch(t);
  const bin = join(dir, "bin");
  mkdirSync(bin);
  writeFileSync(join(dir, "calls.log"), "");
  shellStub(
    bin,
    "docker",
    `if [[ "\${STUB_FAIL:-}" == "$1" ]]; then
  echo "Error response from daemon: stub $1 failed" >&2
  exit 1
fi
case "$1" in
  inspect)
    if [[ "$3" == "{{.State.Status}}" ]]; then
      [[ -z "\${STUB_STATE_GONE:-}" ]] || { echo "Error: No such object: caddy" >&2; exit 1; }
      echo "\${STUB_STATE:-running}"
    else
      [[ -n "\${STUB_INSPECT:-}" ]] && echo "$STUB_INSPECT" || exit 1
    fi
    ;;
  image)
    if [[ "$2" == "inspect" && -n "\${STUB_NO_IMAGE:-}" ]]; then exit 1; fi
    ;;
  exec)
    if [[ "$*" == *wget* ]]; then
      count="$(cat "$STUB_DIR/wget.count" 2> /dev/null || echo 0)"
      echo $((count + 1)) > "$STUB_DIR/wget.count"
      if [[ "\${STUB_WGET_FAILS:-0}" == always ]] || (( count < \${STUB_WGET_FAILS:-0} )); then
        echo "wget: can't connect to remote host" >&2
        exit 1
      fi
    fi
    ;;
esac`,
  );
  const env = { ...baseEnv, PATH: `${bin}${delimiter}${baseEnv.PATH}` };
  env.STUB_DIR = dir;
  if (inspect !== undefined) env.STUB_INSPECT = inspect;
  if (fail !== undefined) env.STUB_FAIL = fail;
  if (noImage) env.STUB_NO_IMAGE = "1";
  if (state !== undefined) env.STUB_STATE = state;
  if (gone) env.STUB_STATE_GONE = "1";
  if (wgetFails !== undefined) env.STUB_WGET_FAILS = String(wgetFails);
  env.START_CADDY_READY_TIMEOUT = "2";
  env.START_CADDY_READY_INTERVAL = "0.1";
  if (simulateClock) {
    // Replace Bash's wall clock with a counter advanced by each sleep.
    env.BASH_ENV = join(dir, "clock.bash");
    writeFileSync(
      env.BASH_ENV,
      "unset SECONDS\nSECONDS=0\nsleep() { SECONDS=$((SECONDS + $1)); }\n",
    );
    env.START_CADDY_READY_INTERVAL = "1";
  }
  Object.assign(env, extra);
  const result = spawnSync(
    "bash",
    [startCaddy, ...(args ?? [caddyImage, "/srv/delino", "delino"])],
    { encoding: "utf8", env },
  );
  return {
    ...result,
    calls: readFileSync(join(dir, "calls.log"), "utf8")
      .trim()
      .split("\n")
      .filter(Boolean),
  };
}

const caddyRun = `docker run --detach --name caddy --network delino --restart unless-stopped --publish 80:80 --publish 443:443 --publish 443:443/udp --volume /srv/delino/caddy:/etc/caddy:ro --volume caddy-data:/data --volume caddy-config:/config ${caddyImage}`;
const caddyInspect =
  "docker inspect --format {{.Config.Image}} {{.State.Running}} caddy";
const caddyState = "docker inspect --format {{.State.Status}} caddy";
const caddyProbe =
  "docker exec caddy wget -q -T 2 -O /dev/null http://127.0.0.1:2019/config/";
const caddyImageInspect = `docker image inspect ${caddyImage}`;
const caddyPull = `docker pull ${caddyImage}`;
const caddyReload =
  "docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile";

test("start-caddy creates a missing container", { skip }, (t) => {
  const result = runCaddy(t);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    caddyInspect,
    caddyImageInspect,
    "docker rm --force caddy",
    caddyRun,
    caddyState,
    caddyProbe,
  ]);
});

test("start-caddy reloads a running container", { skip }, (t) => {
  const result = runCaddy(t, { inspect: `${caddyImage} true` });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [caddyInspect, caddyReload]);
});

test("start-caddy starts a stopped container", { skip }, (t) => {
  const result = runCaddy(t, { inspect: `${caddyImage} false` });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    caddyInspect,
    "docker start caddy",
    caddyState,
    caddyProbe,
  ]);
});

test(
  "start-caddy probes after create and start with padded timeouts",
  { skip },
  (t) => {
    const createCalls = [
      caddyImageInspect,
      "docker rm --force caddy",
      caddyRun,
    ];
    const startCalls = ["docker start caddy"];
    for (const timeout of ["08", "8", "09", "9"]) {
      for (const [inspect, operationCalls] of [
        [undefined, createCalls],
        [`${caddyImage} false`, startCalls],
      ]) {
        const result = runCaddy(t, {
          inspect,
          env: { START_CADDY_READY_TIMEOUT: timeout },
        });
        const label = `${inspect ?? "missing"} with timeout ${timeout}`;
        assert.equal(result.status, 0, `${label}: ${result.stderr}`);
        assert.deepEqual(
          result.calls,
          [caddyInspect, ...operationCalls, caddyState, caddyProbe],
          label,
        );
      }
    }
  },
);

test("start-caddy recreates a container with another image", { skip }, (t) => {
  for (const running of ["true", "false"]) {
    const result = runCaddy(t, {
      inspect: `caddy:2.10.0@sha256:${"a".repeat(64)} ${running}`,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(
      result.calls,
      [
        caddyInspect,
        caddyImageInspect,
        "docker rm --force caddy",
        caddyRun,
        caddyState,
        caddyProbe,
      ],
      running,
    );
  }
});

test(
  "start-caddy pulls a missing image before it removes the container",
  {
    skip,
  },
  (t) => {
    for (const inspect of [
      undefined,
      `caddy:2.10.0@sha256:${"a".repeat(64)} true`,
    ]) {
      const result = runCaddy(t, { inspect, noImage: true });
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(
        result.calls,
        [
          caddyInspect,
          caddyImageInspect,
          caddyPull,
          "docker rm --force caddy",
          caddyRun,
          caddyState,
          caddyProbe,
        ],
        String(inspect),
      );
    }
  },
);

test(
  "start-caddy keeps the old container when the pull fails",
  { skip },
  (t) => {
    for (const inspect of [
      undefined,
      `caddy:2.10.0@sha256:${"a".repeat(64)} true`,
    ]) {
      const result = runCaddy(t, { inspect, noImage: true, fail: "pull" });
      const label = String(inspect);
      assert.notEqual(result.status, 0, label);
      assert.match(
        result.stderr,
        /Error response from daemon: stub pull failed/,
        label,
      );
      assert.deepEqual(
        result.calls,
        [caddyInspect, caddyImageInspect, caddyPull],
        label,
      );
    }
  },
);

test("start-caddy needs no pull for a same-image container", { skip }, (t) => {
  for (const running of ["true", "false"]) {
    const result = runCaddy(t, {
      inspect: `${caddyImage} ${running}`,
      noImage: true,
      fail: "pull",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.ok(!result.calls.some((call) => call.startsWith("docker pull")));
  }
});

test("start-caddy fails with the Docker diagnostic", { skip }, (t) => {
  for (const [inspect, fail] of [
    [`${caddyImage} false`, "start"],
    [`${caddyImage} true`, "exec"],
    [undefined, "run"],
    ["caddy:other true", "run"],
  ]) {
    const result = runCaddy(t, { inspect, fail });
    const label = `${inspect} ${fail}`;
    assert.notEqual(result.status, 0, label);
    assert.match(
      result.stderr,
      new RegExp(`Error response from daemon: stub ${fail} failed`),
      label,
    );
  }
});

test("start-caddy rejects bad usage before any change", { skip }, (t) => {
  const result = runCaddy(t, { args: [caddyImage, "/srv/delino"] });
  assert.equal(result.status, 2);
  assert.match(result.stderr, /usage: start-caddy\.sh/);
  assert.deepEqual(result.calls, []);
});

// --- configure-docker-logs.sh ----------------------------------------------

const configureLogs = join(root, "scripts/droplet/configure-docker-logs.sh");

// The configuration and the stub daemon's driver persist across runs.
function logsFixture(
  t,
  { content, readOnlyDirectory, noDirectory, driver = "local" } = {},
) {
  const dir = scratch(t);
  const bin = join(dir, "bin");
  mkdirSync(bin);
  const calls = join(dir, "calls.log");
  writeFileSync(join(dir, "driver"), driver);
  shellStub(
    bin,
    "docker",
    `[[ -z "$DOCKER_CONTEXT" && "$*" == "--host unix:///var/run/docker.sock info --format {{.LoggingDriver}}" ]] || exit 2
driver="$(< "$STUB_DIR/driver")"
[[ -n "$driver" ]] || exit 1
printf '%s\\n' "$driver"`,
  );
  shellStub(
    bin,
    "systemctl",
    `[[ "$*" == "restart docker" ]] || exit 2
if [[ "$STUB_RESTART_STATUS" != 0 ]]; then
  exit "$STUB_RESTART_STATUS"
fi
printf '%s' "$STUB_RESTARTED_DRIVER" > "$STUB_DIR/driver"`,
  );
  const configDirectory = join(dir, "docker");
  if (!noDirectory) mkdirSync(configDirectory);
  const config = join(configDirectory, "daemon.json");
  if (content !== undefined) writeFileSync(config, content, { mode: 0o600 });

  return {
    run({ restartStatus = 0, restartedDriver = "local" } = {}) {
      writeFileSync(calls, "");
      if (readOnlyDirectory) chmodSync(configDirectory, 0o555);
      const result = spawnSync("bash", [configureLogs, config], {
        encoding: "utf8",
        env: {
          ...baseEnv,
          PATH: `${bin}${delimiter}${baseEnv.PATH}`,
          DOCKER_CONTEXT: "remote-context",
          DOCKER_HOST: "tcp://remote.example:2375",
          STUB_DIR: dir,
          STUB_RESTART_STATUS: String(restartStatus),
          STUB_RESTARTED_DRIVER: restartedDriver,
        },
      });
      // Restore the permission now, so the scratch directory can be removed.
      if (readOnlyDirectory) chmodSync(configDirectory, 0o755);
      return {
        ...result,
        config,
        read: () => readFileSync(config, "utf8"),
        leftovers: () =>
          existsSync(configDirectory)
            ? readdirSync(configDirectory).filter(
                (name) => name !== "daemon.json",
              )
            : [],
        calls: readFileSync(calls, "utf8").trim().split("\n").filter(Boolean),
      };
    },
  };
}

const restartDocker = "systemctl restart docker";
const queryLogDriver =
  "docker --host unix:///var/run/docker.sock info --format {{.LoggingDriver}}";

test("configure-docker-logs creates a missing file", { skip }, (t) => {
  const result = logsFixture(t).run();
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.read()), { "log-driver": "local" });
  assert.deepEqual(result.calls, [restartDocker, queryLogDriver]);
  assert.deepEqual(result.leftovers(), []);
});

test(
  "configure-docker-logs keeps the other settings and is idempotent",
  { skip },
  (t) => {
    const original = {
      "data-root": "/mnt/docker",
      "registry-mirrors": ["https://mirror.example.com"],
      "log-driver": "json-file",
      "log-opts": { "max-size": "10m" },
      "default-address-pools": [{ base: "10.10.0.0/16", size: 24 }],
    };
    const fixture = logsFixture(t, {
      content: JSON.stringify(original),
      driver: "json-file",
    });
    const first = fixture.run();
    assert.equal(first.status, 0, first.stderr);
    assert.deepEqual(JSON.parse(first.read()), {
      ...original,
      "log-driver": "local",
    });
    assert.deepEqual(first.calls, [restartDocker, queryLogDriver]);
    assert.equal(statSync(first.config).mode & 0o777, 0o600);
    assert.deepEqual(first.leftovers(), []);

    // A second run on the written file changes nothing and restarts nothing.
    const written = first.read();
    const second = fixture.run();
    assert.equal(second.status, 0, second.stderr);
    assert.equal(second.read(), written);
    assert.deepEqual(second.calls, [queryLogDriver]);
  },
);

test(
  "configure-docker-logs adds the key to an object without it",
  {
    skip,
  },
  (t) => {
    const result = logsFixture(t, {
      content: '{"data-root": "/mnt/docker"}',
    }).run();
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(JSON.parse(result.read()), {
      "data-root": "/mnt/docker",
      "log-driver": "local",
    });
    assert.deepEqual(result.calls, [restartDocker, queryLogDriver]);
  },
);

test(
  "configure-docker-logs leaves a correct file byte for byte",
  { skip },
  (t) => {
    // Unusual formatting, key order and an extra setting must all stay.
    const content = '{"data-root":"/x",   "log-driver":"local"}\n\n';
    const result = logsFixture(t, { content }).run();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.read(), content);
    assert.deepEqual(result.calls, [queryLogDriver]);
  },
);

test(
  "configure-docker-logs applies an unchanged file after an interruption",
  { skip },
  (t) => {
    const content = '{"data-root":"/x",   "log-driver":"local"}\n\n';
    for (const driver of ["json-file", ""]) {
      const fixture = logsFixture(t, { content, driver });
      const first = fixture.run();
      assert.equal(first.status, 0, first.stderr);
      assert.equal(first.read(), content);
      assert.deepEqual(first.calls, [
        queryLogDriver,
        restartDocker,
        queryLogDriver,
      ]);

      const second = fixture.run();
      assert.equal(second.status, 0, second.stderr);
      assert.equal(second.read(), content);
      assert.deepEqual(second.calls, [queryLogDriver]);
    }
  },
);

test(
  "configure-docker-logs recovers on a rerun after a failed restart",
  { skip },
  (t) => {
    const fixture = logsFixture(t, {
      content: '{"log-driver":"json-file","data-root":"/srv/docker"}',
      driver: "json-file",
    });
    const first = fixture.run({ restartStatus: 1 });
    assert.notEqual(first.status, 0);
    assert.match(first.stderr, /could not restart Docker/);
    assert.deepEqual(first.calls, [restartDocker]);
    const written = first.read();
    assert.deepEqual(JSON.parse(written), {
      "log-driver": "local",
      "data-root": "/srv/docker",
    });

    const second = fixture.run();
    assert.equal(second.status, 0, second.stderr);
    assert.equal(second.read(), written);
    assert.deepEqual(second.calls, [
      queryLogDriver,
      restartDocker,
      queryLogDriver,
    ]);
  },
);

test(
  "configure-docker-logs fails if a restart does not confirm local",
  { skip },
  (t) => {
    const content = '{"log-driver":"local"}\n';
    for (const [restartedDriver, diagnostic] of [
      ["", /could not read Docker's active log driver .* after restart/],
      [
        "json-file",
        /Docker reports log driver 'json-file' after restart; expected local/,
      ],
    ]) {
      const fixture = logsFixture(t, { content, driver: "json-file" });
      const result = fixture.run({ restartedDriver });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, diagnostic);
      assert.equal(result.read(), content);
      assert.deepEqual(result.calls, [
        queryLogDriver,
        restartDocker,
        queryLogDriver,
      ]);
    }
  },
);

test(
  "configure-docker-logs rejects invalid input before any change",
  { skip },
  (t) => {
    for (const content of [
      "{ not json",
      "",
      "[]",
      '["log-driver", "local"]',
      '"local"',
      "null",
      "42",
    ]) {
      const result = logsFixture(t, { content }).run();
      const label = JSON.stringify(content);
      assert.notEqual(result.status, 0, label);
      assert.match(result.stderr, /error: .*daemon\.json/, label);
      assert.equal(result.read(), content, label);
      assert.deepEqual(result.calls, [], label);
      assert.deepEqual(result.leftovers(), [], label);
    }
  },
);

test(
  "configure-docker-logs keeps the file and Docker when the write fails",
  { skip },
  (t) => {
    // The directory of a missing configuration directory makes the write
    // fail for every user, root included.
    const missing = logsFixture(t, { noDirectory: true }).run();
    assert.notEqual(missing.status, 0);
    assert.match(missing.stderr, /cannot write .*daemon\.json/);
    assert.deepEqual(missing.calls, []);

    // A read-only directory blocks the temporary file for an unprivileged
    // user; root ignores directory permissions.
    if (process.getuid?.() === 0) return;
    const content = '{"data-root": "/mnt/docker"}';
    const result = logsFixture(t, { content, readOnlyDirectory: true }).run();
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /cannot write .*daemon\.json/);
    assert.match(result.stderr, /was not changed and Docker was not restarted/);
    assert.equal(result.read(), content);
    assert.deepEqual(result.calls, []);
  },
);

test("start-caddy waits until Caddy answers its admin API", { skip }, (t) => {
  for (const inspect of [undefined, `${caddyImage} false`]) {
    const result = runCaddy(t, { inspect, wgetFails: 2 });
    const label = String(inspect);
    assert.equal(result.status, 0, `${label}: ${result.stderr}`);
    const probes = result.calls.filter((call) => call === caddyProbe);
    assert.equal(probes.length, 3, label);
    assert.equal(
      result.calls.filter((call) => call === caddyState).length,
      3,
      label,
    );
  }
});

test(
  "start-caddy fails at once for a container that is not running",
  {
    skip,
  },
  (t) => {
    for (const inspect of [undefined, `${caddyImage} false`]) {
      for (const state of ["exited", "dead", "restarting"]) {
        const result = runCaddy(t, { inspect, state });
        const label = `${inspect} ${state}`;
        assert.notEqual(result.status, 0, label);
        assert.match(result.stderr, new RegExp(`container is ${state}`), label);
        assert.match(result.stderr, /docker logs caddy/, label);
        assert.deepEqual(
          result.calls.filter((call) => call === caddyProbe),
          [],
          label,
        );
        assert.equal(
          result.calls.filter((call) => call === caddyState).length,
          1,
          label,
        );
      }
    }
  },
);

test("start-caddy fails when Caddy never answers", { skip }, (t) => {
  for (const inspect of [undefined, `${caddyImage} false`]) {
    const result = runCaddy(t, { inspect, wgetFails: "always" });
    const label = String(inspect);
    assert.notEqual(result.status, 0, label);
    assert.match(result.stderr, /did not answer its admin API/, label);
    assert.match(result.stderr, /within 2 seconds/, label);
    assert.match(result.stderr, /container state is running/, label);
    assert.match(result.stderr, /docker logs caddy/, label);
    const probes = result.calls.filter((call) => call === caddyProbe).length;
    // The deadline bounds the attempts: at least two, far below an
    // unbounded loop.
    assert.ok(probes >= 2 && probes <= 60, `${label}: ${probes} probes`);
  }
});

test("start-caddy uses decimal readiness deadlines", { skip }, (t) => {
  for (const [timeout, seconds] of [
    ["010", 10],
    ["10", 10],
    ["000", 0],
    ["0", 0],
    [undefined, 30],
  ]) {
    const label = timeout ?? "default";
    const result = runCaddy(t, {
      inspect: `${caddyImage} false`,
      wgetFails: "always",
      simulateClock: true,
      env: { START_CADDY_READY_TIMEOUT: timeout },
    });
    assert.equal(result.status, 1, label);
    assert.match(result.stderr, /did not answer its admin API/, label);
    assert.match(
      result.stderr,
      new RegExp(`within ${timeout ?? seconds} seconds`),
      label,
    );
    // The initial probe runs at second 0, even with a zero-second budget.
    assert.equal(
      result.calls.filter((call) => call === caddyProbe).length,
      seconds + 1,
      label,
    );
  }
});

test("start-caddy fails when the container disappears", { skip }, (t) => {
  const result = runCaddy(t, { gone: true });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /cannot read the state of the caddy container/);
  assert.match(result.stderr, /docker logs caddy/);
  assert.deepEqual(
    result.calls.filter((call) => call === caddyProbe),
    [],
  );
});

test("start-caddy does not wait on the reload path", { skip }, (t) => {
  const result = runCaddy(t, {
    inspect: `${caddyImage} true`,
    state: "exited",
    wgetFails: "always",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [caddyInspect, caddyReload]);
});

test("start-caddy rejects a bad readiness setting", { skip }, (t) => {
  for (const env of [
    { START_CADDY_READY_TIMEOUT: "soon" },
    { START_CADDY_READY_INTERVAL: "-1" },
  ]) {
    const result = runCaddy(t, { env });
    assert.equal(result.status, 2, JSON.stringify(env));
    assert.match(result.stderr, /START_CADDY_READY_TIMEOUT/);
    assert.deepEqual(result.calls, []);
  }
});

// The proxy lock `state/.proxy.lock` serializes the route change and the
// proxy reload of every service on the Droplet. The tests below pause the
// first deployment inside `caddy reload` (STUB_BLOCK_ON, released by the
// `release-docker` file) and start the deployment of another service.
const hasPgrep = spawnSync("sh", ["-c", "command -v pgrep"]).status === 0;
const skipProxyLock =
  skip || (!hasPgrep && "pgrep is not installed (procps is missing)");
const workerArgs = [
  "worker",
  concurrentImage,
  "9000",
  "worker.example.com",
  "http1",
  "none",
];
const apiArgs = [
  "api",
  dropletImage,
  "8080",
  "api.example.com",
  "http1",
  "none",
];

// A `mv` stub that records its calls (like every stub) and then runs the real
// `mv`, so that a test can see when a route file is restored.
const loggingMv = 'exec "$(command -v -p mv)" "$@"';

// Starts the deployment of `api` and pauses it inside the proxy reload, with
// the route of `api` already installed. Returns the setup and the run.
async function startPausedApiReload(t, env = {}) {
  const setup = setupDroplet(t, apiArgs, {
    route: "# previous api route\n",
    stubs: { mv: loggingMv },
    prepare: (deployRoot) =>
      writeFileSync(join(deployRoot, "env", "worker.env"), "LOG_LEVEL=info\n"),
  });
  const first = startDroplet(t, setup, apiArgs, {
    STUB_BLOCK_ON: "exec *",
    ...env,
  });
  await waitFor(
    () => setup.calls().some((call) => call.startsWith("docker exec")),
    "the first proxy reload",
  );
  return { setup, first };
}

// Waits until the run waits for the proxy lock: its `flock` child process is
// alive only while the lock is held by another process, because the service
// lock of the run was free.
async function waitForLockWait(run, what) {
  await waitFor(
    () =>
      spawnSync("pgrep", ["-P", String(run.child.pid), "-x", "flock"])
        .status === 0,
    what,
  );
}

test(
  "droplet deploy serializes the route changes and proxy reloads of different services",
  { skip: skipProxyLock },
  async (t) => {
    const { setup, first } = await startPausedApiReload(t);
    const second = startDroplet(t, setup, workerArgs);
    await waitForLockWait(
      second,
      "the second deployment to wait for the proxy lock",
    );
    // The second run did not touch its route file or the proxy, while the
    // first run still reloads.
    assert.equal(setup.read("caddy/sites/worker.caddy"), undefined);
    assert.equal(setup.read("caddy/sites/worker.caddy.new"), undefined);
    assert.equal(
      setup.calls().filter((call) => call.startsWith("docker exec")).length,
      1,
    );
    writeFileSync(join(setup.dir, "release-docker"), "");
    const [firstResult, secondResult] = await Promise.all([
      first.done,
      second.done,
    ]);
    assert.equal(firstResult.status, 0, firstResult.stderr);
    assert.equal(secondResult.status, 0, secondResult.stderr);
    assert.match(setup.read("caddy/sites/api.caddy"), /api\.example\.com/);
    assert.match(
      setup.read("caddy/sites/worker.caddy"),
      /worker\.example\.com/,
    );
    assert.equal(setup.read("state/api.current"), `${dropletImage}\n`);
    assert.equal(setup.read("state/worker.current"), `${concurrentImage}\n`);
    assert.equal(
      setup.calls().filter((call) => call.startsWith("docker exec")).length,
      2,
    );
  },
);

test(
  "droplet deploy restores a rejected route before another service adapts the Caddyfile",
  { skip: skipProxyLock },
  async (t) => {
    const { setup, first } = await startPausedApiReload(t, {
      STUB_RELOAD: "1",
    });
    const second = startDroplet(t, setup, workerArgs);
    await waitForLockWait(
      second,
      "the second deployment to wait for the proxy lock",
    );
    // The rejected route of the first run is still installed while it reloads.
    assert.match(setup.read("caddy/sites/api.caddy"), /api\.example\.com/);
    writeFileSync(join(setup.dir, "release-docker"), "");
    const [firstResult, secondResult] = await Promise.all([
      first.done,
      second.done,
    ]);
    assert.equal(firstResult.status, 1, firstResult.stderr);
    assert.match(firstResult.stderr, /rejected the route of api/);
    assert.equal(secondResult.status, 0, secondResult.stderr);
    assert.equal(setup.read("caddy/sites/api.caddy"), "# previous api route\n");
    assert.match(
      setup.read("caddy/sites/worker.caddy"),
      /worker\.example\.com/,
    );
    const calls = setup.calls();
    const restored = calls.findIndex(
      (call) => call.includes("mv -f") && call.includes("api.caddy.previous"),
    );
    const execs = calls
      .map((call, index) => (call.startsWith("docker exec") ? index : -1))
      .filter((index) => index >= 0);
    assert.equal(execs.length, 2, calls.join("\n"));
    // The restore of the rejected route happens before the second reload,
    // which adapts the Caddyfile with every route file.
    assert.ok(restored >= 0 && restored < execs[1], calls.join("\n"));
  },
);

test(
  "droplet deploy changes no route when the proxy lock stays held",
  { skip: skipProxyLock },
  async (t) => {
    const { setup, first } = await startPausedApiReload(t);
    const second = await startDroplet(t, setup, workerArgs, {
      DROPLET_PROXY_LOCK_TIMEOUT: "1",
    }).done;
    assert.equal(second.status, 1, second.stderr);
    assert.match(
      second.stderr,
      /another deployment held the proxy lock .*\.proxy\.lock for 1 seconds; the route of worker was not changed/,
    );
    assert.match(
      second.stderr,
      /the route of worker was not switched; the previous container keeps serving/,
    );
    assert.equal(setup.read("caddy/sites/worker.caddy"), undefined);
    assert.equal(setup.read("caddy/sites/worker.caddy.new"), undefined);
    assert.equal(setup.read("caddy/sites/worker.caddy.previous"), undefined);
    assert.equal(setup.read("state/worker.current"), undefined);
    assert.equal(
      setup.calls().filter((call) => call.startsWith("docker exec")).length,
      1,
    );
    // The replacement of the failed run is removed; the first run is intact.
    assert.ok(
      setup.calls().some((call) => /^docker rm --force worker-/.test(call)),
    );
    writeFileSync(join(setup.dir, "release-docker"), "");
    assert.equal((await first.done).status, 0);
    assert.match(setup.read("caddy/sites/api.caddy"), /api\.example\.com/);
    assert.equal(setup.read("state/api.current"), `${dropletImage}\n`);
  },
);

test(
  "droplet deploy changes no route when the proxy lock file cannot be opened",
  { skip },
  (t) => {
    const route = "# previous api route\n";
    const result = runDroplet(t, undefined, {
      route,
      // A directory in the place of the proxy lock file.
      prepare: (deployRoot) => {
        mkdirSync(join(deployRoot, "state", ".proxy.lock"), {
          recursive: true,
        });
      },
    });
    assert.equal(result.status, 1, result.stderr);
    assert.match(
      result.stderr,
      /could not open the proxy lock file .*\.proxy\.lock/,
    );
    assert.match(result.stderr, /the route of api was not switched/);
    assert.equal(result.read("caddy/sites/api.caddy"), route);
    assert.equal(result.read("caddy/sites/api.caddy.new"), undefined);
    assert.equal(result.read("caddy/sites/api.caddy.previous"), undefined);
    assert.equal(result.read("state/api.current"), undefined);
    assert.ok(!result.calls.some((call) => call.startsWith("docker exec")));
  },
);
