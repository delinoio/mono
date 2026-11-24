// SPDX-License-Identifier: Apache-2.0
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, isAbsolute, resolve } from "node:path";
import { describe, expect, test } from "@rstest/core";
import ownsandConfig from "../../ownsand/e2e.config.ts";
import tideConfig from "../../tide/e2e.config.ts";
import accountsConfig from "../e2e.config.ts";

// An e2e app command that lets Rsbuild read `.env` and `.env.local` makes the
// suite depend on a developer's local values: an invalid Logto registration
// fails the start, and a valid one changes the pages under test. The app
// command must pass `--no-env`; `rsbuild.config.ts` inlines the `PUBLIC_*`
// values itself in that case.
const appsDirectory = new URL("../../", import.meta.url);

describe("e2e app command", () => {
  for (const { directory, config, configArgs, env } of [
    {
      directory: "delino-accounts",
      config: accountsConfig,
      configArgs: [],
      env: { PUBLIC_API_ORIGIN: "http://127.0.0.1:28100" },
    },
    {
      directory: "ownsand",
      config: ownsandConfig,
      configArgs: ["--config", "e2e/rsbuild.config.ts"],
      env: { PUBLIC_OWNSAND_MOCK_API: " true " },
    },
    {
      directory: "tide",
      config: tideConfig,
      configArgs: ["--mode", "development"],
      env: { NODE_ENV: "production", PUBLIC_TIDE_MOCK_API: " true " },
    },
  ]) {
    test(`starts ${directory} through Node on the selected loopback port`, () => {
      const { url, command } = config.targets[0].app;
      const [cli, ...args] = command.args;
      const packageUrl = new URL(`${directory}/package.json`, appsDirectory);
      const manifestPath = createRequire(packageUrl).resolve(
        "@rsbuild/core/package.json",
      );
      const manifest = JSON.parse(readFileSync(manifestPath, "utf8")) as {
        bin: { rsbuild: string };
      };

      expect(url).toBe("http://127.0.0.1:0");
      expect(command.executable).toBe(process.execPath);
      expect(isAbsolute(cli)).toBe(true);
      expect(statSync(cli).isFile()).toBe(true);
      expect(cli).toBe(resolve(dirname(manifestPath), manifest.bin.rsbuild));
      expect(args).toEqual([
        "dev",
        "--no-env",
        ...configArgs,
        "--host",
        "127.0.0.1",
        "--port",
        "{port}",
      ]);
      expect(command.env).toEqual(env);
      expect(command.log).toBe(".e2e/logs/app.log");
    });
  }

  test("passes --no-env in every app e2e config that has a .env.example", () => {
    const guarded = readdirSync(appsDirectory, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => new URL(`${entry.name}/`, appsDirectory))
      .filter(
        (dir) =>
          existsSync(new URL(".env.example", dir)) &&
          existsSync(new URL("e2e.config.ts", dir)),
      );
    expect(guarded.length).toBeGreaterThanOrEqual(2);
    for (const dir of guarded) {
      const source = readFileSync(new URL("e2e.config.ts", dir), "utf8");
      expect(source, dir.pathname).toContain('"--no-env"');
    }
  });
});
