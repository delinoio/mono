// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Code } from "@connectrpc/connect";
import { Arch, HostOs } from "@delino/ownsand-protos/v1/common_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";
import { uuidV7 } from "../src/uuid.ts";

function createSandbox(
  browser: Browser,
  idempotencyKey: string,
  placement: { deviceId?: string; hostOs?: HostOs; arch?: Arch } | null = null,
) {
  return browser.evaluate(
    ({ idempotencyKey, placement }) =>
      window.createSandboxWithPlacement(idempotencyKey, placement ?? undefined),
    { idempotencyKey, placement },
  );
}

describe("sandbox platform placement", () => {
  test("automatic placement filters platforms before reserving a key", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();
    const list = () => browser.evaluate(() => window.listSandboxIds());
    const before = await list();
    const key = uuidV7();
    for (const [idempotencyKey, placement] of [
      [uuidV7(), { hostOs: HostOs.WINDOWS }],
      [uuidV7(), { arch: Arch.ARM64 }],
      [key, { hostOs: HostOs.LINUX, arch: Arch.ARM64 }],
    ] as const) {
      expect(await createSandbox(browser, idempotencyKey, placement)).toEqual({
        code: Code.FailedPrecondition,
      });
      expect(await list()).toEqual(before);
    }

    const linuxAmd64 = { hostOs: HostOs.LINUX, arch: Arch.AMD64 };
    const created = await createSandbox(browser, key, linuxAmd64);
    assert(
      "sandboxId" in created,
      "Matching placement did not create a sandbox",
    );
    expect(created).toEqual({
      sandboxId: created.sandboxId,
      deviceId: MOCK_IDS.buildBox,
      ...linuxAmd64,
    });
    expect(await createSandbox(browser, key, linuxAmd64)).toEqual(created);
    expect(await list()).toEqual([...before, created.sandboxId]);

    for (const placement of [
      null,
      { hostOs: HostOs.UNSPECIFIED, arch: Arch.UNSPECIFIED },
    ]) {
      expect(await createSandbox(browser, uuidV7(), placement)).toMatchObject({
        deviceId: MOCK_IDS.buildBox,
      });
    }
  });

  test("an explicit device's platform takes precedence over availability", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();
    const list = () => browser.evaluate(() => window.listSandboxIds());
    const before = await list();
    for (const [placement, code] of [
      [
        { deviceId: MOCK_IDS.buildBox, arch: Arch.ARM64 },
        Code.FailedPrecondition,
      ],
      [
        { deviceId: MOCK_IDS.studioMac, hostOs: HostOs.LINUX },
        Code.FailedPrecondition,
      ],
      [
        {
          deviceId: MOCK_IDS.studioMac,
          hostOs: HostOs.MACOS,
          arch: Arch.ARM64,
        },
        Code.Unavailable,
      ],
    ] as const) {
      expect(await createSandbox(browser, uuidV7(), placement)).toEqual({
        code,
      });
      expect(await list()).toEqual(before);
    }
  });
});
