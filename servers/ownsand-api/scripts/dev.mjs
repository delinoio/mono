// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for ownsand-api (`pnpm --filter ownsand-api dev`).
//
// It replaces `go run ./cmd/ownsand-api`. `go run` forwards only SIGINT and
// SIGQUIT to the compiled server and does not tie the server's lifetime to its
// own, so a SIGTERM sent to the launcher process (`kill`, `timeout`, a
// supervisor, pnpm or Turbo) ended `go run` and left the server listening
// without a graceful shutdown. This launcher builds the server into a
// temporary directory, runs the binary as its direct child, forwards SIGINT,
// SIGTERM and SIGQUIT to the active child, waits for it to exit and removes the
// build output afterwards. The child runs in its own process group so that a
// process-group signal (a terminal Ctrl-C or Ctrl-\) reaches it exactly once,
// through the launcher's forward. SIGQUIT makes the Go server print its
// goroutine dump and exit; the launcher then exits with status 131.
//
// SIGHUP is not handled. Because the child is no longer in the launcher's
// process group, a SIGHUP to that group (closing the terminal) or a SIGKILL of
// the launcher stops the launcher only and leaves the server running as an
// orphan that keeps its listening port. Stop such a server by its pid.
//
// Child processes use explicit argument arrays and `shell: false` according to
// the repository environment contract. Configuration (`ADDR`, `LOG_LEVEL`),
// standard streams and the working directory reach the server unchanged.
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { constants, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Signals that stop the launcher. Each one is forwarded to the active child.
const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  // Without a handler, Node ends the launcher on SIGQUIT and leaves the server
  // running as an orphan (the server is in its own process group).
  Quit: "SIGQUIT",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];

// Children run in their own process group (and session) on POSIX. A terminal
// Ctrl-C signals the whole foreground group; with the child in that group it
// would receive SIGINT directly and then again from the launcher's forward. The
// server releases its signal registration on the first signal (a second signal
// terminates it immediately), so the duplicate would abort the graceful drain.
// Detached, only the launcher receives the group signal and forwards it once.
// Windows is excluded: there `detached` opens a separate console window for the
// child instead of creating a process group, and the launcher's Windows
// behavior is unchanged. Remove the platform condition only together with a
// Windows-specific signal design.
const ownProcessGroup = process.platform !== "win32";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const binaryName =
  process.platform === "win32" ? "ownsand-api.exe" : "ownsand-api";

let received = null;
let activeChild = null;
const handlers = Object.values(ForwardedSignal).map((signal) => {
  const handler = () => {
    received ??= signal;
    console.error(JSON.stringify({ event: "ownsand_api_dev_signal", signal }));
    // Between phases there is no child; the phase boundary checks `received`.
    activeChild?.kill(signal);
  };
  process.on(signal, handler);
  return [signal, handler];
});

// Runs one child with inherited stdio and resolves with its exit status once
// it has exited: its exit code, or the shell convention for a fatal signal.
function run(command, args) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd: packageDir,
      stdio: "inherit",
      shell: false,
      detached: ownProcessGroup,
    });
    activeChild = child;
    child.once("error", (error) => {
      activeChild = null;
      reject(error);
    });
    child.once("close", (code, signal) => {
      activeChild = null;
      resolvePromise(code ?? (signal ? signalExitCode(signal) : 1));
    });
  });
}

// A stopped launcher reports the shell convention for the received signal.
const stoppedStatus = () => signalExitCode(received);

async function main() {
  const buildDir = await mkdtemp(join(tmpdir(), "ownsand-api-dev-"));
  try {
    // A signal during the `mkdtemp` await above found no child to forward to
    // and was only recorded; the build must not start after it.
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script: in a linked Git worktree Go's
    // VCS discovery can fail with "error obtaining VCS status". Remove it
    // together with the `build` script flag as documented in
    // docs/servers-ownsand-api-foundation.md.
    const buildStatus = await run("go", [
      "build",
      "-buildvcs=false",
      "-o",
      binary,
      "./cmd/ownsand-api",
    ]);
    // A signal during the build stops the launcher even when the build still
    // finished: the server must never start after a termination request.
    if (received) return stoppedStatus();
    if (buildStatus !== 0) return buildStatus;

    const serverStatus = await run(binary, []);
    return received ? stoppedStatus() : serverStatus;
  } finally {
    // Reached only after the last child has exited, so nothing uses the binary.
    await rm(buildDir, { recursive: true, force: true });
  }
}

try {
  process.exitCode = await main();
} catch (error) {
  console.error(
    JSON.stringify({ event: "ownsand_api_dev_failed", error: String(error) }),
  );
  process.exitCode = 1;
} finally {
  for (const [signal, handler] of handlers) process.off(signal, handler);
}
