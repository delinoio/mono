// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isolateGitEnvironment } from "./git-fixture-env.mjs";

const script = fileURLToPath(
  new URL("./validate-changesets.sh", import.meta.url),
);
const baseEnv = isolateGitEnvironment({ ...process.env });
const skip = process.platform === "win32" && "the CI scripts run on Linux";

// A Git repository with `.changeset/` and a `pnpm` stub that logs its
// arguments and exits with `pnpmExit`.
function run(t, files, pnpmExit = 0) {
  const dir = mkdtempSync(join(tmpdir(), "validate-changesets-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  execFileSync("git", ["init", "--quiet"], { cwd: dir, env: baseEnv });
  mkdirSync(join(dir, ".changeset"));
  for (const [name, content] of Object.entries(files)) {
    writeFileSync(join(dir, ".changeset", name), content);
  }
  const bin = join(dir, "bin");
  mkdirSync(bin);
  writeFileSync(
    join(bin, "pnpm"),
    `#!/usr/bin/env bash\necho "$*" >> "${join(dir, "calls.log")}"\nexit ${pnpmExit}\n`,
  );
  chmodSync(join(bin, "pnpm"), 0o755);
  writeFileSync(join(dir, "calls.log"), "");
  const result = spawnSync("bash", [script], {
    cwd: dir,
    encoding: "utf8",
    env: { ...baseEnv, PATH: `${bin}${delimiter}${baseEnv.PATH}` },
  });
  return { ...result, calls: readFileSync(join(dir, "calls.log"), "utf8") };
}

const base = { "README.md": "# Changesets\n", "config.json": "{}\n" };

test("passes without running changeset when none is pending", { skip }, (t) => {
  const result = run(t, base, 1);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /No pending changesets/);
  assert.equal(result.calls, "");
});

test("runs changeset version for a pending changeset", { skip }, (t) => {
  const ok = run(t, { ...base, "brave.md": '---\n"a": patch\n---\n\nx\n' });
  assert.equal(ok.status, 0, ok.stdout + ok.stderr);
  assert.equal(ok.calls, "exec changeset version\n");
  const bad = run(t, { ...base, "brave.md": '---\n"a": patch\n---\n\nx\n' }, 1);
  assert.equal(bad.status, 1);
});
