// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  ACCOUNT_ID,
  createFakeApi,
  INVITATION_TOKEN,
  OUTSIDER,
  OWNER,
} from "./testing/fake-api.ts";

const rejection = async (promise: Promise<unknown>) => {
  try {
    await promise;
  } catch (error) {
    return ConnectError.from(error);
  }
  throw new Error("the call did not fail");
};

describe("fake AcceptInvitation", () => {
  test("rejects a known token of a deleted account and keeps the token", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    const account = api.accounts.get(ACCOUNT_ID)!;
    // The fake refuses to delete an account that still links organizations.
    account.linked = [];
    api.userId = OWNER;
    await client.deleteBillingAccount({ billingAccountId: ACCOUNT_ID });
    const membersBefore = account.members.map((m) => ({ ...m }));

    api.userId = OUTSIDER;
    const error = await rejection(
      client.acceptInvitation({ invitationToken: INVITATION_TOKEN }),
    );

    expect(error.code).toBe(Code.FailedPrecondition);
    expect(account.members).toEqual(membersBefore);
    expect(api.invitationTokens.has(INVITATION_TOKEN)).toBe(true);
  });

  test("still accepts a token of an active account for a nonmember", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    api.userId = OUTSIDER;

    const accepted = await client.acceptInvitation({
      invitationToken: INVITATION_TOKEN,
    });

    expect(accepted.member?.userId).toBe(OUTSIDER);
    expect(accepted.member?.role).toBe(BillingAccountRole.VIEWER);
    expect(
      api.accounts.get(ACCOUNT_ID)!.members.some((m) => m.userId === OUTSIDER),
    ).toBe(true);
    expect(api.invitationTokens.has(INVITATION_TOKEN)).toBe(false);
  });

  test("still returns NotFound for an unknown token", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    api.userId = OUTSIDER;

    const error = await rejection(
      client.acceptInvitation({ invitationToken: "no-such-token" }),
    );

    expect(error.code).toBe(Code.NotFound);
  });
});
