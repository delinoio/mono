// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

export default defineConfig({
  testEnvironment: "node",
  // Use workspace source for the modules that Rstest bundles.
  resolve: { conditionNames: ["browser", "..."] },
  include: ["src/**/*.test.ts"],
});
