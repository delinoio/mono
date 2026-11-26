// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

export function deadlineSkipReason({ needsTimeout = true } = {}) {
  if (process.platform !== "linux") {
    return "deadline fixtures need Linux /proc/uptime";
  }
  try {
    readFileSync("/proc/uptime", "utf8");
  } catch {
    return "/proc/uptime is not readable";
  }
  if (!needsTimeout) return false;

  const version = spawnSync("timeout", ["--version"], {
    encoding: "utf8",
    env: { ...process.env, LC_ALL: "C" },
  });
  return version.status === 0 && version.stdout?.includes("GNU coreutils")
    ? false
    : "GNU coreutils timeout is not available";
}
