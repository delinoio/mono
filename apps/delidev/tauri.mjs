// SPDX-License-Identifier: Apache-2.0
import { spawn, spawnSync } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createRequire } from "node:module";
import { constants, tmpdir } from "node:os";
import { extname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { run } from "@tauri-apps/cli";
import { linuxCefCli } from "./tauri-debian-cli.mjs";

const args = process.argv.slice(2);
const commandIndex = args.findIndex((arg) => !arg.startsWith("-"));
const command = args[commandIndex];
const options = args.slice(commandIndex + 1);
const forwardedIndex = options.indexOf("--");
const tauriOptions =
  forwardedIndex === -1 ? options : options.slice(0, forwardedIndex);
const require = createRequire(import.meta.url);

function requestedBundles(options) {
  let formats;
  for (const [index, option] of options.entries()) {
    const format = /^(?:--bundles(?:=(.*))?|-[dv]*b(?:=?(.*))?)$/.exec(option);
    if (!format) continue;

    formats ??= [];
    const attached = format[1] ?? format[2];
    if (attached) formats.push(...attached.split(","));
    for (const value of options.slice(index + 1)) {
      if (value.startsWith("-")) break;
      formats.push(...value.split(","));
    }
  }
  return formats;
}

function requestedTarget(options) {
  for (const [index, option] of options.entries()) {
    const target = /^(?:--target(?:=(.*))?|-[dv]*t(?:=?(.*))?)$/.exec(option);
    if (target) {
      return (target[1] ?? target[2]) || options[index + 1];
    }
  }
}

function mergeConfig(base, patch, deleteNulls) {
  if (patch === null || typeof patch !== "object" || Array.isArray(patch)) {
    return patch;
  }
  const merged = Object.create(null);
  if (base !== null && typeof base === "object" && !Array.isArray(base)) {
    Object.assign(merged, base);
  }
  for (const [key, value] of Object.entries(patch)) {
    if (value === null && deleteNulls) {
      delete merged[key];
    } else {
      merged[key] = mergeConfig(merged[key], value, deleteNulls);
    }
  }
  return merged;
}

function readConfig(value, getProbeDir) {
  if (value === undefined) throw new Error("Missing Tauri configuration");
  if (value.startsWith("{")) return JSON.parse(value);
  const extension = extname(value);
  if (extension !== ".toml" && extension !== ".json5") {
    const content = readFileSync(value, "utf8");
    try {
      return JSON.parse(content);
    } catch {
      // Tauri also accepts JSON5 in files with the .json extension.
    }
  }

  // The pinned CLI exports the parsed CLI override as TAURI_CONFIG before
  // validating the merged app configuration. Use an isolated app directory
  // and an empty bundle list so the probe never runs build or bundle hooks.
  // Remove the probe when the CLI exposes a config parsing API.
  const probeDir = getProbeDir();
  writeFileSync(
    join(probeDir, "Tauri.toml"),
    'identifier = "io.delino.config-probe"\n',
  );
  const { stdout, stderr, error, status } = spawnSync(
    process.execPath,
    [
      "-e",
      `const { run } = require(process.argv[1]);
       delete process.env.TAURI_CONFIG;
       run(["bundle", "--config", process.argv[2], "--bundles"], "pnpm tauri")
         .catch(error => { if (!process.env.TAURI_CONFIG) throw error; })
         .then(() => process.stdout.write(process.env.TAURI_CONFIG));`,
      "--",
      require.resolve("@tauri-apps/cli"),
      value,
    ],
    {
      encoding: "utf8",
      env: { ...process.env, TAURI_APP_PATH: probeDir },
    },
  );
  if (error) throw error;
  if (status !== 0) {
    throw new Error(
      stderr.trim() || `Failed to read Tauri configuration ${value}`,
    );
  }
  return JSON.parse(stdout);
}

function resolveConfig(platformName) {
  let probeDir;
  const getProbeDir = () =>
    (probeDir ??= mkdtempSync(join(tmpdir(), "delidev-config-")));
  try {
    const tauriDir = fileURLToPath(new URL("./src-tauri/", import.meta.url));
    const base = ["tauri.conf.json", "tauri.conf.json5", "Tauri.toml"]
      .map((file) => join(tauriDir, file))
      .find(existsSync);
    const platform = [
      `tauri.${platformName}.conf.json`,
      `tauri.${platformName}.conf.json5`,
      `Tauri.${platformName}.toml`,
    ]
      .map((file) => join(tauriDir, file))
      .find(existsSync);

    // Tauri applies the platform patch first. It combines CLI patches
    // separately so their nulls can remove base or platform keys.
    let baseConfig = readConfig(base, getProbeDir);
    if (platform) {
      baseConfig = mergeConfig(
        baseConfig,
        readConfig(platform, getProbeDir),
        true,
      );
    }
    let overrides = {};
    let configIndex = 0;
    let debFilesCleared = false;
    for (let index = 0; index < tauriOptions.length; index++) {
      const option = tauriOptions[index];
      const config = /^(?:--config(?:=(.*))?|-[dv]*c(?:=?(.*))?)$/.exec(option);
      if (!config) continue;

      const value = (config[1] ?? config[2]) || tauriOptions[++index];
      const patch = readConfig(value, getProbeDir);
      debFilesCleared ||= patch?.bundle?.linux?.deb?.files === null;
      overrides = mergeConfig(overrides, patch, false);
      configIndex = index + 1;
    }
    return {
      config: mergeConfig(baseConfig, overrides, true),
      baseConfig,
      overrides,
      configIndex,
      debFilesCleared,
    };
  } finally {
    if (probeDir) rmSync(probeDir, { recursive: true, force: true });
  }
}

function resolveBundleConfig() {
  if (
    (command !== "build" && command !== "bundle") ||
    tauriOptions.some(
      (option) => option === "--help" || /^-[dv]*h$/.test(option),
    ) ||
    tauriOptions.includes("--no-bundle")
  ) {
    return;
  }

  const target = requestedTarget(tauriOptions);
  let platformName;
  let noticeFormats;
  if (
    process.platform === "linux" &&
    (!target || (target.includes("-linux-") && !target.includes("android")))
  ) {
    platformName = "linux";
    noticeFormats = ["deb", "rpm", "appimage"];
  } else if (
    process.platform === "darwin" &&
    (!target || target.endsWith("-apple-darwin"))
  ) {
    platformName = "macos";
    noticeFormats = ["app", "dmg"];
  } else {
    return;
  }

  const supportedFormats =
    platformName === "linux" ? ["deb", "rpm", "appimage"] : noticeFormats;
  const includesFormat = (formats, candidates) =>
    formats.some((format) => candidates.includes(format));
  const explicitFormats = requestedBundles(tauriOptions)?.map((format) =>
    format.toLowerCase(),
  );
  if (explicitFormats && !includesFormat(explicitFormats, supportedFormats)) {
    return;
  }

  const resolved = resolveConfig(platformName);
  const bundle = resolved.config?.bundle;
  if (!explicitFormats && command === "build" && bundle?.active !== true) {
    return;
  }
  const targets = bundle?.targets ?? "all";
  const selectedFormats =
    explicitFormats ??
    (typeof targets === "string" && targets.toLowerCase() === "all"
      ? supportedFormats
      : [targets]
          .flat()
          .filter((target) => typeof target === "string")
          .map((target) => target.toLowerCase()));
  if (!includesFormat(selectedFormats, supportedFormats)) return;
  const formats = supportedFormats.filter((format) =>
    selectedFormats.includes(format),
  );
  const embedsCef = bundle?.cef?.embed !== false;
  const stageNotices =
    embedsCef && includesFormat(selectedFormats, noticeFormats);
  // On Linux, DeliDev links CEF for both shared and embedded bundles.
  const usePatchedCli =
    platformName === "linux" &&
    (embedsCef || includesFormat(selectedFormats, ["deb", "rpm"]));
  return {
    ...resolved,
    platformName,
    formats,
    stageNotices,
    usePatchedCli,
  };
}

function shellQuote(value) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

// Adding a nested object to a CLI patch can replace an earlier null.
// Clear the corresponding base fields explicitly in that case.
function preserveClears(base, override, additions) {
  if (typeof additions !== "object" || additions === null) return additions;
  const patch =
    override === null
      ? Object.fromEntries(Object.keys(base ?? {}).map((key) => [key, null]))
      : Object.create(null);
  for (const [key, value] of Object.entries(additions)) {
    patch[key] = preserveClears(
      base?.[key],
      override === null ? null : override?.[key],
      value,
    );
  }
  return patch;
}

async function runPatchedCli(cli, args, options) {
  const child = spawn(cli, args, options);
  let interrupted;
  const onSignal = (signal) => {
    interrupted ??= signal;
    // Terminal Ctrl-C already reaches the CLI in the shared process group.
    if (signal === "SIGTERM" && !child.killed) child.kill(signal);
  };
  process.on("SIGINT", onSignal);
  process.on("SIGTERM", onSignal);
  try {
    let failure;
    child.on("error", (error) => {
      failure = error;
    });
    const [code, signal] = await new Promise((resolve) => {
      child.once("close", (code, signal) => resolve([code, signal]));
    });
    if (failure) throw failure;
    const stoppingSignal = interrupted ?? signal;
    return stoppingSignal ? 128 + constants.signals[stoppingSignal] : code;
  } finally {
    process.off("SIGINT", onSignal);
    process.off("SIGTERM", onSignal);
  }
}

async function runNoticeBuild(
  {
    config,
    baseConfig,
    overrides,
    configIndex,
    debFilesCleared,
    platformName,
    formats,
  },
  cli,
) {
  // Tauri reads its file mappings before Cargo downloads CEF. Stage the files
  // in a private directory after the download and before Tauri bundles the app.
  const noticeDir = mkdtempSync(join(tmpdir(), "delidev-cef-"));
  try {
    const build = config?.build ?? {};
    const key = Object.hasOwn(build, "before-bundle-command")
      ? "before-bundle-command"
      : "beforeBundleCommand";
    const hook = build[key];
    const helper = fileURLToPath(new URL("./cef-notices.mjs", import.meta.url));
    const stage = `${shellQuote(process.execPath)} ${shellQuote(helper)}`;
    const custom = typeof hook === "string" ? hook : hook?.script;
    let script = stage;
    if (custom) {
      const hookPath = join(noticeDir, "before-bundle.sh");
      writeFileSync(hookPath, `${stage} && exec sh -c ${shellQuote(custom)}\n`);
      script = `exec sh ${shellQuote(hookPath)}`;
    }
    const noticeFiles = (docDir) => ({
      [`${docDir}/LICENSE.txt`]: join(noticeDir, "LICENSE.txt"),
      [`${docDir}/CREDITS.html`]: join(noticeDir, "CREDITS.html"),
    });
    const composedHook =
      hook?.cwd === undefined ? script : { script, cwd: hook.cwd };
    const additions = { build: { [key]: composedHook } };
    if (platformName === "macos") {
      // The macOS files map is relative to Contents and is only read during
      // bundling. Leave the caller's bundle.resources list or map intact.
      const macKey = Object.hasOwn(config?.bundle ?? {}, "macos")
        ? "macos"
        : "macOS";
      additions.bundle = {
        [macKey]: { files: noticeFiles("Resources/cef") },
      };
    } else {
      const linux = {};
      if (debFilesCleared && formats.includes("deb")) {
        // The clear removes the notice defaults. Restore them in the final patch.
        linux.deb = { files: noticeFiles("/usr/share/doc/delidev/cef") };
      }
      if (formats.includes("rpm")) {
        linux.rpm = {
          files: noticeFiles("/usr/share/doc/delidev/cef"),
        };
      }
      if (formats.includes("appimage")) {
        linux.appimage = {
          files: noticeFiles("usr/share/doc/delidev/cef"),
        };
      }
      if (Object.keys(linux).length > 0) additions.bundle = { linux };
    }
    const patch = preserveClears(baseConfig, overrides, additions);
    args.splice(
      commandIndex + 1 + configIndex,
      0,
      "--config",
      JSON.stringify(patch),
    );
    if (!debFilesCleared && formats.includes("deb")) {
      // Preserve the precedence of the Debian file mapping.
      const fileConfig = JSON.stringify({
        bundle: {
          linux: {
            deb: { files: noticeFiles("/usr/share/doc/delidev/cef") },
          },
        },
      });
      args.splice(commandIndex + 1, 0, "--config", fileConfig);
    }

    // A CLI subprocess lets us clean up even when Tauri exits during argument parsing.
    const options = {
      env: { ...process.env, DELIDEV_CEF_NOTICE_DIR: noticeDir },
      stdio: "inherit",
      shell: false,
    };
    if (cli) {
      process.exitCode = await runPatchedCli(cli, args, options);
    } else {
      const cliArgs = [require.resolve("@tauri-apps/cli/tauri.js"), ...args];
      const { error, status } = spawnSync(process.execPath, cliArgs, options);
      if (error) throw error;
      process.exitCode = status ?? 1;
    }
  } finally {
    rmSync(noticeDir, { recursive: true, force: true });
  }
}

try {
  const resolved = resolveBundleConfig();
  const cli = resolved?.usePatchedCli ? await linuxCefCli() : undefined;
  if (resolved?.stageNotices) {
    await runNoticeBuild(resolved, cli);
  } else if (cli) {
    process.exitCode = await runPatchedCli(cli, args, { stdio: "inherit" });
  } else {
    await run(args, "pnpm tauri");
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
