// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("sandbox create TTLs", () => {
  test("rejects an over-limit TTL and replays a corrected subsecond create", async ({
    app,
    browser,
  }) => {
    await app.open("/sandbox-idempotency.html");
    const start = 1_750_000_000_999;
    const sevenDaysNanos = 604_800_000_000_000;
    const list = () => browser.evaluate(() => window.listSandboxIds());
    const create = (clockMs: number, idleNanos: number, maxNanos: number) =>
      browser.evaluate(
        ({ clockMs, idleNanos, maxNanos }) =>
          window.createSandboxWithTtls(clockMs, 0, idleNanos, maxNanos),
        { clockMs, idleNanos, maxNanos },
      );

    const before = await list();
    expect(await create(start, 500_000, sevenDaysNanos + 1)).toEqual({
      code: Code.InvalidArgument,
    });
    expect(await list()).toEqual(before);

    const first = await create(start, 500_000, 500_000_000);
    assert("id" in first, "The corrected create did not return a sandbox");
    expect(first.created).toBe("1750000000999000000");
    expect(first.idleOffset).toBe(500_000);
    expect(first.maxOffset).toBe(500_000_000);
    expect(await create(start + 1_000, 500_000, 500_000_000)).toEqual(first);
    expect(await list()).toEqual([...before, first.id]);
  });

  test("sets equal deadlines at the seven-day limit under an advancing clock", async ({
    app,
    browser,
  }) => {
    await app.open("/sandbox-idempotency.html");
    const sevenDaysNanos = 604_800_000_000_000;
    const result = await browser.evaluate(
      (ttl) => window.createSandboxWithTtls(1_750_000_000_000, 1, ttl, ttl),
      sevenDaysNanos,
    );

    assert("id" in result, "The seven-day create did not return a sandbox");
    expect(result.idleOffset).toBe(sevenDaysNanos);
    expect(result.maxOffset).toBe(sevenDaysNanos);
  });

  test("creates a sandbox with the console form defaults", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await app.open("/orgs/0193a000-0000-7000-8000-000000000001/sandboxes");
    await screen.getByRole("button", "Create sandbox").tap();
    const dialog = screen.getByRole("dialog", "Create a sandbox");
    await dialog.getByRole("button", "Create").tap();

    await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);
    await expect(
      screen.getByText("Idle deadline", { exact: true }),
    ).toBeVisible();
    await expect(
      screen.getByText("Maximum deadline", { exact: true }),
    ).toBeVisible();
    await app.screenshot("sandbox-created-with-default-ttls");
  });
});
