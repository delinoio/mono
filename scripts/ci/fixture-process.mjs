// SPDX-License-Identifier: Apache-2.0
//
// Starts a fixture subprocess in its own process group and stops the whole
// group when the test ends. POSIX only: the callers skip on Windows.
import { spawn } from "node:child_process";
import { StringDecoder } from "node:string_decoder";

// Sends SIGKILL to the process group of `child`. A group that already exited
// is normal teardown (ESRCH); any other error is a real failure.
export function killProcessGroup(child) {
  // No pid means the spawn failed and no process exists.
  if (child.pid === undefined) return;
  try {
    process.kill(-child.pid, "SIGKILL");
  } catch (error) {
    if (error?.code !== "ESRCH") throw error;
  }
}

// Spawns `command` with piped stdio in a new process group (detached) and
// returns the child plus `done`, which resolves with the exit status and the
// captured output once the child exited and its streams closed. `t.after`
// kills the group and waits for `done`.
//
// Killing only the child is not enough: a command substitution or pipeline
// that the child started (for example a stubbed `curl` that blocks until a
// file appears) keeps the stdout and stderr pipes open after the child died.
// `close` never fires then and `node --test` cannot exit after a failing
// test. Killing the group closes the pipes. The group id equals the child pid
// only because `detached: true` makes the child a group leader.
export function spawnProcessGroup(t, command, args, options = {}) {
  const child = spawn(command, args, {
    ...options,
    stdio: ["pipe", "pipe", "pipe"],
    detached: true,
  });
  const stdoutDecoder = new StringDecoder("utf8");
  const stderrDecoder = new StringDecoder("utf8");
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => (stdout += stdoutDecoder.write(chunk)));
  child.stderr.on("data", (chunk) => (stderr += stderrDecoder.write(chunk)));
  const done = new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (status) =>
      resolve({
        status,
        stdout: stdout + stdoutDecoder.end(),
        stderr: stderr + stderrDecoder.end(),
      }),
    );
  });
  // An unawaited `done` must not turn a spawn error into an unhandled
  // rejection after the test has already failed.
  done.catch(() => {});
  t.after(async () => {
    killProcessGroup(child);
    await done;
  });
  return { child, done };
}
