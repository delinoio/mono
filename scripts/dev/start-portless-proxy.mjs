// SPDX-License-Identifier: Apache-2.0

// Starts the portless proxy once before root `pnpm dev` runs `turbo run dev`.
//
// Workaround: the `dev` script of each web app is `portless run --prefix=`,
// and `portless run` starts the proxy itself when the proxy is not running.
// `turbo run dev` starts all of these apps at the same time, so each of them
// starts its own proxy: they generate the local certificate authority into
// the same state directory at the same time and each of them asks for `sudo`
// to bind port 443 and to trust the certificate authority. Only one prompt
// can be answered, the certificate files are overwritten while other
// processes read them, and the other apps exit before they register their
// names. This script starts the proxy in the developer's terminal, before
// Turbo starts, so every app finds a running proxy and only registers its
// route. Remove this script and the root `dev` preflight when portless
// serializes concurrent proxy starts from parallel `portless run` processes.
//
// The script does nothing when `PORTLESS` disables portless (the same values
// that `portless run` accepts) or when no app runs its development server
// through portless. Otherwise it runs `portless proxy start` from the pinned
// portless installation of an app. That command exits at once when the proxy
// already runs, and it reads the same `PORTLESS_*` settings as `portless run`.
// When it fails, this script prints a `portless_proxy_start_failed` record and
// exits with its status, so Turbo does not start apps that would fail.
//
// Second workaround: a proxy that an older portless started (for example a
// global installation) keeps running across `pnpm dev` runs, and `portless
// proxy start` accepts it. portless 0.15.8 watches the routes file itself, and
// portless 0.18.1 replaces that file with a rename, so after the first route
// change the old proxy never reloads its routes. It then answers 404 for every
// app that registers later and sends requests to stale ports. Before the
// start, this script therefore reads the PID of the running proxy from the
// portless state directory, finds the portless package that the process runs
// and, when its version is older than the pinned version, runs `portless proxy
// stop` from the pinned installation. The version check works only where `ps`
// is available (not on Windows), and it treats a proxy whose version it cannot
// read as current. Remove this check when the repository no longer supports a
// proxy from a portless version that watches the routes file itself, or when
// portless restarts an outdated proxy by itself.
//
// Child processes use explicit argument arrays and `shell: false` according
// to the repository environment contract.
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, realpathSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Values of `PORTLESS` that make `portless run` start the app without the
// proxy. They match `isPortlessSkipped` in portless 0.18.1.
const PortlessSkipValues = Object.freeze(new Set(["0", "false", "skip"]));

const PreflightEvent = Object.freeze({
  Disabled: "portless_proxy_preflight_disabled",
  NoPortlessApp: "portless_proxy_preflight_skipped",
  Starting: "portless_proxy_start",
  Failed: "portless_proxy_start_failed",
  Outdated: "portless_proxy_outdated",
  StopFailed: "portless_proxy_stop_failed",
});

const repositoryRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
);

function log(record) {
  console.error(JSON.stringify(record));
}

// A `dev` script that runs portless: `portless ...` directly, or the
// Infisical development launcher (`scripts/dev/with-infisical.mjs`) in front
// of the pinned portless CLI file, which the launcher starts as
// `node node_modules/portless/dist/cli.js ...` because `shell: false` cannot
// start the `.cmd` shim on Windows. See
// docs/repository-environment-contract.md#local-development-injection.
const PortlessDevScript =
  /^(?:node \.\.\/\.\.\/scripts\/dev\/with-infisical\.mjs --path=\S+ -- )?(?:portless|node node_modules\/portless\/dist\/cli\.js)(?:\s|$)/;

