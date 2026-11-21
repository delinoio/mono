// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the shell steps of `.github/workflows/deploy.yml`:
// `check-pending-changesets.sh` (against a stubbed `gh`),
// `deploy-provider.sh` and `deploy-targets.sh` (against stubbed CLIs),
// `droplet-deploy.sh` (against stubbed `docker` and `curl`),
// `start-caddy.sh` (against a stubbed `docker`) and
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
// The scripts and the stubs call jq, which the Ubuntu runners provide.
const hasJq = spawnSync("sh", ["-c", "command -v jq"]).status === 0;
function skipReason() {
  if (process.platform === "win32") return "the deploy scripts run on Linux";
  if (!hasJq) return "jq is not installed";
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
// input to `remote-<n>.sh`.
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
  const result = spawnSync("bash", [targets, command], {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
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
        },
        {
          name: "worker",
          version: "1.0.0",
          service: "worker-svc",
          hostname: "worker.example.com",
          port: "9000",
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

test("targets build sites without uploading them", { skip }, (t) => {
  const result = runTargets(t, "build-static-sites");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(result.calls, [
    "pnpm exec turbo run build --filter=docs",
    "pnpm exec turbo run build --filter=site",
  ]);
});

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
      `ssh -F ${result.dir}/ssh_config droplet bash -s -- api ${registry}/api@${digest} 8080 api.example.com`,
      `docker load --quiet --input ${result.dir}/servers/worker.tar.gz`,
      `docker tag worker:${sha} ${registry}/worker-svc:${sha}`,
      `docker push --quiet ${registry}/worker-svc:${sha}`,
      `docker buildx imagetools inspect ${registry}/worker-svc:${sha} --format {{json .Manifest}}`,
      "guard ",
      `ssh -F ${result.dir}/ssh_config droplet bash -s -- worker-svc ${registry}/worker-svc@${digest} 9000 worker.example.com`,
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
// `stubs` adds stub commands (name -> shell body) that shadow system tools, and
// `prepare(deployRoot)` changes the scratch state before the run.
function runDroplet(
  t,
  args = ["api", dropletImage, "8080", "api.example.com"],
  {
    env = {},
    old = [],
    envFile = "LOG_LEVEL=info\n",
    route,
    state,
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
  // `null` leaves the env file out.
  if (envFile !== null) {
    writeFileSync(join(deployRoot, "env", `${args[0]}.env`), envFile);
  }
  if (state !== undefined) {
    mkdirSync(join(deployRoot, "state"));
    writeFileSync(join(deployRoot, "state", "api.current"), state);
  }
  if (route !== undefined) {
    writeFileSync(join(deployRoot, "caddy", "sites", "api.caddy"), route);
  }
  shellStub(
    bin,
    "docker",
    `case "$1" in
  ps)
    [[ "\${STUB_PS_EXIT:-0}" == 0 ]] || exit "\$STUB_PS_EXIT"
    printf '%s\\n' ${old.map((id) => `'${id}'`).join(" ")} | grep . || true ;;
  inspect)
    case "$3" in
      *State.Status*) echo "\${STUB_STATUS:-running}" ;;
      *) echo 172.18.0.5 ;;
    esac ;;
  exec) exit "\${STUB_RELOAD:-0}" ;;
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
  const result = spawnSync("bash", ["-s", "--", ...args], {
    encoding: "utf8",
    input: readFileSync(dropletDeploy, "utf8"),
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      STUB_DIR: dir,
      DROPLET_ROOT: deployRoot,
      DROPLET_HEALTH_TIMEOUT: "3",
      ...env,
    },
  });
  const read = (path) => {
    try {
      return readFileSync(join(deployRoot, path), "utf8");
    } catch {
      return undefined;
    }
  };
  return {
    ...result,
    read,
    calls: readFileSync(join(dir, "calls.log"), "utf8").trim().split("\n"),
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
      "docker run",
      "docker inspect",
      "docker inspect",
      "curl --silent",
      "docker exec",
      "docker stop",
      "docker rm",
      "docker image",
    ]);
    const run = result.calls.find((call) => call.startsWith("docker run"));
    // The replacement joins a unique alias, never the alias of the running
    // container, so the live route cannot reach it before the health check.
    const alias = run.match(/--network-alias (\S+)/)[1];
    assert.match(alias, /^api-\d{14}-e{12}$/);
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
        [service, dropletImage, "8080", "api.example.com"],
        { old: ["old1"] },
      );
      assert.equal(result.status, 0, `${service}: ${result.stderr}`);
      const run = result.calls.find((call) => call.startsWith("docker run"));
      const alias = run.match(/--network-alias (\S+)/)[1];
      assert.match(alias, /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/, service);
      assert.match(alias, /-\d{14}-e{12}$/, service);
      assert.ok(!alias.includes("--"), alias);
      assert.ok(alias.startsWith(service.slice(0, 35).replace(/-+$/, "")));
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

test(
  "droplet deploy rejects bad arguments and configuration before any change",
  { skip },
  (t) => {
    const image = dropletImage;
    for (const [args, options, message] of [
      [["api", image, "8080"], {}, /usage: droplet-deploy\.sh/],
      [["API", image, "8080", "api.example.com"], {}, /service name/],
      [["api;x", image, "8080", "api.example.com"], {}, /service name/],
      [
        ["api", "registry/api:latest", "8080", "api.example.com"],
        {},
        /pinned by digest/,
      ],
      [["api", image, "80", "api.example.com"], {}, /port/],
      [["api", image, "70000", "api.example.com"], {}, /port/],
      [["api", image, "8080", "localhost"], {}, /host name/],
      [["api", image, "8080", "a.example.com;x"], {}, /host name/],
      [
        ["api", image, "8080", `${"a".repeat(64)}.example.com`],
        {},
        /host name/,
      ],
      [["api", image, "8080", `api.${"a".repeat(64)}.com`], {}, /host name/],
      [
        ["api", image, "8080", "api.example.com"],
        { envFile: null },
        /api\.env does not exist/,
      ],
      [
        ["api", image, "8080", "api.example.com"],
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
      ["api", image, "8080", `${"a".repeat(63)}.example.com`],
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
function runCaddy(t, { inspect, fail, args } = {}) {
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
  inspect) [[ -n "\${STUB_INSPECT:-}" ]] && echo "$STUB_INSPECT" || exit 1 ;;
esac`,
  );
  const env = { ...baseEnv, PATH: `${bin}${delimiter}${baseEnv.PATH}` };
  env.STUB_DIR = dir;
  if (inspect !== undefined) env.STUB_INSPECT = inspect;
  if (fail !== undefined) env.STUB_FAIL = fail;
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
const caddyReload =
  "docker exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile";

test("start-caddy creates a missing container", { skip }, (t) => {
  const result = runCaddy(t);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.calls, [
    caddyInspect,
    "docker rm --force caddy",
    caddyRun,
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
  assert.deepEqual(result.calls, [caddyInspect, "docker start caddy"]);
});

test("start-caddy recreates a container with another image", { skip }, (t) => {
  for (const running of ["true", "false"]) {
    const result = runCaddy(t, {
      inspect: `caddy:2.10.0@sha256:${"a".repeat(64)} ${running}`,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(
      result.calls,
      [caddyInspect, "docker rm --force caddy", caddyRun],
      running,
    );
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
