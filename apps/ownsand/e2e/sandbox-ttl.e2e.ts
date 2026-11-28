// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { fromJsonString } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import { CreateSandboxRequestSchema } from "@delino/ownsand-protos/v1/sandbox_pb";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

describe("sandbox create TTLs", () => {
  test("rejects an over-limit TTL and replays a corrected subsecond create", async ({
    app,
    browser,
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();
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
    screen,
  }) => {
    await app.open("/sandbox-idempotency.html");
    await expect(screen.getByText("Sandbox RPC fixture ready")).toBeVisible();
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
    const sandboxesPath =
      "/orgs/0193a000-0000-7000-8000-000000000001/sandboxes";
    await app.open("/");
    await screen.getByRole("button", "Sign in with Delino").tap();
    await expect(screen.getByRole("link", "Acme Robotics Owner")).toBeVisible();
    await browser.evaluate(() => {
      sessionStorage.setItem("ownsand-e2e-create-response", "valid");
      return true;
    });
    await app.open(sandboxesPath);
    await screen.getByRole("button", "Create sandbox").tap();
    const dialog = screen.getByRole("dialog", "Create a sandbox");
    await dialog.getByRole("button", "Create").tap();

    await expect(browser).toHaveURL(/\/sandboxes\/[0-9a-f-]+$/);
    const [recordedCreate] = await browser.evaluate(
      () =>
        JSON.parse(sessionStorage.getItem("ownsand-e2e-creates") ?? "[]") as {
          request: string;
          sandboxId: string;
        }[],
    );
    assert(recordedCreate, "The fixture did not record CreateSandbox");
    await expect(browser).toHaveURL(
      `${sandboxesPath}/${recordedCreate.sandboxId}`,
    );

    const { idleTtl, maxTtl } = fromJsonString(
      CreateSandboxRequestSchema,
      recordedCreate.request,
    );
    expect(idleTtl?.seconds, "Idle TTL seconds").toBe(3_600n);
    expect(idleTtl?.nanos, "Idle TTL nanoseconds").toBe(0);
    expect(maxTtl?.seconds, "Maximum TTL seconds").toBe(86_400n);
    expect(maxTtl?.nanos, "Maximum TTL nanoseconds").toBe(0);

    const detail = (term: string) =>
      browser.locator(`dt:text-is("${term}") + dd`);
    await expect(detail("Idle TTL")).toHaveText("1 h");
    await expect(detail("Maximum TTL")).toHaveText("1 d");
    for (const term of ["Idle deadline", "Maximum deadline"]) {
      const deadline = detail(term);
      await expect(deadline).toBeVisible();
      await expect(deadline).toHaveText(/\S/);
      await expect(deadline).not.toHaveText("—");
    }
    await app.screenshot("sandbox-created-with-default-ttls");
  });
});
