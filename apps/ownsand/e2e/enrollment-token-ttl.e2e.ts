// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Code } from "@connectrpc/connect";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

const clockMs = Date.UTC(2026, 0, 1, 12, 0, 0, 750);
type Fixture = Window["enrollmentTokenFixture"];

const enrollmentTest = test.extend<{
  fixture: {
    create(
      ttl: Parameters<Fixture["create"]>[1],
    ): ReturnType<Fixture["create"]>;
    listIds: Fixture["listIds"];
  };
}>({
  fixture: async ({ app, browser, screen }, use) => {
    await app.open("/enrollment-token.html");
    await expect(
      screen.getByText("Enrollment token RPC fixture ready"),
    ).toBeVisible();
    await use({
      create: (ttl) =>
        browser.evaluate(
          ({ clockMs, ttl }) =>
            window.enrollmentTokenFixture.create(clockMs, ttl),
          { clockMs, ttl },
        ),
      listIds: () =>
        browser.evaluate(() => window.enrollmentTokenFixture.listIds()),
    });
  },
});

describe("enrollment token TTLs", () => {
  enrollmentTest(
    "stores the exact expiry for fractional TTLs and 24 hours",
    async ({ fixture }) => {
      const before = await fixture.listIds();
      const ids: string[] = [];
      for (const { seconds, nanos, expires } of [
        { seconds: 0, nanos: 500_000_000, expires: "1767268801250000000" },
        { seconds: 1, nanos: 500_000_000, expires: "1767268802250000000" },
        { seconds: 86_400, nanos: 0, expires: "1767355200750000000" },
      ]) {
        const result = await fixture.create({ seconds, nanos });
        assert(
          "id" in result,
          `CreateEnrollmentToken rejected a valid TTL: ${JSON.stringify(result)}`,
        );
        expect(result.created).toBe("1767268800750000000");
        expect(result.expires).toBe(expires);
        ids.push(result.id);
      }
      expect(await fixture.listIds()).toEqual([...before, ...ids]);
    },
  );

  enrollmentTest(
    "rejects invalid TTLs without storing a token",
    async ({ fixture }) => {
      const before = await fixture.listIds();
      for (const ttl of [
        null,
        { seconds: 0, nanos: 0 },
        { seconds: 0, nanos: -1 },
        { seconds: 1, nanos: -1 },
        { seconds: 1, nanos: 1_000_000_000 },
        { seconds: 86_400, nanos: 1 },
      ]) {
        expect(await fixture.create(ttl)).toEqual({
          code: Code.InvalidArgument,
        });
        expect(await fixture.listIds()).toEqual(before);
      }
    },
  );
});
