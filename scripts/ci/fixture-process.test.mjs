// SPDX-License-Identifier: Apache-2.0
//
// Tests that a failing test does not leave the fixture's blocked subprocesses
// running. A nested `node --test` run starts a blocked command substitution
// (the shape of the stubbed `curl` in the Droplet deployment fixtures) through
// `spawnProcessGroup` and then fails before it releases the command. The
// nested runner must report that failure and exit, and the blocked process
// must be gone.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { once } from "node:events";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";
import { killProcessGroup, spawnProcessGroup } from "./fixture-process.mjs";

const skip =
  process.platform === "win32"
    ? "the fixtures use POSIX process groups"
    : false;
const helperUrl = pathToFileURL(
  fileURLToPath(new URL("./fixture-process.mjs", import.meta.url)),
).href;

function alive(pid, readStat = readFileSync) {
  try {
    process.kill(pid, 0);
  } catch (error) {
    if (error?.code === "ESRCH") return false;
    throw error;
  }

  if (process.platform !== "linux") return true;

  try {
    // Signal zero still succeeds until the parent reaps a Linux zombie.
    const stat = readStat(`/proc/${pid}/stat`, "utf8");
    // The command name can contain spaces and closing parentheses.
    const commandEnd = stat.lastIndexOf(")");
    return !(
      stat.startsWith(`${pid} (`) && stat.startsWith(") Z ", commandEnd)
    );
  } catch {
    // Keep the signal-zero result when procfs cannot supply the state.
    return true;
  }
}

async function waitFor(condition, what) {
  for (let attempt = 0; attempt < 300; attempt += 1) {
    if (condition()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert.fail(`timed out waiting for ${what}`);
}

// The nested fixture: bash runs a command substitution that blocks until
// `release` exists and records its own pid; the test fails before it writes
// `release`.
function fixtureSource(dir) {
  return `
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { spawnProcessGroup } from ${JSON.stringify(helperUrl)};

const dir = ${JSON.stringify(dir)};
const script = \`out=$(bash -c 'echo "$$" > "$DIR/blocked.pid.tmp"; mv "$DIR/blocked.pid.tmp" "$DIR/blocked.pid"; while [[ ! -e "$DIR/release" ]]; do sleep 0.05; done; printf 200')
echo "$out"\`;

test("fails before it releases the blocked command", async (t) => {
  spawnProcessGroup(t, "bash", ["-c", script], { env: { ...process.env, DIR: dir } });
  for (let n = 0; n < 300 && !existsSync(join(dir, "blocked.pid")); n += 1) {
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert.ok(readFileSync(join(dir, "blocked.pid"), "utf8").trim());
  assert.fail("deliberate failure before release");
});
`;
}

test(
  "a failing test stops the blocked fixture subprocess and the runner exits",
  { skip },
  async (t) => {
    const dir = mkdtempSync(join(tmpdir(), "fixture-process-"));
    const pidFile = join(dir, "blocked.pid");
    t.after(() => {
      // Only needed when the teardown under test did not work.
      if (existsSync(pidFile)) {
        const pid = Number(readFileSync(pidFile, "utf8"));
        if (pid > 0 && alive(pid)) process.kill(pid, "SIGKILL");
      }
      rmSync(dir, { recursive: true, force: true });
    });
    const fixture = join(dir, "blocked-fixture.mjs");
    writeFileSync(fixture, fixtureSource(dir));
    // The nested runner must not inherit this runner's test context.
    const env = { ...process.env };
    delete env.NODE_TEST_CONTEXT;

    const result = spawnSync(process.execPath, ["--test", fixture], {
      encoding: "utf8",
      env,
      // Bounded so that a regression fails this test instead of hanging it.
      timeout: 60_000,
      killSignal: "SIGKILL",
    });

    assert.equal(result.error, undefined, "the nested runner did not exit");
    assert.equal(result.signal, null);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /deliberate failure before release/);
    const pid = Number(readFileSync(pidFile, "utf8"));
    assert.ok(pid > 0);
    await waitFor(() => !alive(pid), "the blocked subprocess to exit");
  },
);

test(
  "teardown treats a process group that already exited as normal",
  { skip },
  async (t) => {
    const { child, done } = spawnProcessGroup(t, "bash", ["-c", "exit 3"]);
    const result = await done;
    assert.equal(result.status, 3);
    // The same call the teardown makes; it must not throw.
    killProcessGroup(child);
    killProcessGroup(child);
  },
);

test(
  "Linux fixture liveness requires a parseable zombie state to override signal zero",
  { skip: process.platform !== "linux" },
  () => {
    const pid = process.pid;
    assert.equal(
      alive(pid, () => `${pid} (name) with spaces) Z 1 2 3`),
      false,
    );
    assert.equal(
      alive(pid, () => `${pid} (name) Z in command) S 1 2 3`),
      true,
    );
    assert.equal(
      alive(pid, () => `${pid} (name Z 1 2 3`),
      true,
    );
    assert.equal(
      alive(pid, () => "malformed) Z 1 2 3"),
      true,
    );
    assert.equal(
      alive(pid, (path, encoding) => readFileSync(`${path}.missing`, encoding)),
      true,
    );
  },
);

test(
  "captures split UTF-8 characters independently on stdout and stderr",
  { skip, timeout: 5_000 },
  async (t) => {
    const { child, done } = spawnProcessGroup(t, process.execPath, [
      "-e",
      `
        process.stdout.write(Buffer.from("41ed", "hex"));
        process.stderr.write(Buffer.from("42ed", "hex"));
        process.stdin.once("data", () => {
          process.stdout.write(Buffer.from("959c", "hex"));
          process.stderr.write(Buffer.from("959c", "hex"));
        });
      `,
    ]);
    const [[stdoutPrefix], [stderrPrefix]] = await Promise.all([
      once(child.stdout, "data", { signal: t.signal }),
      once(child.stderr, "data", { signal: t.signal }),
    ]);
    assert.deepEqual(stdoutPrefix, Buffer.from("41ed", "hex"));
    assert.deepEqual(stderrPrefix, Buffer.from("42ed", "hex"));
    child.stdin.end("continue");

    assert.deepEqual(await done, { status: 0, stdout: "A한", stderr: "B한" });
  },
);

test(
  "flushes incomplete UTF-8 when the child closes",
  { skip, timeout: 5_000 },
  async (t) => {
    const { done } = spawnProcessGroup(t, process.execPath, [
      "-e",
      `
        process.stdout.write(Buffer.from("6f6bed", "hex"));
        process.stderr.write("한");
        process.exitCode = 3;
      `,
    ]);

    assert.deepEqual(await done, { status: 3, stdout: "ok�", stderr: "한" });
  },
);
