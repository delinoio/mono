// SPDX-License-Identifier: FSL-1.1-ALv2
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import {
  chmodSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

// These tests start the real launcher without a database. The test that runs
// the real server through the launcher needs Postgres and owns a generated
// database, so it is a Go test:
// TestLauncherForwardsSIGTERMToServerOnGeneratedDatabase in
// ../cmd/ownsand-api/launcher_signal_test.go.
const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const Timeout = 120_000;
// The tests that put a fake `go` first on PATH run a POSIX shell script, so
// Windows is skipped like the accounts-api launcher tests.
const skip = process.platform === "win32";
const ProductionOrigin = "https://ownsand.delino.io";
const DefaultCorsOrigins = [
  ProductionOrigin,
  "http://127.0.0.1:13300",
  "https://ownsand.delino.localhost",
  "http://ownsand.delino.localhost",
].join(",");

function start(env) {
  const child = spawn(process.execPath, [launcher], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
    shell: false,
  });
  let output = "";
  child.stdout.on("data", (d) => (output += d));
  child.stderr.on("data", (d) => (output += d));
  const exited = new Promise((resolve) =>
    child.once("close", (code, signal) => resolve({ code, signal })),
  );
  return { child, exited, output: () => output };
}

test("skips without DATABASE_URL", { timeout: Timeout }, async () => {
  const env = { ...process.env };
  delete env.DATABASE_URL;
  const run = start(env);
  const { code } = await run.exited;
  assert.equal(code, 0);
  assert.match(run.output(), /ownsand_api_dev_skipped/);
});

function fakeGo(t, script) {
  const dir = mkdtempSync(join(tmpdir(), "ownsand-api-fake-go-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "go");
  writeFileSync(file, `#!/bin/sh\n${script}\n`);
  chmodSync(file, 0o755);
  return dir;
}

// A fake `go` whose "build" writes a program that prints the variables it
// received and exits.
function fakeGoBuildingEnvPrinter(t) {
  const program = `#!${process.execPath}
console.log(JSON.stringify({ event: "env", cors: process.env.CORS_ORIGINS, direct: process.env.ALLOW_DIRECT_CLIENT_ADDRESS }));
`;
  return fakeGo(
    t,
    [
      'while [ $# -gt 0 ]; do if [ "$1" = "-o" ]; then out="$2"; fi; shift; done',
      `cat > "$out" <<'PROGRAM'\n${program}PROGRAM`,
      'chmod 755 "$out"',
    ].join("\n"),
  );
}

// Runs the launcher with a test-owned TMPDIR, so a build directory is visible.
async function runLauncher(t, { env, goDir }) {
  const tmp = mkdtempSync(join(tmpdir(), "ownsand-api-dev-test-"));
  t.after(() => rmSync(tmp, { recursive: true, force: true }));
  const childEnv = {
    ...process.env,
    PATH: `${goDir}${delimiter}${process.env.PATH}`,
    TMPDIR: tmp,
  };
  delete childEnv.DATABASE_URL;
  delete childEnv.CORS_ORIGINS;
  delete childEnv.ALLOW_DIRECT_CLIENT_ADDRESS;
  Object.assign(childEnv, env);
  const run = start(childEnv);
  const { code } = await run.exited;
  return { code, output: run.output(), tmp };
}

const envRecord = (output) =>
  JSON.parse(output.split("\n").find((line) => line.includes('"event":"env"')));

for (const [label, value] of [
  ["empty", ""],
  ["white space", "   "],
]) {
  test(
    `DATABASE_URL that is ${label} skips and starts no build`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output, tmp } = await runLauncher(t, {
        env: { DATABASE_URL: value },
        // A build would fail with this status; the launcher must not run it.
        goDir: fakeGo(t, "exit 3"),
      });

      assert.equal(code, 0, output);
      assert.match(output, /ownsand_api_dev_skipped/);
      assert.deepEqual(readdirSync(tmp), [], "a build directory was created");
    },
  );

  test(
    `CORS_ORIGINS and ALLOW_DIRECT_CLIENT_ADDRESS that are ${label} use the defaults`,
    { skip, timeout: Timeout },
    async (t) => {
      const { code, output } = await runLauncher(t, {
        env: {
          DATABASE_URL: "postgres://launcher-test",
          CORS_ORIGINS: value,
          ALLOW_DIRECT_CLIENT_ADDRESS: value,
        },
        goDir: fakeGoBuildingEnvPrinter(t),
      });

      assert.equal(code, 0, output);
      assert.deepEqual(envRecord(output), {
        event: "env",
        cors: DefaultCorsOrigins,
        direct: "true",
      });
    },
  );
}

test(
  "unset development variables use the defaults",
  { skip, timeout: Timeout },
  async (t) => {
    const { code, output } = await runLauncher(t, {
      env: { DATABASE_URL: "postgres://launcher-test" },
      goDir: fakeGoBuildingEnvPrinter(t),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      cors: DefaultCorsOrigins,
      direct: "true",
    });
  },
);

test(
  "explicit values pass through unchanged",
  { skip, timeout: Timeout },
  async (t) => {
    const { code, output } = await runLauncher(t, {
      env: {
        DATABASE_URL: "postgres://launcher-test",
        CORS_ORIGINS: "https://a.example",
        ALLOW_DIRECT_CLIENT_ADDRESS: "false",
      },
      goDir: fakeGoBuildingEnvPrinter(t),
    });

    assert.equal(code, 0, output);
    assert.deepEqual(envRecord(output), {
      event: "env",
      cors: "https://a.example",
      direct: "false",
    });
  },
);
