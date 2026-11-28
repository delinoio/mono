// SPDX-License-Identifier: Apache-2.0
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
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
    for (const [scenario, value, message] of [
      [
        "a hidden path",
        "https:///api.test/v1/..",
        "must be an origin without a path, query or fragment",
      ],
      [
        "a path with surrounding nonbreaking spaces",
        "\u00a0https://api.example.test/path\u00a0",
        "must be an origin without a path, query or fragment",
      ],
      [
        "an empty user-info delimiter",
        "https://@api.test",
        "must use an authority without @, tabs or line breaks",
      ],
    ]) {
      test(`rejects ${scenario} in ${name} with app ID ${JSON.stringify(appId)} before listening`, () => {
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
              [name]: value,
            },
            encoding: "utf8",
            timeout: 15_000,
          },
        );
        const output = result.stdout + result.stderr;
        expect(result.status, output).toBe(1);
        expect(output).toContain(`${name} ${message}`);
        expect(output).not.toContain(value);
      });
    }
  }
});

describe("preview server configuration", () => {
  test("reports missing build output with the mock flag enabled", () => {
    const fixture = mkdtempSync(join(tmpdir(), "sandrun-preview-"));
    try {
      const result = spawnSync(
        process.execPath,
        [
          rsbuild,
          "preview",
          "--no-env",
          "--mode",
          "production",
          "--dist-path",
          join(fixture, "dist"),
        ],
        {
          cwd: new URL("..", import.meta.url),
          env: {
            PATH: process.env.PATH,
            HOME: process.env.HOME,
            TMPDIR: process.env.TMPDIR,
            TMP: process.env.TMP,
            TEMP: process.env.TEMP,
            PUBLIC_SANDRUN_MOCK_API: "true",
          },
          encoding: "utf8",
          timeout: 15_000,
        },
      );

      const output = result.stdout + result.stderr;
      expect(result.status, output).toBe(1);
      expect(output).toContain("The output directory");
      expect(output).toContain("does not exist, please build the project");
    } finally {
      rmSync(fixture, { recursive: true, force: true });
    }
  });
});
