// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

export default defineConfig({
  testEnvironment: "node",
  // Bundle workspace source so the unit tests can run before a build.
  resolve: { conditionNames: ["browser", "..."] },
  include: ["src/**/*.test.ts"],
});
