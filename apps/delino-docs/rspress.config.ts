// SPDX-License-Identifier: Apache-2.0
import * as path from "node:path";
import { defineConfig } from "@rspress/core";

export default defineConfig({
  root: path.join(__dirname, "docs"),
  // Keep build output in the repository-wide ignored `dist` directory.
  outDir: "dist",
  base: "/",
  builderConfig: {
    server: {
      // Fixed loopback port for `rspress dev` and `rspress preview`; fail
      // instead of picking another port on conflict. The Rspress CLI has no
      // strict-port flag, so only this config can express the rule. The e2e
      // suite overrides it with explicit `--host` and `--port` flags.
      host: "127.0.0.1",
      port: 3500,
      strictPort: true,
    },
  },
  lang: "en",
  title: "Delino Docs",
  description: "Delino documentation",
  icon: "/icon.svg",
  logo: "/icon.svg",
  logoText: "Delino Docs",
  sitemap: { siteUrl: "https://docs.delino.io" },
  themeConfig: {
    // `nav` and `sidebar` are intentionally unset: defining either one makes
    // Rspress skip its automatic pass, which reads `docs/_nav.json` for the
    // navbar and each directory's `_meta.json` for the sidebar.
    socialLinks: [
      { icon: "github", mode: "link", content: "https://github.com/delinoio" },
    ],
  },
});
