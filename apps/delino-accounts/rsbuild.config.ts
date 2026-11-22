// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";
import { readAccountsConfig } from "./src/config.ts";

// Every build-time variable of the web app. Keep this list equal to
// `AccountsAppBuildEnv` in `src/config.ts` and `turbo.json`.
const PUBLIC_ENV_NAMES = [
  "PUBLIC_API_ORIGIN",
  "PUBLIC_LOGTO_ENDPOINT",
  "PUBLIC_LOGTO_APP_ID",
  "PUBLIC_PADDLE_CLIENT_TOKEN",
  "PUBLIC_PADDLE_ENVIRONMENT",
] as const;

// Fail the build and the dev server early when the configuration (the API
// origin, the Logto registration or the Paddle checkout) is not valid. The
// Rsbuild CLI loads `.env*` files into `process.env` before this file, unless
// it runs with `--no-env`, which the e2e app command uses.
// `src/api-origin.ts` and `src/build-config.ts` inline the same validated
// values into the client bundle.
readAccountsConfig(process.env);

export default defineConfig({
  plugins: [pluginReact()],
  source: {
    // Always inline the `PUBLIC_*` values from `process.env`. Without the
    // dotenv step (`rsbuild dev --no-env`) the Rsbuild CLI does not expose
    // them to `import.meta.env`, so the e2e suite would lose the variables
    // that its app command passes. With the dotenv step, `process.env` already
    // holds the loaded values and these definitions equal what Rsbuild inlines.
    // An unset variable becomes an empty string, which the readers in
    // `src/config.ts` treat like an unset one. Remove this workaround only if
    // the e2e app command stops using `--no-env`.
    define: Object.fromEntries(
      PUBLIC_ENV_NAMES.map((name) => [
        `import.meta.env.${name}`,
        JSON.stringify(process.env[name] ?? ""),
      ]),
    ),
  },
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
