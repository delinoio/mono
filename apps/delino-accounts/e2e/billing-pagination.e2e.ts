// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  ListBillingAccountsRequestSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const tokens = ["", "p1", "p2", "p3"];
const names = ["Acme", "Bravo", "Cedar", "Delta"];
const requestBodies = tokens.map((pageToken) =>
  Buffer.from(
    toBinary(
      ListBillingAccountsRequestSchema,
      create(ListBillingAccountsRequestSchema, { pageSize: 200, pageToken }),
    ),
  ).toString("utf8"),
);

async function serveBillingPages(
  browser: Browser,
  appOrigin: string,
  pageCount: number,
  failThirdPage = false,
) {
  await seedFixtureSession(browser, appOrigin);
  const requestedTokens: string[] = [];
  await browser.route(`${API_ORIGIN}/**/ListBillingAccounts`, async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }

    // The route API exposes the binary request as a UTF-8 string.
    const page = requestBodies.indexOf(route.request.postData ?? "");
    if (page < 0 || page >= pageCount) {
      throw new Error("Unexpected ListBillingAccounts request");
    }
    requestedTokens.push(tokens[page]);
    if (page === 2 && failThirdPage) {
      failThirdPage = false;
      await route.fulfill({
        status: 429,
        contentType: "application/json",
        headers,
        body: '{"code":"resource_exhausted"}',
      });
      return;
    }
    const response = create(ListBillingAccountsResponseSchema, {
      billingAccounts: [
        {
          billingAccountId: `0193a1b2-0000-7000-8000-00000000000${page + 1}`,
          name: names[page],
          callerRole: BillingAccountRole.OWNER,
        },
      ],
      nextPageToken: page + 1 < pageCount ? tokens[page + 1] : "",
    });
    await route.fulfill({
      contentType: "application/proto",
      headers,
      body: toBinary(ListBillingAccountsResponseSchema, response),
    });
  });
  return requestedTokens;
}

describe("billing account pagination", () => {
  test("reads four immediate pages in order", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 4);
    await app.open("/billing");

    const rows = screen
      .getByRole("list", "Billing accounts")
      .getByRole("listitem");
    await expect(rows).toHaveCount(4);
    for (const [index, name] of names.entries()) {
      await expect(
        rows.nth(index).getByRole("link", `${name} Owner`),
      ).toBeVisible();
    }
    await expect(screen.getByText("Loading more")).toBeHidden();
    expect(requestedTokens).toEqual(tokens);
    await app.screenshot("billing-all-pages");
  });

  test("stops when the first page has no continuation token", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(browser, app.baseUrl, 1);
    await app.open("/billing");

    await expect(screen.getByRole("link", "Acme Owner")).toBeVisible();
    await expect(screen.getByText("Loading more")).toBeHidden();
    expect(requestedTokens).toEqual([""]);
    await app.screenshot("billing-first-page-terminal");
  });

  test("keeps loaded rows and retries the failed token", async ({
    app,
    browser,
    screen,
  }) => {
    const requestedTokens = await serveBillingPages(
      browser,
      app.baseUrl,
      4,
      true,
    );
    await app.open("/billing");

    const rows = screen
      .getByRole("list", "Billing accounts")
      .getByRole("listitem");
    const errorMessage = screen.getByText(/Some items did not load/);
    const loading = screen.getByText("Loading more");
    await expect(errorMessage).toBeVisible();
    await expect(rows).toHaveCount(2);
    await expect(loading).toBeHidden();
    expect(requestedTokens).toEqual(["", "p1", "p2"]);
    await app.screenshot("billing-continuation-paused");

    await screen.getByRole("button", "Try again").click();
    await expect(rows).toHaveCount(4);
    await expect(errorMessage).toBeHidden();
    await expect(loading).toBeHidden();
    expect(requestedTokens).toEqual(["", "p1", "p2", "p2", "p3"]);
    await app.screenshot("billing-continuation-recovered");
  });
});
