// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { pluginConsoleConfig } from "./src/config-plugin.ts";

export default defineConfig({
  plugins: [
    // Rsbuild adds Fast Refresh only in development mode with HMR. Remove
    // this override when the Rspack plugin also honors that resolved mode
    // with an inherited production NODE_ENV.
    pluginReact({ reactRefreshOptions: { forceEnable: true } }),
    pluginConsoleConfig(process.env),
  ],
  html: {
    template: "./index.html",
    title: "SandRun",
    favicon: "./public/icon.svg",
  },
  server: {
    // Fixed loopback port; fail instead of picking another port on conflict.
    host: "127.0.0.1",
    port: 13400,
    strictPort: true,
    // Single-page application: every route serves index.html.
    historyApiFallback: true,
  },
});
