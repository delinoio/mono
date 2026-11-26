// SPDX-License-Identifier: Apache-2.0
import { createRsbuild, type RsbuildMode } from "@rsbuild/core";
import { describe, expect, test } from "@rstest/core";
import { pluginConsoleConfig } from "./config-plugin.ts";
import type { ConsoleBuildEnv } from "./config.ts";

async function initConsoleConfig(mode: RsbuildMode, env: ConsoleBuildEnv) {
  const rsbuild = await createRsbuild({
    rsbuildConfig: {
      mode,
      plugins: [pluginConsoleConfig(env)],
    },
  });
  await rsbuild.initConfigs();
  return rsbuild;
}

async function readMockDefine(mode: RsbuildMode, value: string | undefined) {
  const rsbuild = await initConsoleConfig(mode, {
    PUBLIC_SANDRUN_MOCK_API: value,
  });
  return rsbuild.getNormalizedConfig().environments.web.source.define[
    "import.meta.env.PUBLIC_SANDRUN_MOCK_API"
  ];
}

describe("console configuration plugin", () => {
  test.each(["true", " true "])(
    "rejects the mock flag %j in production mode",
    async (value) => {
      await expect(readMockDefine("production", value)).rejects.toThrow(
        "PUBLIC_SANDRUN_MOCK_API must not be set for a production build",
      );
    },
  );

  test.each(["true", " true "])(
    "inlines the normalized mock flag %j in development mode",
    async (value) => {
      await expect(readMockDefine("development", value)).resolves.toBe(
        '"true"',
      );
    },
  );

  describe.each(["production", "development"] as const)("%s mode", (mode) => {
    test("rejects a Logto endpoint with a path and a blank app ID", async () => {
      await expect(
        initConsoleConfig(mode, {
          PUBLIC_LOGTO_ENDPOINT: "https://login.example/path",
          PUBLIC_LOGTO_APP_ID: "  ",
        }),
      ).rejects.toThrow(
        "PUBLIC_LOGTO_ENDPOINT must be an origin without a path, query or fragment",
      );
    });

    test("initializes with a valid Logto endpoint and a blank app ID", async () => {
      await expect(
        initConsoleConfig(mode, {
          PUBLIC_LOGTO_ENDPOINT: "https://login.example",
          PUBLIC_LOGTO_APP_ID: "  ",
        }),
      ).resolves.toBeDefined();
    });

    test.each([
      ["false", '"false"'],
      [undefined, '""'],
    ])("inlines the disabled mock flag %j", async (value, define) => {
      await expect(readMockDefine(mode, value)).resolves.toBe(define);
    });

    test("rejects a malformed mock flag", async () => {
      await expect(readMockDefine(mode, "yes")).rejects.toThrow(
        "PUBLIC_SANDRUN_MOCK_API must be true or false",
      );
    });
  });
});
