// SPDX-License-Identifier: Apache-2.0
import type { RsbuildPlugin } from "@rsbuild/core";
import {
  type ConsoleBuildEnv,
  normalizeMockFlag,
  readConsoleConfig,
} from "./config.ts";

export function pluginConsoleConfig(env: ConsoleBuildEnv): RsbuildPlugin {
  return {
    name: "sandrun:console-config",
    setup(api) {
      api.modifyEnvironmentConfig((config) => {
        // The resolved mode also sets import.meta.env.PROD in the bundle.
        readConsoleConfig(env, { production: config.mode === "production" });

        // Inline the normalized flag even when unset so the minifier can
        // drop the mock transport and padded true enables it with mock sign-in.
        config.source.define["import.meta.env.PUBLIC_SANDRUN_MOCK_API"] =
          JSON.stringify(normalizeMockFlag(env.PUBLIC_SANDRUN_MOCK_API));
      });
    },
  };
}
