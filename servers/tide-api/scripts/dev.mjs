// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for tide-api (`pnpm --filter tide-api dev`).
//
// It follows the ownsand-api launcher: it builds the server into a temporary
// directory, runs the binary as its direct child in its own process group,
// forwards SIGINT, SIGTERM and SIGQUIT to the active child, waits for it to
// exit and removes the build output. A signal before the server starts stops
// the launcher without starting the server.
//
// The server needs Postgres and TIDE_SECRET_KEYS. An empty value (empty or
// only whitespace) counts as unset for DATABASE_URL, CORS_ORIGINS,
// ALLOW_DIRECT_CLIENT_ADDRESS and METRICS_ADDR, as it does for the server.
// When DATABASE_URL is unset, the launcher prints a `tide_api_dev_skipped`
// record and exits with status 0 instead of failing, so the root `pnpm dev`
// keeps running the other development servers on machines without a
// database. The server listens on its development default
// 127.0.0.1:18400. When METRICS_ADDR is unset, the launcher sets the
// development metrics port 127.0.0.1:19400. When CORS_ORIGINS is unset, it
// allows the console's development origins in addition to the production
// origin: http://127.0.0.1:13400 (the fixed console port) and
// https://tide.delino.localhost and http://tide.delino.localhost (the
// portless URLs that the console will use, like ownsand-console). When
// ALLOW_DIRECT_CLIENT_ADDRESS is unset, it sets it to true: the development
// server is reached directly, so the TCP peer is the client address for rate
// limits. TRUST_PROXY_CLIENT_ADDRESS=true still takes precedence.
//
// Child processes use explicit argument arrays and `shell: false` according
// to the repository environment contract.
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { constants, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ForwardedSignal = Object.freeze({
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
  Quit: "SIGQUIT",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];
// See the accounts-api launcher: the child runs in its own process group on
// POSIX so a terminal signal reaches it once, through the forward.
const ownProcessGroup = process.platform !== "win32";
const DevelopmentConsoleOrigins = Object.freeze([
  "http://127.0.0.1:13400",
  "https://tide.delino.localhost",
  "http://tide.delino.localhost",
]);
const ProductionConsoleOrigin = "https://tide.delino.io";
const DevelopmentMetricsAddr = "127.0.0.1:19400";

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const binaryName = process.platform === "win32" ? "tide-api.exe" : "tide-api";

let received = null;
let activeChild = null;
const handlers = Object.values(ForwardedSignal).map((signal) => {
  const handler = () => {
    received ??= signal;
    console.error(JSON.stringify({ event: "tide_api_dev_signal", signal }));
    activeChild?.kill(signal);
  };
  process.on(signal, handler);
  return [signal, handler];
});

function run(command, args, env) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd: packageDir,
      stdio: "inherit",
      shell: false,
      detached: ownProcessGroup,
      env,
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

const stoppedStatus = () => signalExitCode(received);

async function main() {
  // An empty value counts as unset, as it does for the server.
  if (!process.env.DATABASE_URL?.trim()) {
    console.error(
      JSON.stringify({
        event: "tide_api_dev_skipped",
        reason: "DATABASE_URL is not set",
      }),
    );
    return 0;
  }
  const env = { ...process.env };
  if (!env.ALLOW_DIRECT_CLIENT_ADDRESS?.trim()) {
    env.ALLOW_DIRECT_CLIENT_ADDRESS = "true";
  }
  if (!env.METRICS_ADDR?.trim()) {
    env.METRICS_ADDR = DevelopmentMetricsAddr;
  }
  if (!env.CORS_ORIGINS?.trim()) {
    env.CORS_ORIGINS = [
      ProductionConsoleOrigin,
      ...DevelopmentConsoleOrigins,
    ].join(",");
  }
  const buildDir = await mkdtemp(join(tmpdir(), "tide-api-dev-"));
  try {
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script (VCS discovery can fail in
    // a linked Git worktree).
    const buildStatus = await run(
      "go",
      ["build", "-buildvcs=false", "-o", binary, "./cmd/tide-api"],
      env,
    );
    if (received) return stoppedStatus();
    if (buildStatus !== 0) return buildStatus;
    const serverStatus = await run(binary, [], env);
    return received ? stoppedStatus() : serverStatus;
  } finally {
    await rm(buildDir, { recursive: true, force: true });
  }
}

try {
  process.exitCode = await main();
} catch (error) {
  console.error(
    JSON.stringify({ event: "tide_api_dev_failed", error: String(error) }),
  );
  process.exitCode = 1;
} finally {
  for (const [signal, handler] of handlers) process.off(signal, handler);
}
