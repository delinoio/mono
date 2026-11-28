// SPDX-License-Identifier: Apache-2.0
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
import { spawnProcessGroup, startProcessGroup } from "./fixture-process.mjs";

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

// Both commands leave a Bash descendant blocked with an open output pipe.
// The second command exits before teardown runs.
function fixtureSource(dir, commandExits) {
  const blocker = `bash -c 'echo "$$" > "$DIR/blocked.pid.tmp"; mv "$DIR/blocked.pid.tmp" "$DIR/blocked.pid"; while [[ ! -e "$DIR/release" ]]; do sleep 0.05; done; printf 200'`;
  return `
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { spawnProcessGroup } from ${JSON.stringify(helperUrl)};

const dir = ${JSON.stringify(dir)};
const blocker = ${JSON.stringify(blocker)};
const commandExits = ${JSON.stringify(commandExits)};
const script = commandExits ? blocker + " & exit 3" : "out=$(" + blocker + '); echo "$out"';

test("fails before it releases the blocked command", async (t) => {
  const { child } = spawnProcessGroup(t, "bash", ["-c", script], { env: { ...process.env, DIR: dir } });
  for (let n = 0; n < 300; n += 1) {
    if (existsSync(join(dir, "blocked.pid")) && (!commandExits || child.exitCode !== null)) break;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert.ok(readFileSync(join(dir, "blocked.pid"), "utf8").trim());
  if (commandExits) {
    assert.equal(child.exitCode, 3);
    assert.equal(child.stdout.readableEnded, false);
  }
  assert.fail("deliberate failure before release");
});
`;
}

for (const [name, commandExits] of [
  ["a failing test stops a blocked command substitution", false],
  [
    "teardown stops a descendant that keeps stdout open after command exit",
    true,
  ],
]) {
  test(name, { skip }, async (t) => {
    const dir = mkdtempSync(join(tmpdir(), "fixture-process-"));
    const pidFile = join(dir, "blocked.pid");
    t.after(async () => {
      try {
        // Release our own blocker if teardown failed.
        writeFileSync(join(dir, "release"), "");
        if (existsSync(pidFile)) {
          const pid = Number(readFileSync(pidFile, "utf8"));
          await waitFor(() => !alive(pid), "the released subprocess to exit");
        }
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
    const fixture = join(dir, "blocked-fixture.mjs");
    writeFileSync(fixture, fixtureSource(dir, commandExits));
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
  });
}

test(
  "repeated teardown sends no signal to a simulated reused owner group",
  { skip },
  async (t) => {
    const { child, done, reap } = spawnProcessGroup(t, "bash", [
      "-c",
      'printf "%s" "$PPID"; exit 3',
    ]);
    const result = await done;
    assert.equal(result.status, 3);
    const ownerPid = Number(result.stdout);
    assert.ok(ownerPid > 0);
    assert.ok(child.pid > 0);
    assert.notEqual(child.pid, ownerPid);

    // The first reap waits for the owner to exit. Model reuse of that ID
    // entirely in the runner, without sending any signal to a real group.
    await reap();
    const calls = [];
    const mock = t.mock.method(process, "kill", (pid, signal) => {
      calls.push({ pid, signal, sentinel: pid === -ownerPid });
      return true;
    });
    try {
      await reap();
      await reap();
      assert.deepEqual(calls, []);
    } finally {
      mock.mock.restore();
    }
  },
);

test(
  "an unexpected owner exit rejects the active command result and teardown",
  { skip, timeout: 5_000 },
  async (t) => {
    const { child, done, reap, signalGroup } = startProcessGroup(
      process.execPath,
      ["-e", 'process.stdout.write("ready"); process.stdin.resume()'],
    );
    t.after(() => reap().catch(() => {}));

    await once(child.stdout, "data", { signal: t.signal });
    signalGroup("SIGKILL");

    await assert.rejects(done, /Fixture process owner disconnected/);
    await assert.rejects(reap(), /Fixture process owner disconnected/);
  },
);

test(
  "reports a command spawn failure and still reaps its owner",
  { skip },
  async () => {
    const { done, reap } = startProcessGroup("bash", ["-c", "exit 0"], {
      cwd: new URL("./fixture-process.mjs", import.meta.url),
    });
    try {
      await assert.rejects(done, { code: "ENOTDIR" });
    } finally {
      await assert.rejects(reap(), { code: "ENOTDIR" });
    }
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
