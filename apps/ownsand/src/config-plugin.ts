// SPDX-License-Identifier: Apache-2.0
import type { RsbuildPlugin } from "@rsbuild/core";
import { type ConsoleBuildEnv, readConsoleConfig } from "./config.ts";

/**
 * Validates the console configuration for every Rsbuild environment before
 * compilation, and refuses the mock API when the resolved build mode is
 * `production`.
 *
 * The check must follow the resolved mode, not `process.env.NODE_ENV`: Rsbuild
 * keeps a preset `NODE_ENV` and applies an explicit `--mode` afterwards, while
 * `import.meta.env.PROD` (read by `src/build-config.ts` in the bundle) follows
 * the resolved mode. A check on the inherited `NODE_ENV` would let
 * `NODE_ENV=development rsbuild build --mode production` emit a production
 * bundle that fails in the browser.
 */
export function pluginConsoleConfig(env: ConsoleBuildEnv): RsbuildPlugin {
  return {
    name: "ownsand:console-config",
    setup(api) {
      api.modifyEnvironmentConfig((config) => {
        readConsoleConfig(env, { production: config.mode === "production" });
      });
    },
  };
}
