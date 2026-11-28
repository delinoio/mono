// SPDX-License-Identifier: Apache-2.0
import { create, toBinary } from "@bufbuild/protobuf";
import {
  BillingAccountRole,
  GetBillingAccountResponseSchema,
  InvitationState,
  ListBillingAccountsResponseSchema,
  ListInvitationsRequestSchema,
  ListInvitationsResponseSchema,
  RevokeInvitationRequestSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { test } from "@e2e-dev/web";
import { expect } from "e2e";
import { API_ORIGIN, seedFixtureSession } from "./fixture-session.ts";

const account = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000001",
  name: "Example account",
  callerRole: BillingAccountRole.OWNER,
};
const { billingAccountId } = account;
const staleEmail = "stale@example.com";
const pendingEmail = "pending@example.com";
const staleInvitationId = "0193a1b2-0000-7000-8000-000000000002";
const firstPageRequest = Buffer.from(
  toBinary(
    ListInvitationsRequestSchema,
    create(ListInvitationsRequestSchema, { billingAccountId, pageSize: 200 }),
  ),
).toString("utf8");
const revokeRequest = Buffer.from(
  toBinary(
    RevokeInvitationRequestSchema,
    create(RevokeInvitationRequestSchema, {
      billingAccountId,
      invitationId: staleInvitationId,
    }),
  ),
).toString("utf8");

for (const failRefresh of [false, true]) {
  test(
    failRefresh
      ? "a rejected revoke keeps pending rows until the failed refresh is retried"
      : "a rejected revoke immediately refreshes pending invitations",
    async ({ app, browser, screen }) => {
      await seedFixtureSession(browser, app.baseUrl);
      const invitationRequests: Array<string | null | undefined> = [];
      const revokeRequests: Array<string | null | undefined> = [];
      let revokedElsewhere = false;
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
        let body: Uint8Array = new Uint8Array();
        if (method === "GetBillingAccount") {
          body = toBinary(
            GetBillingAccountResponseSchema,
            create(GetBillingAccountResponseSchema, {
              billingAccount: account,
            }),
          );
        } else if (method === "ListBillingAccounts") {
          body = toBinary(
            ListBillingAccountsResponseSchema,
            create(ListBillingAccountsResponseSchema, {
              billingAccounts: [account],
            }),
          );
        } else if (method === "ListInvitations") {
          invitationRequests.push(route.request.postData);
          if (failRefresh && invitationRequests.length === 2) {
            await route.fulfill({
              status: 429,
              contentType: "application/json",
              headers,
              body: '{"code":"resource_exhausted"}',
            });
            return;
          }
          const pending = {
            billingAccountId,
            state: InvitationState.PENDING,
            role: BillingAccountRole.VIEWER,
          };
          const invitations = [
            {
              ...pending,
              invitationId: "0193a1b2-0000-7000-8000-000000000003",
              email: pendingEmail,
            },
          ];
          if (!revokedElsewhere) {
            invitations.push({
              ...pending,
              invitationId: staleInvitationId,
              email: staleEmail,
            });
          }
          body = toBinary(
            ListInvitationsResponseSchema,
            create(ListInvitationsResponseSchema, { invitations }),
          );
        } else if (method === "RevokeInvitation") {
          if (!revokedElsewhere) throw new Error("Expected a stale invitation");
          revokeRequests.push(route.request.postData);
          await route.fulfill({
            status: 400,
            contentType: "application/json",
            headers,
            body: '{"code":"failed_precondition","message":"the invitation is not pending"}',
          });
          return;
        }
        await route.fulfill({
          contentType: "application/proto",
          headers,
          body,
        });
      });

      await app.open(`/billing/${billingAccountId}`);
      await screen.getByRole("tab", "Members").click();
      const list = screen.getByRole("list", "Pending invitations");
      const staleRow = list
        .getByRole("listitem")
        .filter({ hasText: staleEmail });
      const pendingRow = list
        .getByRole("listitem")
        .filter({ hasText: pendingEmail });
      await expect(staleRow).toBeVisible();
      await expect(pendingRow).toBeVisible();
      expect(invitationRequests).toEqual([firstPageRequest]);

      revokedElsewhere = true;
      await staleRow.getByRole("button", "Revoke").click();
      await expect(
        screen.getByText("the invitation is not pending"),
      ).toBeVisible();
      if (failRefresh) {
        const notice = screen
          .getByRole("alert")
          .filter({ hasText: "Too many requests" });
        await expect(notice).toBeVisible();
        await expect(staleRow).toBeVisible();
        await expect(pendingRow).toBeVisible();
        expect(invitationRequests).toEqual([
          firstPageRequest,
          firstPageRequest,
        ]);
        await app.screenshot("invitation-refresh-failed");

        await notice.getByRole("button", "Try again").click();
        await expect(notice).toBeHidden();
      }
      await expect(staleRow).toBeHidden();
      await expect(pendingRow.getByRole("button", "Revoke")).toBeVisible();
      await expect(list.getByRole("button", "Revoke")).toHaveCount(1);
      await expect(screen.getByText("The invitation is revoked.")).toBeHidden();
      expect(revokeRequests).toEqual([revokeRequest]);
      expect(invitationRequests).toEqual(
        Array(failRefresh ? 3 : 2).fill(firstPageRequest),
      );
      await app.screenshot(
        failRefresh ? "invitation-refresh-retried" : "stale-invitation-removed",
      );
    },
  );
}
