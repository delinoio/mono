// SPDX-License-Identifier: Apache-2.0
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

// Each fixture owns PATH and records the requests and exit statuses of its ps.
export function createPsFixture(
  dir,
  { pid, command, startTime, mode = "available" } = {},
) {
  if (process.platform === "win32") {
    return { env: {}, readCalls: () => [] };
  }

  const bin = join(dir, "bin");
  const record = join(dir, "ps.jsonl");
  mkdirSync(bin);
  if (mode !== "missing") {
    const ps = join(bin, "ps");
    // Use the test runner's absolute Node path even when PATH is empty.
    writeFileSync(ps, '#!/bin/sh\nexec "$FIXTURE_PS_NODE" "$0.mjs" "$@"\n', {
      mode: 0o700,
    });
    writeFileSync(
      `${ps}.mjs`,
      `import { isDeepStrictEqual } from "node:util";
import { appendFileSync } from "node:fs";
const args = process.argv.slice(2);
let output;
if (${JSON.stringify(mode)} === "available") {
  const pid = ${JSON.stringify(String(pid))};
  if (isDeepStrictEqual(args, ["-o", "command=", "-p", pid])) {
    output = ${JSON.stringify(command)};
  } else if (
    isDeepStrictEqual(args, ["-o", "stat=", "-o", "lstart=", "-p", pid]) &&
    process.env.LC_ALL === "C" &&
    process.env.TZ === "UTC"
  ) {
    const startTime = ${JSON.stringify(startTime)};
    if (startTime !== undefined) output = "S  " + startTime;
  }
}
const status = output === undefined ? 1 : 0;
appendFileSync(${JSON.stringify(record)}, JSON.stringify({ args, status }) + "\\n");
if (output !== undefined) console.log(output);
process.exitCode = status;
`,
    );
  }

  return {
    env: { PATH: bin, FIXTURE_PS_NODE: process.execPath },
    readCalls() {
      if (!existsSync(record)) return [];
      return readFileSync(record, "utf8").trim().split("\n").map(JSON.parse);
    },
  };
}
