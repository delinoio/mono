// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Code } from "@connectrpc/connect";
import { Arch, BaseImage, HostOs } from "@delino/ownsand-protos/v1/common_pb";
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
    for (const [idempotencyKey, placement, code] of [
      [uuidV7(), { hostOs: HostOs.WINDOWS }, Code.FailedPrecondition],
      [uuidV7(), { arch: Arch.ARM64 }, Code.Unavailable],
      [
        key,
        { hostOs: HostOs.LINUX, arch: Arch.ARM64 },
        Code.FailedPrecondition,
      ],
    ] as const) {
      expect(await createSandbox(browser, idempotencyKey, placement)).toEqual({
        code,
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
const key = "0193a000-0000-7000-8000-0000000000f1";

describe("automatic sandbox placement", () => {
  for (const { name, image, windows, code } of [
    {
      name: "the seeded macOS device is offline",
      image: BaseImage.MACOS,
      windows: null,
      code: Code.Unavailable,
    },
    {
      name: "the only compatible Windows device is offline",
      image: BaseImage.WINDOWS,
      windows: "offline",
      code: Code.Unavailable,
    },
    {
      name: "no device supports Windows",
      image: BaseImage.WINDOWS,
      windows: null,
      code: Code.FailedPrecondition,
    },
  ] as const) {
    test(name, async ({ app, browser, screen }) => {
      await app.open("/sandbox-idempotency.html");
      await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();

      const result = await browser.evaluate(
        async ({ image, windows, key, retryImage }) => {
          const fixture = window.sandboxPlacementFixture(windows ?? undefined);
          const before = await fixture.listIds();
          const rejection = await fixture.create(image, key);
          const after = await fixture.listIds();
          const retry = await fixture.create(retryImage, key);
          return {
            rejection,
            before,
            after,
            retry,
            buildBoxId: fixture.buildBoxId,
          };
        },
        { image, windows, key, retryImage: BaseImage.LINUX },
      );

      expect(result.rejection).toEqual({ code });
      expect(result.after).toEqual(result.before);
      expect(result.retry).toEqual({ deviceId: result.buildBoxId });
    });
  }

  test("uses the online device when both devices support the image", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();

    const { automatic, explicit, buildBoxId } = await browser.evaluate(
      async ({ image, key, explicitKey }) => {
        const fixture = window.sandboxPlacementFixture("both");
        return {
          automatic: await fixture.create(image, key),
          explicit: await fixture.create(
            image,
            explicitKey,
            fixture.studioMacId,
          ),
          buildBoxId: fixture.buildBoxId,
        };
      },
      {
        image: BaseImage.WINDOWS,
        key,
        explicitKey: "0193a000-0000-7000-8000-0000000000f2",
      },
    );

    expect(automatic).toEqual({ deviceId: buildBoxId });
    expect(explicit).toEqual({ code: Code.Unavailable });
  });

  test("excludes a removed device from automatic placement", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();

    const { offline, removed } = await browser.evaluate(
      async ({ image, key }) => {
        const fixture = window.sandboxPlacementFixture("offline");
        const offline = await fixture.create(image, key);
        await fixture.removeStudioMac();
        const removed = await fixture.create(image, key);
        return { offline, removed };
      },
      { image: BaseImage.WINDOWS, key },
    );

    expect(offline).toEqual({ code: Code.Unavailable });
    expect(removed).toEqual({ code: Code.FailedPrecondition });
  });
});
