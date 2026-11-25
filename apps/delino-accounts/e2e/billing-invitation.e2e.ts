// SPDX-License-Identifier: Apache-2.0
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  CreateInvitationRequestSchema,
  GetBillingAccountResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

test("malformed invitation emails stay in the dialog without an RPC", async ({
  app,
  browser,
  screen,
}) => {
  const billingAccountId = "0193a1b2-0000-7000-8000-000000000001";
  await seedFixtureSession(browser, app.baseUrl);
  const invitationRequests: string[] = [];
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
    const method = new URL(route.request.url).pathname.split("/").pop();
    if (method === "CreateInvitation") {
      invitationRequests.push(route.request.postData ?? "");
    }
    const body =
      method === "GetBillingAccount"
        ? toBinary(
            GetBillingAccountResponseSchema,
            create(GetBillingAccountResponseSchema, {
              billingAccount: {
                billingAccountId,
                name: "Example account",
                callerRole: BillingAccountRole.OWNER,
              },
            }),
          )
        : new Uint8Array();
    await route.fulfill({ contentType: "application/proto", headers, body });
  });

  await app.open(`/billing/${billingAccountId}`);
  await screen.getByRole("tab", "Members").click();
  await screen.getByRole("button", "Invite by email").click();
  const dialog = screen.getByRole("dialog", "Invite by email");
  const email = dialog.getByRole("textbox", "Email");
  const send = dialog.getByRole("button", "Send invitation");
  for (const [address, screenshot] of [
    ["a..b@example.test", "invitation-email-double-dot"],
    ["<a@b>", "invitation-email-angle-brackets"],
    ["‹a@b›", "invitation-email-angle-quotes"],
  ] as const) {
    await email.fill(address);
    await send.click();
    await expect(dialog).toBeVisible();
    await expect(email).toHaveAttribute("aria-invalid", "true");
    await expect(
      dialog.getByText("Enter an email address like name@example.com."),
    ).toBeVisible();
    expect(invitationRequests).toHaveLength(0);
    await app.screenshot(screenshot);
  }

  await email.fill("a.b@example.test");
  await send.click();
  await expect(dialog).toBeHidden();
  expect(invitationRequests).toHaveLength(1);
  // The route API exposes the protobuf body as a string. This request is ASCII.
  const sent = fromBinary(
    CreateInvitationRequestSchema,
    new TextEncoder().encode(invitationRequests[0]),
  );
  expect(sent.email).toBe("a.b@example.test");
});
