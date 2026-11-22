// SPDX-License-Identifier: Apache-2.0
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { describe, expect, test } from "@rstest/core";
import e2eConfig from "../e2e.config.ts";

// An e2e app command that lets Rsbuild read `.env` and `.env.local` makes the
// suite depend on a developer's local values: an invalid Logto registration
// fails the start, and a valid one changes the pages under test. The app
// command must pass `--no-env`; `rsbuild.config.ts` inlines the `PUBLIC_*`
// values itself in that case.
const appsDirectory = new URL("../../", import.meta.url);

describe("e2e app command", () => {
  test("starts the Rsbuild development server without dotenv files", () => {
    const command = e2eConfig.targets[0]?.app.command;
    expect(command?.executable).toBe("rsbuild");
    expect(command?.args).toEqual([
      "dev",
      "--no-env",
      "--host",
      "127.0.0.1",
      "--port",
      "{port}",
    ]);
  });

  test("passes --no-env in every app e2e config that has a .env.example", () => {
    const guarded = readdirSync(appsDirectory, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => new URL(`${entry.name}/`, appsDirectory))
      .filter(
        (dir) =>
          existsSync(new URL(".env.example", dir)) &&
          existsSync(new URL("e2e.config.ts", dir)),
      );
    // delino-accounts and ownsand-console read dotenv files today.
    expect(guarded.length).toBeGreaterThanOrEqual(2);
    for (const dir of guarded) {
      const source = readFileSync(new URL("e2e.config.ts", dir), "utf8");
      expect(source, dir.pathname).toContain('"--no-env"');
    }
  });
});
