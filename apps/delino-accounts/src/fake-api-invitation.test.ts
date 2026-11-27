// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  BillingAccountRole,
  BillingAccountService,
  InvitationState,
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

const OTHER_OUTSIDER = "user-other-outsider";
const OTHER_TOKEN = "bai_other-token";
const accountRequest = { billingAccountId: ACCOUNT_ID };

describe("fake AcceptInvitation", () => {
  test("rejects a known token of a deleted account and keeps the token", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    const account = api.accounts.get(ACCOUNT_ID)!;
    // The fake refuses to delete an account that still links organizations.
    account.linked = [];
    api.userId = OWNER;
    await client.deleteBillingAccount(accountRequest);
    const membersBefore = account.members.map((m) => ({ ...m }));

    api.userId = OUTSIDER;
    for (let attempt = 0; attempt < 2; attempt++) {
      const error = await rejection(
        client.acceptInvitation({ invitationToken: INVITATION_TOKEN }),
      );
      expect(error.code).toBe(Code.FailedPrecondition);
      expect(error.rawMessage).toBe("the billing account was deleted");
    }
    expect(account.members).toEqual(membersBefore);
  });

  test("accepts once and rejects replay by either identity", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    const account = api.accounts.get(ACCOUNT_ID)!;
    const original = account.invitations[0];
    const membersBefore = account.members.map((member) => ({ ...member }));
    const otherId = api.addInvitationToken(
      OTHER_TOKEN,
      ACCOUNT_ID,
      BillingAccountRole.ADMIN,
    );
    api.userId = OUTSIDER;

    const accepted = await client.acceptInvitation({
      invitationToken: INVITATION_TOKEN,
    });

    expect(accepted.member?.userId).toBe(OUTSIDER);
    expect(accepted.member?.role).toBe(BillingAccountRole.VIEWER);
    expect(original.state).toBe(InvitationState.ACCEPTED);
    expect(account.members).toEqual([
      ...membersBefore,
      expect.objectContaining({
        userId: OUTSIDER,
        role: BillingAccountRole.VIEWER,
      }),
    ]);
    const membersAfter = account.members.map((member) => ({ ...member }));

    api.userId = OWNER;
    const pending = await client.listInvitations(accountRequest);
    expect(
      pending.invitations.map((invitation) => invitation.invitationId),
    ).toEqual([otherId]);
    const revokeAccepted = await rejection(
      client.revokeInvitation({
        ...accountRequest,
        invitationId: original.invitationId,
      }),
    );
    expect(revokeAccepted.code).toBe(Code.FailedPrecondition);

    for (const userId of [OUTSIDER, OTHER_OUTSIDER]) {
      api.userId = userId;
      const error = await rejection(
        client.acceptInvitation({ invitationToken: INVITATION_TOKEN }),
      );
      expect(error.code).toBe(Code.FailedPrecondition);
      expect(account.members).toEqual(membersAfter);
    }

    api.userId = OUTSIDER;
    const error = await rejection(
      client.acceptInvitation({ invitationToken: OTHER_TOKEN }),
    );
    expect(error.code).toBe(Code.AlreadyExists);
    expect(account.members).toEqual(membersAfter);
    api.userId = OWNER;
    expect(await client.listInvitations(accountRequest)).toEqual(pending);
  });

  test("revokes a pending invitation and rejects reuse of its row and token", async () => {
    const api = createFakeApi();
    const client = createClient(BillingAccountService, api.transport);
    const account = api.accounts.get(ACCOUNT_ID)!;
    const membersBefore = account.members.map((member) => ({ ...member }));
    const initial = await client.listInvitations(accountRequest);
    expect(initial.invitations).toHaveLength(1);
    const request = {
      ...accountRequest,
      invitationId: initial.invitations[0].invitationId,
    };
    const otherId = api.addInvitationToken(
      OTHER_TOKEN,
      ACCOUNT_ID,
      BillingAccountRole.ADMIN,
    );

    const revoked = await client.revokeInvitation(request);
    expect(revoked.invitation?.state).toBe(InvitationState.REVOKED);
    const pending = await client.listInvitations(accountRequest);
    expect(
      pending.invitations.map((invitation) => invitation.invitationId),
    ).toEqual([otherId]);
    expect((await rejection(client.revokeInvitation(request))).code).toBe(
      Code.FailedPrecondition,
    );

    for (const userId of [OUTSIDER, OUTSIDER, OTHER_OUTSIDER]) {
      api.userId = userId;
      const error = await rejection(
        client.acceptInvitation({ invitationToken: INVITATION_TOKEN }),
      );
      expect(error.code).toBe(Code.FailedPrecondition);
      expect(account.members).toEqual(membersBefore);
    }

    api.userId = OUTSIDER;
    const accepted = await client.acceptInvitation({
      invitationToken: OTHER_TOKEN,
    });
    expect(accepted.member?.userId).toBe(OUTSIDER);
    expect(accepted.member?.role).toBe(BillingAccountRole.ADMIN);
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
