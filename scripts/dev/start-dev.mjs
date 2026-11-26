// SPDX-License-Identifier: Apache-2.0
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { constants } from "node:os";
import { dirname, resolve } from "node:path";
import { startPortlessProxy } from "./start-portless-proxy.mjs";

const require = createRequire(import.meta.url);

async function main() {
  const status = await startPortlessProxy();
  if (status !== 0) return status;

  // Run the installed CLI with Node because shell-free spawn cannot run a
  // node_modules/.bin .cmd shim on Windows.
  const manifest = require.resolve("turbo/package.json");
  const cli = resolve(dirname(manifest), require(manifest).bin.turbo);
  const child = spawn(
    process.execPath,
    [cli, "run", "dev", ...process.argv.slice(2)],
    { stdio: "inherit", shell: false },
  );

  // Turbo receives terminal Ctrl-C directly in the shared process group.
  // Relay a PID-directed SIGTERM once. In a group SIGTERM, the CLI forwards
  // at most once and native Turbo counts its direct and relayed signals as one.
  const signals = ["SIGINT", "SIGTERM", "SIGQUIT", "SIGHUP"];
  const onSignal = (signal) => {
    console.error(JSON.stringify({ event: "root_dev_signal", signal }));
    if (process.platform !== "win32" && signal === "SIGTERM" && !child.killed) {
      child.kill(signal);
    }
  };
  for (const signal of signals) process.on(signal, onSignal);
  try {
    return await new Promise((resolveStatus, reject) => {
      child.once("error", reject);
      child.once("close", (code, signal) => {
        resolveStatus(code ?? 128 + constants.signals[signal]);
      });
    });
  } finally {
    for (const signal of signals) process.off(signal, onSignal);
  }
}

try {
  process.exitCode = await main();
} catch (error) {
  console.error(
    JSON.stringify({ event: "root_dev_failed", reason: error.message }),
  );
  process.exitCode = 1;
}
