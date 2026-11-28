// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

export default defineConfig({
  // Pages need a DOM for routing, dialogs and focus.
  testEnvironment: "happy-dom",
  include: ["src/**/*.test.{ts,tsx}"],
  tools: {
    swc: {
      jsc: {
        transform: {
          react: { runtime: "automatic" },
        },
      },
    },
  },
});
