// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import {
  ACCOUNT_ID,
  ADMIN,
  createFakeApi,
  OWNER,
  VIEWER,
} from "./testing/fake-api.ts";

const rejection = async (promise: Promise<unknown>) => {
  try {
    await promise;
  } catch (error) {
    return ConnectError.from(error);
  }
  throw new Error("the call did not fail");
};

function setup(caller: string) {
  const api = createFakeApi();
  api.userId = caller;
  const client = createClient(BillingAccountService, api.transport);
  const members = () => api.accounts.get(ACCOUNT_ID)!.members;
  const roleOf = (userId: string) =>
    members().find((m) => m.userId === userId)?.role;
  const listMemberRoles = async () => {
    const { members } = await client.listMembers({
      billingAccountId: ACCOUNT_ID,
    });
    return members.map(({ userId, role }) => ({ userId, role }));
  };
  return { api, client, members, roleOf, listMemberRoles };
}

describe("fake owner protection", () => {
  test("rejects an admin changing the owner's role and keeps the role", async () => {
    const { client, roleOf } = setup(ADMIN);

    const error = await rejection(
      client.updateMemberRole({
        billingAccountId: ACCOUNT_ID,
        userId: OWNER,
        role: BillingAccountRole.VIEWER,
      }),
    );

    expect(error.code).toBe(Code.FailedPrecondition);
    expect(roleOf(OWNER)).toBe(BillingAccountRole.OWNER);
  });

  test("rejects an admin removing the owner and keeps the membership", async () => {
    const { client, roleOf } = setup(ADMIN);

    const error = await rejection(
      client.removeMember({ billingAccountId: ACCOUNT_ID, userId: OWNER }),
    );

    expect(error.code).toBe(Code.FailedPrecondition);
    expect(roleOf(OWNER)).toBe(BillingAccountRole.OWNER);
  });

  test("rejects the owner removing itself", async () => {
    const { client, roleOf } = setup(OWNER);

    const error = await rejection(
      client.removeMember({ billingAccountId: ACCOUNT_ID, userId: OWNER }),
    );

    expect(error.code).toBe(Code.FailedPrecondition);
    expect(roleOf(OWNER)).toBe(BillingAccountRole.OWNER);
  });

  test("rejects a transfer to the current owner and preserves membership", async () => {
    const { client, listMemberRoles } = setup(OWNER);
    const before = await listMemberRoles();

    const error = await rejection(
      client.transferOwnership({ billingAccountId: ACCOUNT_ID, userId: OWNER }),
    );

    expect(error.code).toBe(Code.InvalidArgument);
    expect(error.rawMessage).toBe("user_id must name another member");
    const after = await listMemberRoles();
    expect(after).toEqual(before);
    expect(
      after.filter(({ role }) => role === BillingAccountRole.OWNER),
    ).toEqual([{ userId: OWNER, role: BillingAccountRole.OWNER }]);
  });

  test("checks ownership before checking the transfer target", async () => {
    const { client, listMemberRoles } = setup(ADMIN);
    const before = await listMemberRoles();

    const error = await rejection(
      client.transferOwnership({ billingAccountId: ACCOUNT_ID, userId: ADMIN }),
    );

    expect(error.code).toBe(Code.PermissionDenied);
    expect(await listMemberRoles()).toEqual(before);
  });

  test("rejects a transfer to an unknown member and preserves membership", async () => {
    const { client, listMemberRoles } = setup(OWNER);
    const before = await listMemberRoles();

    const error = await rejection(
      client.transferOwnership({
        billingAccountId: ACCOUNT_ID,
        userId: "user-missing",
      }),
    );

    expect(error.code).toBe(Code.NotFound);
    expect(await listMemberRoles()).toEqual(before);
  });

  test("protects the new owner after a transfer and releases the former one", async () => {
    const { api, client, roleOf } = setup(OWNER);
    await client.transferOwnership({
      billingAccountId: ACCOUNT_ID,
      userId: ADMIN,
    });
    expect(roleOf(ADMIN)).toBe(BillingAccountRole.OWNER);
    expect(roleOf(OWNER)).toBe(BillingAccountRole.ADMIN);

    // The former owner is now an ordinary admin, so it cannot touch the new
    // owner, while the new owner can change the former owner.
    const update = await rejection(
      client.updateMemberRole({
        billingAccountId: ACCOUNT_ID,
        userId: ADMIN,
        role: BillingAccountRole.VIEWER,
      }),
    );
    const remove = await rejection(
      client.removeMember({ billingAccountId: ACCOUNT_ID, userId: ADMIN }),
    );
    expect(update.code).toBe(Code.FailedPrecondition);
    expect(remove.code).toBe(Code.FailedPrecondition);
    expect(roleOf(ADMIN)).toBe(BillingAccountRole.OWNER);

    api.userId = ADMIN;
    const updated = await client.updateMemberRole({
      billingAccountId: ACCOUNT_ID,
      userId: OWNER,
      role: BillingAccountRole.VIEWER,
    });
    expect(updated.member?.role).toBe(BillingAccountRole.VIEWER);
    await client.removeMember({ billingAccountId: ACCOUNT_ID, userId: OWNER });
    expect(roleOf(OWNER)).toBeUndefined();
  });

  test("still lets an admin change and remove non-owner members", async () => {
    const { client, roleOf } = setup(ADMIN);

    const promoted = await client.updateMemberRole({
      billingAccountId: ACCOUNT_ID,
      userId: VIEWER,
      role: BillingAccountRole.ADMIN,
    });
    expect(promoted.member?.role).toBe(BillingAccountRole.ADMIN);

    await client.removeMember({ billingAccountId: ACCOUNT_ID, userId: VIEWER });
    expect(roleOf(VIEWER)).toBeUndefined();
  });

  test("keeps PermissionDenied for a viewer removing the owner", async () => {
    const { client, roleOf } = setup(VIEWER);

    const error = await rejection(
      client.removeMember({ billingAccountId: ACCOUNT_ID, userId: OWNER }),
    );

    expect(error.code).toBe(Code.PermissionDenied);
    expect(roleOf(OWNER)).toBe(BillingAccountRole.OWNER);
  });

  test("returns NotFound for a missing member like the production service", async () => {
    const { client } = setup(ADMIN);

    const error = await rejection(
      client.removeMember({
        billingAccountId: ACCOUNT_ID,
        userId: "user-missing",
      }),
    );

    expect(error.code).toBe(Code.NotFound);
  });
});
