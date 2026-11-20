// SPDX-License-Identifier: Apache-2.0
import { defineConfig } from "@rstest/core";

export default defineConfig({
  testEnvironment: "node",
  include: ["test/**/*.test.ts"],
});
