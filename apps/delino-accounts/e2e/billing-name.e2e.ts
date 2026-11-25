// SPDX-License-Identifier: Apache-2.0
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  GetBillingAccountResponseSchema,
  UpdateBillingAccountRequestSchema,
  UpdateBillingAccountResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const ACCOUNT_ID = "0193a1b2-0000-7000-8000-000000000001";
const ACCOUNT_PATH = `/billing/${ACCOUNT_ID}`;
const ORIGINAL_NAME = "Acme Robotics";
const BUSY = "Too many requests. Wait a moment and try again.";

async function interceptAccount(browser: Browser, appOrigin: string) {
  await seedFixtureSession(browser, appOrigin);
  const billingAccount = {
    billingAccountId: ACCOUNT_ID,
    name: ORIGINAL_NAME,
    callerRole: BillingAccountRole.ADMIN,
  };
  const submittedNames: string[] = [];
  const gates = new Map<
    string,
    { markStarted: () => void; fail: Promise<boolean> }
  >();
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
    const method = new URL(route.request.url).pathname.split("/").pop()!;
    const update = method === "UpdateBillingAccount";
    const request = update
      ? fromBinary(
          UpdateBillingAccountRequestSchema,
          // The route API exposes a string. These fixture requests use ASCII.
          new TextEncoder().encode(route.request.postData),
        )
      : undefined;
    if (request !== undefined) submittedNames.push(request.name);
    const gate = gates.get(method);
    if (gate !== undefined) {
      gates.delete(method);
      gate.markStarted();
      if (await gate.fail) {
        await route.fulfill({
          status: 429,
          headers,
          contentType: "application/json",
          body: JSON.stringify({ code: "resource_exhausted", message: "busy" }),
        });
        return;
      }
    }
    if (request !== undefined) billingAccount.name = request.name;
    const body = update
      ? toBinary(
          UpdateBillingAccountResponseSchema,
          create(UpdateBillingAccountResponseSchema, { billingAccount }),
        )
      : method === "GetBillingAccount"
        ? toBinary(
            GetBillingAccountResponseSchema,
            create(GetBillingAccountResponseSchema, { billingAccount }),
          )
        : new Uint8Array();
    await route.fulfill({ contentType: "application/proto", headers, body });
  });
  return {
    submittedNames,
    holdNext(method: "UpdateBillingAccount" | "GetBillingAccount") {
      let markStarted!: () => void;
      let respond!: (fail: boolean) => void;
      const started = new Promise<void>((resolve) => {
        markStarted = resolve;
      });
      const failure = new Promise<boolean>((resolve) => {
        respond = resolve;
      });
      gates.set(method, { markStarted, fail: failure });
      return {
        started,
        release: () => respond(false),
        fail: () => respond(true),
      };
    },
  };
}

describe("billing account name", () => {
  for (const [draft, nextName] of [
    ["  Beta  ", "Beta"],
    [ORIGINAL_NAME, ORIGINAL_NAME],
  ]) {
    test(`preserves a later edit to ${nextName} through a successful save`, async ({
      app,
      browser,
      screen,
    }) => {
      const api = await interceptAccount(browser, app.baseUrl);
      await app.open(ACCOUNT_PATH);
      await screen.getByRole("tab", "Settings").click();
      const field = screen.getByRole("textbox", "Billing account name");
      const save = screen
        .getByRole("region", "Name")
        .getByRole("button", "Save");
      const pending = api.holdNext("UpdateBillingAccount");
      try {
        await field.fill("  Alpha  ");
        await save.click();
        await pending.started;
        expect(api.submittedNames).toEqual(["Alpha"]);
        await expect(save).toBeDisabled();
        await field.fill(draft);
        await app.screenshot("save-pending");

        pending.release();
        await expect(
          screen.getByRole("heading", "Alpha", { level: 1 }),
        ).toBeVisible();
        await expect(field).toHaveValue(draft);
        await app.screenshot("draft-preserved");
        await save.click();
        await expect(
          screen.getByRole("heading", nextName, { level: 1 }),
        ).toBeVisible();
        expect(api.submittedNames).toEqual(["Alpha", nextName]);
      } finally {
        pending.release();
      }
    });
  }

  test("keeps the latest draft when the save fails", async ({
    app,
    browser,
    screen,
  }) => {
    const api = await interceptAccount(browser, app.baseUrl);
    await app.open(ACCOUNT_PATH);
    await screen.getByRole("tab", "Settings").click();
    const field = screen.getByRole("textbox", "Billing account name");
    const save = screen.getByRole("region", "Name").getByRole("button", "Save");
    const pending = api.holdNext("UpdateBillingAccount");
    try {
      await field.fill("Alpha");
      await save.click();
      await pending.started;
      await field.fill("  Beta  ");
      pending.fail();
      await expect(screen.getByText(BUSY)).toBeVisible();
      await expect(field).toHaveValue("  Beta  ");
      await app.screenshot("draft-after-save-failure");

      await save.click();
      await expect(
        screen.getByRole("heading", "Beta", { level: 1 }),
      ).toBeVisible();
      expect(api.submittedNames).toEqual(["Alpha", "Beta"]);
    } finally {
      pending.release();
    }
  });

  test("shows the normalized saved name through a failed refetch", async ({
    app,
    browser,
    screen,
  }) => {
    const api = await interceptAccount(browser, app.baseUrl);
    await app.open(ACCOUNT_PATH);
    await screen.getByRole("tab", "Settings").click();
    const field = screen.getByRole("textbox", "Billing account name");
    const save = screen.getByRole("region", "Name").getByRole("button", "Save");
    const refetch = api.holdNext("GetBillingAccount");
    try {
      await field.fill("  Alpha  ");
      await save.click();
      await refetch.started;
      await expect(field).toHaveValue("Alpha");
      await expect(save).toBeDisabled();
      await expect(
        screen.getByRole("heading", ORIGINAL_NAME, { level: 1 }),
      ).toBeVisible();
      await app.screenshot("refetch-pending");

      refetch.fail();
      await expect(screen.getByText(BUSY)).toBeVisible();
      await expect(field).toHaveValue("Alpha");
      await app.screenshot("name-after-refetch-failure");
      await screen.getByRole("button", "Try again").click();
      await expect(
        screen.getByRole("heading", "Alpha", { level: 1 }),
      ).toBeVisible();
      await expect(field).toHaveValue("Alpha");
    } finally {
      refetch.release();
    }
  });
});
