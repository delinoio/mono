// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("sandbox create idempotency", () => {
  test("reuses a key after 24 hours even when the first reuse fails", async ({
    app,
    browser,
  }) => {
    await app.open("/sandbox-idempotency.html");
    const start = Date.UTC(2026, 0, 1);
    const expiry = start + 24 * 60 * 60 * 1000;
    const create = (
      timeMs: number,
      team: string,
      deviceId: string | null = null,
    ) =>
      browser.evaluate(
        ({ timeMs, team, deviceId }) =>
          window.createSandboxForIdempotencyTest(timeMs, team, deviceId),
        { timeMs, team, deviceId },
      );

    const first = await create(start, "ci");
    assert("id" in first, "The initial create did not return a sandbox");
    expect(first.team).toBe("ci");
    expect(await create(expiry - 1, "ci")).toEqual(first);
    expect(await create(expiry - 1, "other")).toEqual({
      code: Code.AlreadyExists,
    });

    const unknownDevice = "0193a000-0000-7000-8000-0000000000f2";
    expect(await create(expiry, "other", unknownDevice)).toEqual({
      code: Code.NotFound,
    });
    const second = await create(expiry, "other");
    assert("id" in second, "The create after expiry did not return a sandbox");
    expect(second.team).toBe("other");
    expect(second.id).not.toBe(first.id);
    expect(await create(expiry, "other")).toEqual(second);
  });
});
