// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { readAccountsConfig } from "./src/config.ts";

// Fail the build and the dev server early when the configuration (the API
// origin, the Logto registration or the Paddle checkout) is not valid. The
// Rsbuild CLI loads `.env*` files into `process.env` before this file.
// `src/api-origin.ts` and `src/build-config.ts` inline the same validated
// values into the client bundle.
readAccountsConfig(process.env);

export default defineConfig({
  plugins: [pluginReact()],
  html: {
    // Rsbuild can only set attributes on `<html>` through a custom template.
    // The template must not carry a title; Rsbuild injects title and favicon.
    template: "./index.html",
    title: "Delino Accounts",
    favicon: "./public/icon.svg",
  },
  server: {
    // Fixed loopback port; fail instead of picking another port on conflict.
    host: "127.0.0.1",
    port: 13100,
    strictPort: true,
    // Single-page application: every route serves index.html, like the
    // Cloudflare Pages fallback in production.
    historyApiFallback: true,
  },
});
