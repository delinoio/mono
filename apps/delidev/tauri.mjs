// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { run } from "@tauri-apps/cli";

const args = process.argv.slice(2);
const commandIndex = args.findIndex((arg) => !arg.startsWith("-"));
const command = args[commandIndex];
const options = args.slice(commandIndex + 1);
const forwardedIndex = options.indexOf("--");
const tauriOptions =
  forwardedIndex === -1 ? options : options.slice(0, forwardedIndex);

function mayBundleDebian(options) {
  let explicitFormats = false;
  for (const [index, option] of options.entries()) {
    const format = /^(?:--bundles(?:=(.*))?|-[dv]*b(?:=?(.*))?)$/.exec(option);
    if (!format) continue;

    explicitFormats = true;
    const requestedFormats = [format[1] ?? format[2] ?? ""];
    for (const value of options.slice(index + 1)) {
      if (value.startsWith("-")) break;
      requestedFormats.push(value);
    }
    if (requestedFormats.flatMap((value) => value.split(",")).includes("deb")) {
      return true;
    }
  }
  // With no format option Tauri uses the app configuration, which defaults to all bundles.
  return !explicitFormats;
}

function runDebianBuild() {
  // Tauri reads its file mappings before Cargo downloads CEF. Stage the files
  // in a private directory after the download and before Tauri bundles the app.
  const noticeDir = mkdtempSync(join(tmpdir(), "delidev-cef-"));
  try {
    const docDir = "/usr/share/doc/delidev/cef";
    const files = {
      [`${docDir}/LICENSE.txt`]: join(noticeDir, "LICENSE.txt"),
      [`${docDir}/CREDITS.html`]: join(noticeDir, "CREDITS.html"),
    };
    const config = {
      build: { beforeBundleCommand: "node cef-deb-notices.mjs" },
      bundle: { linux: { deb: { files } } },
    };
    args.splice(commandIndex + 1, 0, "--config", JSON.stringify(config));

    // A CLI subprocess lets us clean up even when Tauri exits during argument parsing.
    const cli = createRequire(import.meta.url).resolve(
      "@tauri-apps/cli/tauri.js",
    );
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
