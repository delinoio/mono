// SPDX-License-Identifier: Apache-2.0
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { expect } from "e2e";

describe("sandbox state filter", () => {
  for (const state of [SandboxState.UNSPECIFIED, 99 as SandboxState]) {
    test(`rejects explicit state ${state}`, async ({
      app,
      browser,
      screen,
    }) => {
      await app.open("/sandbox-idempotency.html");
      await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();
      const result = await browser.evaluate(
        (state) => window.listSandboxesForFilterTest({ state }),
        state,
      );
      expect(result).toEqual({
        code: Code.InvalidArgument,
        rawMessage: "invalid state: must be a sandbox state",
      });
    });
  }
});
