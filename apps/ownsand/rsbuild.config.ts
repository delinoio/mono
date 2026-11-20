// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { E2E_FAKE_AUTH_VARIABLE, readConsoleConfig } from "./src/config.ts";

export default defineConfig(({ command }) => {
  // Fail the build and the dev server early when the configuration is not
  // valid. The Rsbuild CLI loads `.env*` files into `process.env` first.
  const config = readConsoleConfig(process.env);
  // The e2e sign-in stand-in exists only for development servers. Refuse a
  // production build that asks for it, in addition to the `DEV` guard in
  // src/index.tsx that drops the module from production bundles.
  if (command !== "dev" && config.e2eFakeAuth) {
    throw new Error(
      `${E2E_FAKE_AUTH_VARIABLE} is allowed only for rsbuild dev`,
    );
  }
  return {
    plugins: [pluginReact()],
    html: {
      // The template sets `lang` on `<html>`; Rsbuild injects title and icon.
      template: "./index.html",
      title: "OwnSand",
      favicon: "./public/icon.svg",
    },
    server: {
      // Fixed loopback port; fail instead of picking another port on conflict.
      host: "127.0.0.1",
      port: 3300,
      strictPort: true,
    },
  };
});
