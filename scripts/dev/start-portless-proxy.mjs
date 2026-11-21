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
// Child processes use explicit argument arrays and `shell: false` according
// to the repository environment contract.
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Values of `PORTLESS` that make `portless run` start the app without the
// proxy. They match `isPortlessSkipped` in portless 0.18.1.
const PortlessSkipValues = Object.freeze(new Set(["0", "false", "skip"]));

const PreflightEvent = Object.freeze({
  Disabled: "portless_proxy_preflight_disabled",
  NoPortlessApp: "portless_proxy_preflight_skipped",
  Starting: "portless_proxy_start",
  Failed: "portless_proxy_start_failed",
});

const repositoryRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
);

function log(record) {
  console.error(JSON.stringify(record));
}

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
    if (typeof devScript !== "string" || !/^portless(\s|$)/.test(devScript)) {
      continue;
    }
    const portlessDir = join(appDir, "node_modules", "portless");
    const portlessManifestPath = join(portlessDir, "package.json");
    if (!existsSync(portlessManifestPath)) {
      missingInstall ??= appDir;
      continue;
    }
    const { bin } = readJson(portlessManifestPath);
    const binPath = typeof bin === "string" ? bin : bin?.portless;
    if (typeof binPath === "string") {
      return { cli: join(portlessDir, binPath), appDir };
    }
  }
  if (missingInstall !== null) {
    return { cli: null, appDir: missingInstall };
  }
  return null;
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
