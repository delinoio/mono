// SPDX-License-Identifier: Apache-2.0
import * as path from "node:path";
import { defineConfig } from "@rspress/core";

// `pnpm dev` runs this server through portless (`dev` script), which assigns
// a free loopback port in `PORT` and sets `PORTLESS_URL`. Read `PORT` only in
// that case, so a `PORT` exported by the shell does not move the fixed port of
// `dev:app` and `preview`.
const devPort =
  process.env.PORTLESS_URL && process.env.PORT
    ? Number(process.env.PORT)
    : 13600;

export default defineConfig({
  root: path.join(__dirname, "docs"),
  // Keep build output in the repository-wide ignored `dist` directory.
  outDir: "dist",
  base: "/",
  builderConfig: {
    server: {
      // Loopback port that `devPort` selects for `rspress dev` and `rspress
      // preview`; fail instead of picking another port on conflict. The
      // Rspress CLI has no strict-port flag, so only this config can express
      // the rule. The e2e suite overrides it with explicit `--host` and
      // `--port` flags.
      host: "127.0.0.1",
      port: devPort,
      strictPort: true,
    },
  },
  lang: "en",
  title: "Delino",
  description: "Delino",
  icon: "/icon.svg",
  logo: "/icon.svg",
  logoText: "Delino",
  // The marketing site has no documentation search; docs.delino.io owns it.
  // This type-valid option still stops Rspress from indexing pages for search,
  // but it does not hide the search UI; see `themeConfig.search` below.
  search: false,
  sitemap: { siteUrl: "https://delino.io" },
  themeConfig: {
    // Documents the intent; the visible search UI is removed by the `Search`
    // override in `theme/index.tsx` until the default theme honours this flag.
    search: false,
    nav: [
      { text: "Docs", link: "https://docs.delino.io" },
      { text: "GitHub", link: "https://github.com/delinoio" },
    ],
  },
});
