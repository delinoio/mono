// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("mock runner profile update masks", () => {
  test("rejects absent and empty masks and accepts a name-only mask", async ({
    app,
    browser,
  }) => {
    await app.open("/runner-profile-update-mask-fixture.html");

    const rejection = {
      errorCode: Code.InvalidArgument,
      errorMessage: "invalid update_mask: must name at least one path",
      unchangedProfile: true,
      unchangedAudit: true,
    };
    await expect(browser.locator("output")).toHaveText(
      JSON.stringify({
        omitted: rejection,
        empty: rejection,
        name: "linux-build-new",
        githubLabel: "sandrun-linux-build-new",
        responseMatchesStored: true,
        addedOneUpdateEvent: true,
      }),
    );
  });
});