function readJson(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

// Returns the path of the portless CLI that the first app with a portless
// `dev` script has installed, or null when no app uses portless.
function findPortlessCli() {
  const appsDir = join(repositoryRoot, "apps");
  if (!existsSync(appsDir)) return null;
  const appNames = readdirSync(appsDir, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
  let missingInstall = null;
  for (const appName of appNames) {
    const appDir = join(appsDir, appName);
    const manifestPath = join(appDir, "package.json");
    if (!existsSync(manifestPath)) continue;
    const devScript = readJson(manifestPath).scripts?.dev;
    if (typeof devScript !== "string" || !PortlessDevScript.test(devScript)) {
      continue;
    }
    const portlessDir = join(appDir, "node_modules", "portless");
    const portlessManifestPath = join(portlessDir, "package.json");
    if (!existsSync(portlessManifestPath)) {
      missingInstall ??= appDir;
      continue;
    }
    const { bin, version } = readJson(portlessManifestPath);
    const binPath = typeof bin === "string" ? bin : bin?.portless;
    if (typeof binPath === "string") {
      return { cli: join(portlessDir, binPath), appDir, version };
    }
  }
  if (missingInstall !== null) {
    return { cli: null, appDir: missingInstall };
  }
  return null;
}

// Returns [major, minor, patch] of a version string, or null.
function parseVersion(version) {
  const match = /^(\d+)\.(\d+)\.(\d+)/.exec(String(version));
  return match === null ? null : match.slice(1, 4).map(Number);
}

function isOlderVersion(version, than) {
  const a = parseVersion(version);
  const b = parseVersion(than);
  if (a === null || b === null) return false;
  for (let i = 0; i < 3; i += 1) {
    if (a[i] !== b[i]) return a[i] < b[i];
  }
  return false;
}

// Returns the version of the portless package that contains `path`, or null.
function portlessVersionOf(path) {
  let dir;
  try {
    dir = dirname(realpathSync(path));
  } catch {
    return null;
  }
  for (;;) {
    const manifestPath = join(dir, "package.json");
    if (existsSync(manifestPath)) {
      try {
        const manifest = readJson(manifestPath);
        if (manifest.name === "portless") return manifest.version ?? null;
      } catch {
        // Not a readable manifest; continue with the parent directory.
      }
    }
    const parent = dirname(dir);
    if (parent === dir) return null;
    dir = parent;
  }
}

// Returns { pid, version } of the proxy that the portless state directory
// records, or null when no proxy is recorded or its version is unknown.
// The state directory matches `resolveStateDir` in portless 0.18.1, which uses
// `PORTLESS_STATE_DIR` as written. A relative value therefore resolves against
// the working directory of the portless process, and this script runs portless
// from `appDir`, not from the directory where this script runs. An absolute
// value and the default `~/.portless` do not depend on the working directory.
function findRunningProxy(appDir) {
  if (process.platform === "win32") return null;
  const stateDir = process.env.PORTLESS_STATE_DIR
    ? resolve(appDir, process.env.PORTLESS_STATE_DIR)
    : join(homedir(), ".portless");
  let pid;
  try {
    pid = Number.parseInt(
      readFileSync(join(stateDir, "proxy.pid"), "utf8"),
      10,
    );
  } catch {
    return null;
  }
  if (!Number.isInteger(pid) || pid <= 0) return null;
  const ps = spawnSync("ps", ["-o", "command=", "-p", String(pid)], {
    encoding: "utf8",
    shell: false,
  });
  if (ps.status !== 0 || typeof ps.stdout !== "string") return null;
  // The proxy runs as `node <portless cli> proxy start ...`. A path that
  // contains a space is split here, and the version stays unknown.
  for (const arg of ps.stdout.trim().split(/\s+/)) {
    if (!isAbsolute(arg)) continue;
    const version = portlessVersionOf(arg);
    if (version !== null) return { pid, version };
  }
  return null;
}

// Stops a running proxy that is older than the pinned portless. Returns 0 when
// no such proxy runs or it stopped, otherwise the exit status for the script.
function stopOutdatedProxy(found) {
  const running = findRunningProxy(found.appDir);
  if (running === null || !isOlderVersion(running.version, found.version)) {
    return 0;
  }
  log({
    event: PreflightEvent.Outdated,
    pid: running.pid,
    runningVersion: running.version,
    pinnedVersion: found.version,
  });
  const result = spawnSync(process.execPath, [found.cli, "proxy", "stop"], {
    cwd: found.appDir,
    stdio: "inherit",
    shell: false,
  });
  if (result.error === undefined && result.status === 0) return 0;
  log({
    event: PreflightEvent.StopFailed,
    pid: running.pid,
    status: result.status,
    signal: result.signal,
    reason: result.error?.message,
    hint: `Stop the portless ${running.version} proxy from an interactive terminal with \`pnpm --filter delino-docs exec portless proxy stop\` or \`sudo kill ${running.pid}\`, then run \`pnpm dev\` again.`,
  });
  return result.status || 1;
}

function main() {
  const portlessSetting = process.env.PORTLESS;
  if (
    portlessSetting !== undefined &&
    PortlessSkipValues.has(portlessSetting)
  ) {
    log({ event: PreflightEvent.Disabled, PORTLESS: portlessSetting });
    return 0;
  }

  const found = findPortlessCli();
  if (found === null) {
    log({
      event: PreflightEvent.NoPortlessApp,
      reason: "no app runs portless",
    });
    return 0;
  }
  if (found.cli === null) {
    log({
      event: PreflightEvent.Failed,
      reason: "portless is not installed",
      app: found.appDir,
      hint: "Run `pnpm install --frozen-lockfile` at the repository root.",
    });
    return 1;
  }

  const stopStatus = stopOutdatedProxy(found);
  if (stopStatus !== 0) return stopStatus;

  log({ event: PreflightEvent.Starting, app: found.appDir });
  const result = spawnSync(process.execPath, [found.cli, "proxy", "start"], {
    cwd: found.appDir,
    stdio: "inherit",
    shell: false,
  });
  if (result.error !== undefined) {
    log({
      event: PreflightEvent.Failed,
      reason: result.error.message,
    });
    return 1;
  }
  if (result.status === 0) return 0;
  log({
    event: PreflightEvent.Failed,
    status: result.status,
    signal: result.signal,
    hint: "Start the proxy from an interactive terminal with `pnpm --filter delino-docs exec portless proxy start`, set PORTLESS_UNPRIVILEGED=1, or set PORTLESS=0 to run the apps on their fixed ports.",
  });
  return result.status ?? 1;
}

process.exitCode = main();
