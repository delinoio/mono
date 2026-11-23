// SPDX-License-Identifier: FSL-1.1-ALv2

// Development launcher for ownsand-api (`pnpm --filter ownsand-api dev`).
//
// It follows the accounts-api launcher: it builds the server into a
// temporary directory, runs the binary as its direct child in its own
// process group, forwards SIGINT, SIGTERM and SIGQUIT to the active child,
// waits for it to exit and removes the build output. A signal before the
// server starts stops the launcher without starting the server.
//
// The server needs Postgres. An empty value (empty or only ASCII white space)
// counts as unset for DATABASE_URL, CORS_ORIGINS and ALLOW_DIRECT_CLIENT_ADDRESS,
// as it does for the server. When DATABASE_URL is unset, the
// launcher prints an `ownsand_api_dev_skipped` record and exits with status 0
// instead of failing, so the root `pnpm dev` keeps running the other
// development servers on machines without a database. When
// CORS_ORIGINS is unset, it allows the console's development origins in
// addition to the production origin: http://127.0.0.1:13300 (the fixed port
// of `dev:app` and `PORTLESS=0`) and https://ownsand.delino.localhost and
// http://ownsand.delino.localhost (the portless URL of the console with the
// default proxy and with an unprivileged proxy). A proxy on a custom
// PORTLESS_PORT adds that port to the URL; set CORS_ORIGINS to it there.
// When ALLOW_DIRECT_CLIENT_ADDRESS is unset, it sets it to true: the development
// server is reached directly, so the TCP peer is the client address for rate
// limits. TRUST_PROXY_CLIENT_ADDRESS=true still takes precedence. On Windows,
// where environment variable names are case-insensitive, an existing
// `cors_origins` or `Allow_Direct_Client_Address` counts as the setting: the
// launcher keeps a nonempty value and writes a default into that same entry, so
// the server never receives a second spelling. On POSIX only the exact
// uppercase names count and differently cased names stay distinct variables.
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
  "http://127.0.0.1:13300",
  "https://ownsand.delino.localhost",
  "http://ownsand.delino.localhost",
]);
const ProductionConsoleOrigin = "https://ownsand.delino.io";

// The server removes only ASCII white space (space, tab, LF, VT, FF, CR) from
// configuration values, so the launcher must use the same rule to decide that a
// value is unset. JavaScript's trim() and \s also remove Unicode white space
// such as U+00A0 and U+3000, which would hide a value the server rejects.
const AsciiWhiteSpaceOnly = /^[\t\n\v\f\r ]*$/;
const isUnset = (value) =>
  value === undefined || AsciiWhiteSpaceOnly.test(value);

// Finds the entries of `name` (an uppercase variable name) in a plain copy of
// the environment. A plain object compares keys exactly, but Windows treats
// names that differ only by case as one variable and Node then keeps only one
// spelling for the child, so a default added under the uppercase name could
// replace an explicit lowercase value. POSIX keeps differently cased names
// distinct.
function envAliases(env, name, platform) {
  const matches =
    platform === "win32"
      ? (key) => key.toUpperCase() === name
      : (key) => key === name;
  // Sorted like the entries that Node sorts before it builds the Windows
  // environment block, so the choice below is the same on every run.
  return Object.keys(env).filter(matches).sort();
}

// Applies `value` to `name` unless the environment already has a nonempty
// value, and keeps exactly one entry for the setting. Mutates `env`.
function applyEnvDefault(env, name, value, platform) {
  const aliases = envAliases(env, name, platform);
  const key =
    aliases.find((alias) => !isUnset(env[alias])) ?? aliases[0] ?? name;
  for (const alias of aliases) if (alias !== key) delete env[alias];
  if (isUnset(env[key])) env[key] = value;
}

// Builds the environment of the build and the server: a copy of `baseEnv`
// with the development defaults for the settings that are unset. Exported for
// the launcher tests, which run it with both platform rules.
export function resolveChildEnv(baseEnv, platform = process.platform) {
  const env = { ...baseEnv };
  applyEnvDefault(env, "ALLOW_DIRECT_CLIENT_ADDRESS", "true", platform);
  applyEnvDefault(
    env,
    "CORS_ORIGINS",
    [ProductionConsoleOrigin, ...DevelopmentConsoleOrigins].join(","),
    platform,
  );
  return env;
}

const packageDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const binaryName =
  process.platform === "win32" ? "ownsand-api.exe" : "ownsand-api";

let received = null;
let activeChild = null;
function installSignalHandlers() {
  return Object.values(ForwardedSignal).map((signal) => {
    const handler = () => {
      received ??= signal;
      console.error(
        JSON.stringify({ event: "ownsand_api_dev_signal", signal }),
      );
      activeChild?.kill(signal);
    };
    process.on(signal, handler);
    return [signal, handler];
  });
}

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
  if (isUnset(process.env.DATABASE_URL)) {
    console.error(
      JSON.stringify({
        event: "ownsand_api_dev_skipped",
        reason: "DATABASE_URL is not set",
      }),
    );
    return 0;
  }
  const env = resolveChildEnv(process.env);
  const buildDir = await mkdtemp(join(tmpdir(), "ownsand-api-dev-"));
  try {
    if (received) return stoppedStatus();
    const binary = join(buildDir, binaryName);
    // -buildvcs=false matches the `build` script (VCS discovery can fail in
    // a linked Git worktree).
    const buildStatus = await run(
      "go",
      ["build", "-buildvcs=false", "-o", binary, "./cmd/ownsand-api"],
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

// Run only when started as the entry point, so the launcher tests can import
// the helpers above without starting a build. import.meta.main needs Node.js
// 24.2 or later, as the launchers in scripts/dev do.
if (import.meta.main) {
  const handlers = installSignalHandlers();
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
}
