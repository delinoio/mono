// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const baseEnv = isolateGitEnvironment({ ...process.env });
const skip = process.platform === "win32" && "the CI scripts run on Linux";
const binary = {
  name: "ownsand-cli",
  version: "0.0.0",
  deploy: {
    target: "github-release",
    binary: "ownsand",
    platforms: ["linux-amd64"],
    licenseFiles: ["LICENSE"],
  },
};

// Run the validator and planner in a detached checkout with origin/main.
// Substitute only the two Changesets commands in this fixture.
function run(
  t,
  files,
  {
    manifest = binary,
    releases = [],
    statusExit = 0,
    pnpmExit = 0,
    temporaryParent = tmpdir(),
  } = {},
) {
  const dir = mkdtempSync(join(temporaryParent, "validate-changesets-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const git = (...args) =>
    execFileSync("git", args, { cwd: dir, env: baseEnv, encoding: "utf8" });
  git("init", "--quiet", "--initial-branch=main");
  mkdirSync(join(dir, ".changeset"));
  for (const [name, content] of Object.entries(files)) {
    writeFileSync(join(dir, ".changeset", name), content);
  }
  mkdirSync(join(dir, "cmds/ownsand"), { recursive: true });
  writeFileSync(
    join(dir, "cmds/ownsand/package.json"),
    JSON.stringify(manifest),
  );
  writeFileSync(join(dir, "LICENSE"), "Fixture license\n");
  const scripts = join(dir, "scripts/ci");
  mkdirSync(scripts, { recursive: true });
  for (const name of [
    "validate-changesets.sh",
    "deploy-plan.mjs",
    "workspace-packages.mjs",
    "workspace-node.mjs",
  ]) {
    copyFileSync(new URL(name, import.meta.url), join(scripts, name));
  }
  git("add", ".");
  git(
    "-c",
    "user.name=Fixture",
    "-c",
    "user.email=fixture@example.invalid",
    "commit",
    "--quiet",
    "-m",
    "Commit changeset fixture",
  );
  git("update-ref", "refs/remotes/origin/main", "HEAD");
  git("checkout", "--quiet", "--detach");
  git("branch", "--quiet", "-d", "main");

  const bin = join(dir, "bin");
  const callsLog = join(dir, "calls.log");
  const temporaryFiles = join(dir, "temporary");
  const githubOutput = join(dir, "github-output");
  const githubSummary = join(dir, "github-summary");
  mkdirSync(bin);
  mkdirSync(temporaryFiles);
  writeFileSync(
    join(bin, "pnpm"),
    `#!/usr/bin/env bash
printf '%s\\n' "$1 $2 $3" >> "$PNPM_CALLS_LOG"
if [[ "$#" == 5 && "$1 $2 $3 $4" == "exec changeset status --output" ]]; then
  printf '%s\\n' "$CHANGESET_STATUS" > "$5"
  exit ${statusExit}
fi
if [[ "$*" == "exec changeset version" ]]; then
  exit ${pnpmExit}
fi
exit 99
`,
  );
  chmodSync(join(bin, "pnpm"), 0o755);
  writeFileSync(callsLog, "");
  const result = spawnSync("bash", [join(scripts, "validate-changesets.sh")], {
    cwd: dir,
    encoding: "utf8",
    env: {
      ...baseEnv,
      PATH: `${bin}${delimiter}${baseEnv.PATH}`,
      PNPM_CALLS_LOG: callsLog,
      TMPDIR: temporaryFiles,
      GITHUB_OUTPUT: githubOutput,
      GITHUB_STEP_SUMMARY: githubSummary,
      CHANGESET_STATUS: JSON.stringify({
        changesets: [{ id: "brave", releases: [{ name: manifest.name }] }],
        releases,
      }),
    },
  });
  assert.deepEqual(readdirSync(temporaryFiles), []);
  assert.throws(() => readFileSync(githubOutput), { code: "ENOENT" });
  assert.throws(() => readFileSync(githubSummary), { code: "ENOENT" });
  return {
    ...result,
    calls: readFileSync(callsLog, "utf8"),
    head: git("rev-parse", "HEAD").trim(),
    branches: git(
      "for-each-ref",
      "--format=%(refname:short) %(objectname)",
      "refs/heads",
    ).trim(),
  };
}

const base = { "README.md": "# Changesets\n", "config.json": "{}\n" };
const pending = {
  ...base,
  "brave.md": '---\n"ownsand-cli": patch\n---\n\nx\n',
};
const release = (newVersion, type = "patch") => ({
  name: binary.name,
  type,
  newVersion,
  changesets: ["brave"],
});
const statusCall = "exec changeset status\n";
const versionCall = "exec changeset version\n";

test("passes without running changeset when none is pending", { skip }, (t) => {
  const result = run(t, base, { pnpmExit: 1 });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /No pending changesets/);
  assert.equal(result.calls, "");
  assert.equal(result.branches, "");
});

test(
  "checks deploy manifests even with no pending changesets",
  { skip },
  (t) => {
    const result = run(t, base, {
      manifest: { ...binary, deploy: { target: "invalid" } },
    });
    assert.equal(result.status, 2, result.stdout + result.stderr);
    assert.match(result.stderr, /ownsand-cli: deploy\.target must be one of/);
    assert.equal(result.calls, "");
  },
);

test(
  "rejects a changeset file name that the deploy planner rejects",
  { skip },
  (t) => {
    const result = run(t, {
      ...base,
      "release notes.md": '---\n"a": patch\n---\n\nx\n',
    });
    assert.equal(result.status, 2, result.stdout + result.stderr);
    assert.match(result.stderr, /release notes\\?" is not a plain file name/);
    assert.equal(result.calls, "");
  },
);

test("ignores the files that Changesets ignores", { skip }, (t) => {
  const result = run(
    t,
    {
      ...base,
      "AGENTS.md": "# Agents\n",
      "CLAUDE.md": "# Claude\n",
      "GEMINI.md": "# Gemini\n",
      "readme.md": "# lower\n",
      ".draft.md": "x\n",
    },
    { pnpmExit: 1 },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /No pending changesets/);
  assert.equal(result.calls, "");
});

test(
  "runs changeset version under a temporary parent with a double quote",
  { skip },
  (t) => {
    const temporaryParent = mkdtempSync(
      join(tmpdir(), 'validate-changesets-"'),
    );
    t.after(() => rmSync(temporaryParent, { recursive: true, force: true }));
    const result = run(t, pending, {
      pnpmExit: 1,
      releases: [release("0.0.1")],
      temporaryParent,
    });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.equal(result.calls, statusCall + versionCall);
  },
);

test(
  "rejects a committed binary prerelease before versioning",
  { skip },
  (t) => {
    const result = run(
      t,
      { ...pending, "config.json": '{"baseBranch":"release-base"}\n' },
      { releases: [release("0.0.1-next.0")] },
    );
    assert.equal(result.status, 2, result.stdout + result.stderr);
    assert.match(
      result.stderr,
      /ownsand-cli: .*MAJOR\.MINOR\.PATCH.*0\.0\.1-next\.0/,
    );
    assert.equal(result.calls, statusCall);
    assert.equal(result.branches, `release-base ${result.head}`);
  },
);

test("versions stable and none binary releases", { skip }, (t) => {
  for (const [plannedRelease, binaries] of [
    [release("0.0.1"), ["ownsand-cli"]],
    [release("0.0.0", "none"), []],
  ]) {
    const result = run(t, pending, { releases: [plannedRelease] });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(result.calls, statusCall + versionCall);
    assert.equal(result.branches, `main ${result.head}`);
    const plan = result.stdout
      .split("\n")
      .find((line) => line.includes('"event":"deploy_plan"'));
    assert.deepEqual(JSON.parse(plan).binaries, binaries);
  }
});

test(
  "propagates a status failure before running the planner",
  { skip },
  (t) => {
    const result = run(t, pending, { statusExit: 1 });
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.equal(result.calls, statusCall);
    assert.doesNotMatch(result.stdout, /"event":"deploy_plan"/);
  },
);
