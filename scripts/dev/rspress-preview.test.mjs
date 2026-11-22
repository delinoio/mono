// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  FailureReason,
  PreviewOverrideVariables,
  previewEnvironment,
  resolveRspressBin,
} from "./rspress-preview.mjs";

const launcher = fileURLToPath(
  new URL("./rspress-preview.mjs", import.meta.url),
);
const repositoryRoot = fileURLToPath(new URL("../..", import.meta.url));
const posixOnly = {
  skip: process.platform === "win32" && "POSIX signal exit statuses",
};

// A fake `@rspress/core` package whose bin file records its arguments and
// environment in FAKE_RSPRESS_RECORD, then exits with FAKE_RSPRESS_EXIT or ends
// itself with the signal in FAKE_RSPRESS_SIGNAL.
const fakeBin = `
const { writeFileSync } = require("node:fs");
writeFileSync(
  process.env.FAKE_RSPRESS_RECORD,
  JSON.stringify({
    args: process.argv.slice(2),
    PORT: process.env.PORT ?? null,
    HOST: process.env.HOST ?? null,
    KEEP: process.env.KEEP ?? null,
    cwd: process.cwd(),
  }),
);
if (process.env.FAKE_RSPRESS_SIGNAL) {
  process.kill(process.pid, process.env.FAKE_RSPRESS_SIGNAL);
  setTimeout(() => {}, 10000);
} else {
  process.exitCode = Number(process.env.FAKE_RSPRESS_EXIT ?? "0");
}
`;

