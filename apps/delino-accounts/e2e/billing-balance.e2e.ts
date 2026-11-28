// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  GetBillingAccountResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  CreditService,
  GetBalanceResponseSchema,
  GetSubscriptionResponseSchema,
  SubscriptionStatus,
} from "@delino/accounts-protos/v1/credit_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const ACCOUNT_ID = "0193a1b2-0000-7000-8000-000000000001";
const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;
const DEBT_EXPLANATION =
  "The balance is negative. The next credit pays off the debt first.";

async function interceptOverview(
  browser: Browser,
  appOrigin: string,
  balanceUsdMicros: bigint,
  callerRole = BillingAccountRole.ADMIN,
  status = SubscriptionStatus.ACTIVE,
) {
  await seedFixtureSession(browser, appOrigin);
  const responses = new Map([
    [
      "GetBillingAccount",
      toBinary(
        GetBillingAccountResponseSchema,
        create(GetBillingAccountResponseSchema, {
          billingAccount: {
            billingAccountId: ACCOUNT_ID,
            name: "Acme",
            callerRole,
          },
        }),
      ),
    ],
    [
      CreditService.method.getBalance.name,
      toBinary(
        GetBalanceResponseSchema,
        create(GetBalanceResponseSchema, { balanceUsdMicros }),
      ),
    ],
    [
      CreditService.method.getSubscription.name,
      toBinary(
        GetSubscriptionResponseSchema,
        create(GetSubscriptionResponseSchema, { subscription: { status } }),
      ),
    ],
  ]);
  await browser.route(`${API_ORIGIN}/**`, async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }
    const method = new URL(route.request.url).pathname.split("/").pop() ?? "";
    const body = responses.get(method);
    if (route.request.method !== "POST" || body === undefined) {
      await route.fulfill({ status: 404, headers });
      return;
    }
    await route.fulfill({ contentType: "application/proto", headers, body });
  });
}

describe("billing account balance", () => {
  for (const [width, micros, text, wraps] of [
    [320, 9223372036854775807n, "$9,223,372,036,854.78", true],
    [320, -9223372036854775808n, "-$9,223,372,036,854.78", true],
    [320, 25000000n, "$25.00", false],
    [1024, 9223372036854775807n, "$9,223,372,036,854.78", false],
    [1024, -9223372036854775808n, "-$9,223,372,036,854.78", false],
  ] as const) {
    test(`fits ${text} inside the Balance card at ${width} CSS px`, async ({
      app,
      browser,
      screen,
    }) => {
      await browser.setViewport({ width, height: 900 });
      await interceptOverview(browser, app.baseUrl, micros);
      await app.open(ACCOUNT_PATH);

      const card = screen.getByRole("region", "Balance", { exact: true });
      const amount = browser.locator(
        'section[aria-labelledby="balance-title"] > .balance',
      );
      await expect(amount).toHaveText(text);
      const debt = card.getByText(DEBT_EXPLANATION);
      if (micros < 0n) {
        await expect(browser).toHaveClass(amount, "balance balance-debt");
        await expect(debt).toBeVisible();
      } else {
        await expect(browser).toHaveClass(amount, "balance");
        await expect(debt).toHaveCount(0);
      }
      await expect(card.getByRole("button", "Top up")).toBeEnabled();

      const layout = await browser.evaluate(async () => {
        await document.fonts.ready;
        const card = document.querySelector<HTMLElement>(
          'section[aria-labelledby="balance-title"]',
        )!;
        const amount = card.querySelector(".balance")!;
        const bounds = card.getBoundingClientRect();
        const style = getComputedStyle(card);
        const left = bounds.left + Number.parseFloat(style.paddingLeft);
        const right = bounds.right - Number.parseFloat(style.paddingRight);
        const top = bounds.top + Number.parseFloat(style.paddingTop);
        const bottom = bounds.bottom - Number.parseFloat(style.paddingBottom);
        const range = document.createRange();
        range.selectNodeContents(amount);
        const rects = Array.from(range.getClientRects());
        const page = document.documentElement;
        return {
          width: page.clientWidth,
          wraps: rects.length > 1,
          // Allow CSS subpixel rounding at the content edges.
          containsText:
            rects.length > 0 &&
            rects.every(
              (rect) =>
                rect.left >= left - 0.5 &&
                rect.right <= right + 0.5 &&
                rect.top >= top - 0.5 &&
                rect.bottom <= bottom + 0.5,
            ),
          cardOverflow: card.scrollWidth - card.clientWidth,
          pageOverflow: page.scrollWidth - page.clientWidth,
        };
      });
      expect(layout).toEqual({
        width,
        wraps,
        containsText: true,
        cardOverflow: 0,
        pageOverflow: 0,
      });
    });
  }

  test("disables Top up for an admin without an active subscription", async ({
    app,
    browser,
    screen,
  }) => {
    await interceptOverview(
      browser,
      app.baseUrl,
      -9223372036854775808n,
      BillingAccountRole.ADMIN,
      SubscriptionStatus.NONE,
    );
    await app.open(ACCOUNT_PATH);

    const card = screen.getByRole("region", "Balance", { exact: true });
    await expect(
      card.getByText("Top-ups need an active subscription."),
    ).toBeVisible();
    await expect(card.getByRole("button", "Top up")).toBeDisabled();
  });

  test("hides Top up for a viewer with an active subscription", async ({
    app,
    browser,
    screen,
  }) => {
    await interceptOverview(
      browser,
      app.baseUrl,
      -9223372036854775808n,
      BillingAccountRole.VIEWER,
    );
    await app.open(ACCOUNT_PATH);

    const card = screen.getByRole("region", "Balance", { exact: true });
    await expect(card.getByText(DEBT_EXPLANATION)).toBeVisible();
    await expect(card.getByRole("button", "Top up")).toHaveCount(0);
  });
});
