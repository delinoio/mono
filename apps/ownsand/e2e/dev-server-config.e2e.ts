// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { rsbuildBin } from "./rsbuild-bin.ts";

describe("development server configuration", () => {
  for (const [name, value] of [
    ["PUBLIC_API_ORIGIN", "https://@api.invalid.test"],
    ["PUBLIC_LOGTO_ENDPOINT", "https://log\nto.invalid.test"],
  ] as const) {
    test(`rejects a malformed authority in ${name} before listening`, () => {
      const result = spawnSync(
        process.execPath,
        [rsbuildBin, "dev", "--no-env", "--host", "127.0.0.1", "--port", "0"],
        {
          cwd: new URL("..", import.meta.url),
          env: {
            PATH: process.env.PATH,
            HOME: process.env.HOME,
            TMPDIR: process.env.TMPDIR,
            TMP: process.env.TMP,
            TEMP: process.env.TEMP,
            [name]: value,
          },
          encoding: "utf8",
          timeout: 15_000,
        },
      );

      const output = result.stdout + result.stderr;
      expect(result.status, output).toBe(1);
      expect(output).toContain(
        `${name} must use an authority without @, tabs or line breaks`,
      );
      expect(output).not.toContain("invalid.test");
      expect(output).not.toContain("Local:");
    });
  }
});
