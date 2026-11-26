// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const manifestPath = createRequire(import.meta.url).resolve(
  "@rsbuild/core/package.json",
);
const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
  bin: { rsbuild: string };
};
const rsbuild = resolve(dirname(manifestPath), manifest.bin.rsbuild);

describe("development server configuration", () => {
  for (const [name, appId] of [
    ["PUBLIC_API_ORIGIN", "sandrun-local-fixture"],
    ["PUBLIC_LOGTO_ENDPOINT", "sandrun-local-fixture"],
    ["PUBLIC_LOGTO_ENDPOINT", "   "],
  ] as const) {
    test(`rejects a hidden path in ${name} with app ID ${JSON.stringify(appId)} before listening`, () => {
      const result = spawnSync(
        process.execPath,
        [rsbuild, "dev", "--no-env", "--host", "127.0.0.1", "--port", "0"],
        {
          cwd: new URL("..", import.meta.url),
          env: {
            PATH: process.env.PATH,
            HOME: process.env.HOME,
            TMPDIR: process.env.TMPDIR,
            TMP: process.env.TMP,
            TEMP: process.env.TEMP,
            PUBLIC_LOGTO_APP_ID: appId,
            [name]: "https:///api.test/v1/..",
          },
          encoding: "utf8",
          timeout: 15_000,
        },
      );
      const output = result.stdout + result.stderr;
      expect(result.status, output).toBe(1);
      expect(output).toContain(
        `${name} must be an origin without a path, query or fragment`,
      );
    });
  }
});
