// SPDX-License-Identifier: Apache-2.0
import { mergeRsbuildConfig } from "@rsbuild/core";
import accountsConfig from "../rsbuild.config.ts";

// The browser suite can exercise protected routes with a synthetic account.
export default mergeRsbuildConfig(accountsConfig, {
  source: {
    entry: {
      index: "./src/index.tsx",
      "auto-top-up": "./e2e/auto-top-up-fixture.tsx",
    },
  },
});
