// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { normalizeMockFlag } from "./src/config.ts";
import { pluginConsoleConfig } from "./src/config-plugin.ts";

// `pluginConsoleConfig` fails the build and the dev server early when the
// configuration is not valid, and refuses the mock API in a build whose
// resolved mode is production. The Rsbuild CLI loads `.env*` files into
// `process.env` before this file; `src/build-config.ts` inlines the same
// values into the client bundle.
export default defineConfig({
  plugins: [pluginConsoleConfig(process.env), pluginReact()],
  source: {
    define: {
      // Always inline the mock setting, also when it is unset, so the
      // minifier removes the mock API chunk from builds without it. The value
      // is normalized like readConsoleConfig does, so a padded value selects
      // both the mock sign-in and the mock transport.
      "import.meta.env.PUBLIC_OWNSAND_MOCK_API": JSON.stringify(
        normalizeMockFlag(process.env.PUBLIC_OWNSAND_MOCK_API),
      ),
    },
  },
  html: {
    template: "./index.html",
    title: "OwnSand",
    favicon: "./public/icon.svg",
  },
  server: {
    // Fixed loopback port; fail instead of picking another port on conflict.
    host: "127.0.0.1",
    port: 13300,
    strictPort: true,
    // Single-page application: every route serves index.html.
    historyApiFallback: true,
  },
});
