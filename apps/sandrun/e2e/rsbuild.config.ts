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
      "credential-replacement-fixture":
        "./e2e/credential-replacement-fixture.ts",
      "direct-action-fixture": "./e2e/direct-action-fixture.tsx",
      "leave-organization-fixture": "./e2e/leave-organization-fixture.tsx",
      "runner-group-fixture": "./e2e/runner-group-fixture.tsx",
      "runner-profile-update-mask-fixture":
        "./e2e/runner-profile-update-mask-fixture.ts",
    },
  },
});
