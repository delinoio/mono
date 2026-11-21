// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rsbuild/core";
import { pluginReact } from "@rsbuild/plugin-react";

// `tauri android dev` and `tauri ios dev` on a physical device set
// `TAURI_DEV_HOST` to a LAN address that the device can reach. Desktop
// development and emulators keep the loopback host.
const devHost = process.env.TAURI_DEV_HOST || "127.0.0.1";

export default defineConfig({
  plugins: [pluginReact()],
  html: {
    // Rsbuild can only set attributes on `<html>` through a custom template.
    // The template must not carry a title; Rsbuild injects title and favicon.
    template: "./index.html",
    title: "DeliDev",
    favicon: "./public/icon.svg",
  },
  server: {
    // Fixed port that `build.devUrl` in `src-tauri/tauri.conf.json` names;
    // fail instead of picking another port on conflict.
    host: devHost,
    port: 13200,
    strictPort: true,
  },
});
