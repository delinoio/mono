// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { BillingService } from "@delino/ownsand-protos/v1/billing_pb";
import { BillingReason, Role } from "@delino/ownsand-protos/v1/common_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { createMockTransport, MOCK_IDS, MOCK_USER } from "./mock-api.ts";

const organizationId = MOCK_IDS.acme;
const ADMIN = "user-admin-jun";

async function errorOf(call: Promise<unknown>) {
  const error = await call.then(
    () => undefined,
    (e: unknown) => e,
  );
  expect(error).toBeInstanceOf(ConnectError);
  return error as ConnectError;
}

describe("mock ownership transfer to the caller", () => {
  const clients = () => {
    const transport = createMockTransport();
    return {
      organizations: createClient(OrganizationService, transport),
      members: createClient(MemberService, transport),
      billing: createClient(BillingService, transport),
    };
  };

  test("fails with invalid_argument and changes no role or billing state", async () => {
    const { organizations, members, billing } = clients();
    const rolesBefore = (
      await members.listMembers({ organizationId })
    ).members.map((m) => [m.userId, m.role]);
    const billingBefore = await billing.getBillingStatus({ organizationId });
    expect(billingBefore.billing?.linked).toBe(true);

    const error = await errorOf(
      organizations.transferOwnership({ organizationId, userId: MOCK_USER }),
    );
    expect(error.code).toBe(Code.InvalidArgument);

    const rolesAfter = (
      await members.listMembers({ organizationId })
    ).members.map((m) => [m.userId, m.role]);
    expect(rolesAfter).toEqual(rolesBefore);
    expect(rolesAfter).toContainEqual([MOCK_USER, Role.OWNER]);
    // The whole status stays, including the link and the block fields.
    expect(await billing.getBillingStatus({ organizationId })).toEqual(
      billingBefore,
    );
  });

  test("still authorizes the owner first", async () => {
    const transport = createMockTransport({ userId: ADMIN });
    const error = await errorOf(
      createClient(OrganizationService, transport).transferOwnership({
        organizationId,
        userId: ADMIN,
      }),
    );
    expect(error.code).toBe(Code.PermissionDenied);
  });

  test("a transfer to another member still changes the owner and unlinks billing", async () => {
    const { organizations, members, billing } = clients();
    await organizations.transferOwnership({ organizationId, userId: ADMIN });

    const roles = new Map(
      (await members.listMembers({ organizationId })).members.map((m) => [
        m.userId,
        m.role,
      ]),
    );
    expect(roles.get(ADMIN)).toBe(Role.OWNER);
    expect(roles.get(MOCK_USER)).toBe(Role.ADMIN);
    const status = await billing.getBillingStatus({ organizationId });
    expect(status.billing?.linked).toBe(false);
    expect(status.billing?.reason).toBe(BillingReason.ACCOUNT_REQUIRED);
  });
});
