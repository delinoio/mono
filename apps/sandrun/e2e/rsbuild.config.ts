// SPDX-License-Identifier: Apache-2.0
import { fileURLToPath } from "node:url";
import { mergeRsbuildConfig } from "@rsbuild/core";
import consoleConfig from "../rsbuild.config.ts";

const fixtureGuestOses = fileURLToPath(
  new URL("./profile-integration-guest-oses.ts", import.meta.url),
);

export default mergeRsbuildConfig(consoleConfig, {
  resolve: {
    alias: {
      "./mock/mock-api.ts$": fileURLToPath(
        new URL("./profile-integration-mock.ts", import.meta.url),
      ),
      "./verified-codex-guest-oses.ts$": fixtureGuestOses,
      "../verified-codex-guest-oses.ts$": fixtureGuestOses,
    },
  },
  source: {
    entry: {
      index: "./src/index.tsx",
      "direct-action-fixture": "./e2e/direct-action-fixture.tsx",
    },
  },
});
