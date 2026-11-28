// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

export default defineConfig({
  // `cli/` tests use `node:test` and run through `node --test`.
  include: ["src/**/*.test.{ts,tsx}"],
  // The React tests render into a DOM.
  testEnvironment: "happy-dom",
  // Match `"jsx": "react-jsx"` in tsconfig.json. Sources do not import React.
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
