// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { extname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { run } from "@tauri-apps/cli";

const args = process.argv.slice(2);
const commandIndex = args.findIndex((arg) => !arg.startsWith("-"));
const command = args[commandIndex];
const options = args.slice(commandIndex + 1);
const forwardedIndex = options.indexOf("--");
const tauriOptions =
  forwardedIndex === -1 ? options : options.slice(0, forwardedIndex);
const require = createRequire(import.meta.url);

function mayBundleDebian(options) {
  let explicitFormats = false;
  let requestsDebian = false;
  for (const [index, option] of options.entries()) {
    const target = /^(?:--target(?:=(.*))?|-[dv]*t(?:=?(.*))?)$/.exec(option);
    if (target) {
      const triple = (target[1] ?? target[2]) || options[index + 1];
      if (triple && !triple.includes("-linux-")) return false;
    }
    const format = /^(?:--bundles(?:=(.*))?|-[dv]*b(?:=?(.*))?)$/.exec(option);
    if (!format) continue;

    explicitFormats = true;
    const requestedFormats = [format[1] ?? format[2] ?? ""];
    for (const value of options.slice(index + 1)) {
      if (value.startsWith("-")) break;
      requestedFormats.push(value);
    }
    if (requestedFormats.flatMap((value) => value.split(",")).includes("deb")) {
      requestsDebian = true;
    }
  }
  // With no format option Tauri uses the app configuration, which defaults to all bundles.
  return !explicitFormats || requestsDebian;
}

function mergeBuildConfig(base, patch, deleteNulls) {
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
      merged[key] = mergeBuildConfig(merged[key], value, deleteNulls);
    }
  }
  return merged;
}

function readConfig(value, probeDir) {
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

function resolveHook(probeDir) {
  const tauriDir = fileURLToPath(new URL("./src-tauri/", import.meta.url));
  const base = ["tauri.conf.json", "tauri.conf.json5", "Tauri.toml"]
    .map((file) => join(tauriDir, file))
    .find(existsSync);
  const platform = [
    "tauri.linux.conf.json",
    "tauri.linux.conf.json5",
    "Tauri.linux.toml",
  ]
    .map((file) => join(tauriDir, file))
    .find(existsSync);

  let overrides = {};
  let configIndex = 0;
  for (let index = 0; index < tauriOptions.length; index++) {
    const option = tauriOptions[index];
    const config = /^(?:--config(?:=(.*))?|-[dv]*c(?:=?(.*))?)$/.exec(option);
    if (!config) continue;

    const value = (config[1] ?? config[2]) || tauriOptions[++index];
    const build = readConfig(value, probeDir)?.build;
    if (build !== undefined) {
      overrides = mergeBuildConfig(overrides, { build }, false);
    }
    configIndex = index + 1;
  }

  // Tauri merges the platform patch into the base first. It combines CLI
  // patches separately so their null values can remove base or platform keys.
  let { build } = readConfig(base, probeDir);
  if (platform) {
    const config = readConfig(platform, probeDir);
    if (config.build !== undefined) {
      build = mergeBuildConfig(build, config.build, true);
    }
  }
  build = mergeBuildConfig({ build }, overrides, true).build ?? {};
  const key = Object.hasOwn(build, "before-bundle-command")
    ? "before-bundle-command"
    : "beforeBundleCommand";
  return { hook: build[key], key, configIndex };
}

function shellQuote(value) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

function runDebianBuild() {
  // Tauri reads its file mappings before Cargo downloads CEF. Stage the files
  // in a private directory after the download and before Tauri bundles the app.
  const noticeDir = mkdtempSync(join(tmpdir(), "delidev-cef-"));
  try {
    const { hook, key, configIndex } = resolveHook(noticeDir);
    const helper = fileURLToPath(
      new URL("./cef-deb-notices.mjs", import.meta.url),
    );
    const stage = `${shellQuote(process.execPath)} ${shellQuote(helper)}`;
    const custom = typeof hook === "string" ? hook : hook?.script;
    let script = stage;
    if (custom) {
      const hookPath = join(noticeDir, "before-bundle.sh");
      writeFileSync(hookPath, `${stage} && exec sh -c ${shellQuote(custom)}\n`);
      script = `exec sh ${shellQuote(hookPath)}`;
    }
    const docDir = "/usr/share/doc/delidev/cef";
    const files = {
      [`${docDir}/LICENSE.txt`]: join(noticeDir, "LICENSE.txt"),
      [`${docDir}/CREDITS.html`]: join(noticeDir, "CREDITS.html"),
    };
    const composedHook =
      hook?.cwd === undefined ? script : { script, cwd: hook.cwd };
    const hookConfig = JSON.stringify({ build: { [key]: composedHook } });
    // Put the hook after the user overrides. Keep the Debian mapping at its
    // original precedence so other file mappings still merge as before.
    args.splice(commandIndex + 1 + configIndex, 0, "--config", hookConfig);
    const fileConfig = JSON.stringify({
      bundle: { linux: { deb: { files } } },
    });
    args.splice(commandIndex + 1, 0, "--config", fileConfig);

    // A CLI subprocess lets us clean up even when Tauri exits during argument parsing.
    const cli = require.resolve("@tauri-apps/cli/tauri.js");
    const { error, status } = spawnSync(process.execPath, [cli, ...args], {
      env: { ...process.env, DELIDEV_CEF_NOTICE_DIR: noticeDir },
      stdio: "inherit",
    });
    if (error) throw error;
    process.exitCode = status ?? 1;
  } finally {
    rmSync(noticeDir, { recursive: true, force: true });
  }
}

try {
  if (
    process.platform === "linux" &&
    (command === "build" || command === "bundle") &&
    !tauriOptions.some(
      (option) => option === "--help" || /^-[dv]*h$/.test(option),
    ) &&
    !tauriOptions.includes("--no-bundle") &&
    mayBundleDebian(tauriOptions)
  ) {
    runDebianBuild();
  } else {
    await run(args, "pnpm tauri");
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