// Creates a package directory whose `node_modules` holds the fake package.
function fixture(
  t,
  manifest = { name: "@rspress/core", bin: { rspress: "./bin/rspress.cjs" } },
) {
  const root = mkdtempSync(join(tmpdir(), "rspress-preview-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const pkg = join(root, "node_modules", "@rspress", "core");
  mkdirSync(join(pkg, "bin"), { recursive: true });
  writeFileSync(join(pkg, "package.json"), JSON.stringify(manifest));
  writeFileSync(join(pkg, "bin", "rspress.cjs"), fakeBin);
  writeFileSync(join(root, "package.json"), "{}");
  return { root, record: join(root, "record.json") };
}

function run({ root, record }, args = [], env = {}, script = launcher) {
  const result = spawnSync(process.execPath, [script, ...args], {
    cwd: root,
    encoding: "utf8",
    env: {
      ...process.env,
      PORT: undefined,
      HOST: undefined,
      FAKE_RSPRESS_RECORD: record,
      ...env,
    },
  });
  let recorded = null;
  try {
    recorded = JSON.parse(readFileSync(record, "utf8"));
  } catch {
    // The fake CLI did not run.
  }
  return { ...result, recorded };
}

test("previewEnvironment removes PORT and HOST only", () => {
  const { env, removed } = previewEnvironment(
    {
      PORT: "5000",
      HOST: "0.0.0.0",
      KEEP: "1",
      PORTLESS_URL: "x",
      PATH: "/bin",
    },
    "linux",
  );
  assert.deepEqual(env, { KEEP: "1", PORTLESS_URL: "x", PATH: "/bin" });
  assert.deepEqual(removed.sort(), [...PreviewOverrideVariables].sort());
});

test("previewEnvironment keeps lower-case names on POSIX and removes them on Windows", () => {
  const input = { Port: "5000", host: "0.0.0.0" };
  assert.deepEqual(previewEnvironment(input, "linux").env, input);
  const windows = previewEnvironment(input, "win32");
  assert.deepEqual(windows.env, {});
  assert.deepEqual(windows.removed.sort(), ["HOST", "PORT"]);
});

test("previewEnvironment does not change its input and reports nothing to remove", () => {
  const input = { KEEP: "1" };
  const { env, removed } = previewEnvironment(input, "linux");
  assert.deepEqual(env, input);
  assert.notEqual(env, input);
  assert.deepEqual(removed, []);
});

test("resolveRspressBin reads the bin file from the package in the working directory", (t) => {
  const f = fixture(t);
  assert.equal(
    resolveRspressBin(f.root),
    join(f.root, "node_modules", "@rspress", "core", "bin", "rspress.cjs"),
  );
});

test("resolveRspressBin accepts a string bin and rejects a missing one", (t) => {
  const string = fixture(t, {
    name: "@rspress/core",
    bin: "./bin/rspress.cjs",
  });
  assert.ok(resolveRspressBin(string.root).endsWith("rspress.cjs"));
  const none = fixture(t, { name: "@rspress/core" });
  assert.throws(
    () => resolveRspressBin(none.root),
    (error) => error.reason === FailureReason.CliNotFound,
  );
});

test("resolveRspressBin fails when the package is not installed", (t) => {
  const root = mkdtempSync(join(tmpdir(), "rspress-preview-empty-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(join(root, "package.json"), "{}");
  assert.throws(
    () => resolveRspressBin(root),
    (error) => error.reason === FailureReason.CliNotFound,
  );
});

test("the launcher starts `rspress preview` without PORT and HOST", (t) => {
  const f = fixture(t);
  const result = run(f, [], { PORT: "5000", HOST: "0.0.0.0", KEEP: "kept" });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.recorded.args, ["preview"]);
  assert.equal(result.recorded.PORT, null);
  assert.equal(result.recorded.HOST, null);
  assert.equal(result.recorded.KEEP, "kept");
  assert.equal(result.recorded.cwd, f.root);
  const record = JSON.parse(result.stderr.trim());
  assert.equal(record.event, "rspress_preview_env");
  assert.deepEqual(record.removed.sort(), ["HOST", "PORT"]);
  assert.doesNotMatch(result.stderr, /5000|0\.0\.0\.0/);
});

test("a symlinked launcher path behaves like the real path", posixOnly, (t) => {
  // Node.js keeps the symlink spelling in process.argv[1] and resolves
  // import.meta.url to the real path, so a guard that compares the two would
  // skip the launcher silently with status 0.
  const f = fixture(t);
  const fileLink = join(f.root, "linked-file", "rspress-preview.mjs");
  mkdirSync(dirname(fileLink));
  symlinkSync(launcher, fileLink);
  const directoryLink = join(f.root, "linked-directory");
  symlinkSync(dirname(launcher), directoryLink, "dir");
  for (const script of [fileLink, join(directoryLink, "rspress-preview.mjs")]) {
    rmSync(f.record, { force: true });
    const result = run(f, ["--open"], { PORT: "5000", KEEP: "kept" }, script);
    assert.equal(result.status, 0, `${script}: ${result.stderr}`);
    assert.deepEqual(result.recorded?.args, ["preview", "--open"], script);
    assert.equal(result.recorded.PORT, null, script);
    assert.equal(result.recorded.KEEP, "kept", script);
    assert.equal(JSON.parse(result.stderr.trim()).event, "rspress_preview_env");
  }
});

test("the launcher logs nothing when PORT and HOST are unset", (t) => {
  const f = fixture(t);
  const result = run(f);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
});

test("arguments after the launcher reach `rspress preview`", (t) => {
  const f = fixture(t);
  const result = run(f, ["--config", "other.ts", "--open"]);
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(result.recorded.args, [
    "preview",
    "--config",
    "other.ts",
    "--open",
  ]);
});

test("the launcher exits with the exit code of Rspress", (t) => {
  const f = fixture(t);
  const result = run(f, [], { FAKE_RSPRESS_EXIT: "3" });
  assert.equal(result.status, 3);
});

test(
  "the launcher exits with 128 plus the signal number when a signal ends Rspress",
  posixOnly,
  (t) => {
    const f = fixture(t);
    const result = run(f, [], { FAKE_RSPRESS_SIGNAL: "SIGTERM" });
    assert.equal(result.status, 128 + 15);
  },
);

test("the launcher fails with a structured record when Rspress is not installed", (t) => {
  const root = mkdtempSync(join(tmpdir(), "rspress-preview-missing-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(join(root, "package.json"), "{}");
  const result = run({ root, record: join(root, "record.json") });
  assert.equal(result.status, 1);
  assert.equal(result.recorded, null);
  const record = JSON.parse(result.stderr.trim().split("\n").at(-1));
  assert.equal(record.event, "rspress_preview_failed");
  assert.equal(record.reason, FailureReason.CliNotFound);
});

test("the preview scripts of both Rspress apps run the launcher", () => {
  for (const app of ["delino-docs", "delino-site"]) {
    const manifest = JSON.parse(
      readFileSync(join(repositoryRoot, "apps", app, "package.json"), "utf8"),
    );
    assert.equal(
      manifest.scripts.preview,
      "node ../../scripts/dev/rspress-preview.mjs",
      app,
    );
    assert.equal(manifest.scripts["dev:app"], "rspress dev", app);
    assert.equal(manifest.scripts.build, "rspress build", app);
  }
});
