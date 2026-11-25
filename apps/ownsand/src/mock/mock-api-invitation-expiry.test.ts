// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { afterEach, describe, expect, rs, test } from "@rstest/core";
import { InvitationState, Role } from "@delino/ownsand-protos/v1/common_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
} from "./mock-api.ts";

const T0 = Date.UTC(2026, 0, 1, 12, 0, 0);
/** The seeded Contoso invitation expires 24 hours after the transport starts. */
const SEEDED_TTL_MS = 24 * 3600 * 1000;
/** An invitation that an administrator creates expires after 7 days. */
const CREATED_TTL_MS = 7 * 24 * 3600 * 1000;

async function codeOf(call: Promise<unknown>) {
  const error = await call.then(
    () => undefined,
    (e: unknown) => e,
  );
  expect(error).toBeInstanceOf(ConnectError);
  return (error as ConnectError).code;
}

describe("mock invitation expiry", () => {
  afterEach(() => {
    rs.useRealTimers();
  });

  const start = (userId?: string) => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0);
    const transport = createMockTransport({ userId });
    return {
      members: createClient(MemberService, transport),
      organizations: createClient(OrganizationService, transport),
    };
  };

  test("an unexpired seeded invitation still joins with the invited role", async () => {
    const { members } = start();
    rs.setSystemTime(T0 + SEEDED_TTL_MS - 1);
    const res = await members.acceptInvitation({
      invitationSecret: MOCK_INVITATION_SECRET,
    });
    expect(res.member?.role).toBe(Role.MEMBER);
    expect(res.organization?.organizationId).toBe(MOCK_IDS.contoso);
  });

  test("an invitation at and after its expiry is not found and nothing changes", async () => {
    const { members, organizations } = start();
    const orgIds = async () =>
      (await organizations.listOrganizations({})).organizations.map(
        (o) => o.organizationId,
      );
    const before = await orgIds();
    expect(before).not.toContain(MOCK_IDS.contoso);

    rs.setSystemTime(T0 + SEEDED_TTL_MS);
    const atExpiry = await members
      .acceptInvitation({ invitationSecret: MOCK_INVITATION_SECRET })
      .catch((e: unknown) => e);
    rs.setSystemTime(T0 + SEEDED_TTL_MS + 1);
    const afterExpiry = await codeOf(
      members.acceptInvitation({ invitationSecret: MOCK_INVITATION_SECRET }),
    );
    const unknown = await members
      .acceptInvitation({ invitationSecret: "osi_unknown_secret" })
      .catch((e: unknown) => e);

    expect(atExpiry).toBeInstanceOf(ConnectError);
    expect((atExpiry as ConnectError).code).toBe(Code.NotFound);
    expect(afterExpiry).toBe(Code.NotFound);
    // The message matches an unknown secret, so an expired link reveals nothing.
    expect((atExpiry as ConnectError).rawMessage).toBe(
      (unknown as ConnectError).rawMessage,
    );
    expect(await orgIds()).toEqual(before);

    // The rejection did not change the invitation: it joins when the clock
    // is before the expiry again.
    rs.setSystemTime(T0 + SEEDED_TTL_MS - 1);
    const res = await members.acceptInvitation({
      invitationSecret: MOCK_INVITATION_SECRET,
    });
    expect(res.member?.role).toBe(Role.MEMBER);
  });

  test("lists a pending invitation only before expiry and keeps the record", async () => {
    const { members } = start();
    const organizationId = MOCK_IDS.acme;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.MEMBER,
    });
    const listInvitations = async () =>
      (await members.listInvitations({ organizationId })).invitations;
    const before = await members.listMembers({ organizationId });

    // The caller owns the organization, so a valid invitation conflicts.
    const expiry = T0 + CREATED_TTL_MS;
    rs.setSystemTime(expiry - 1);
    expect(await listInvitations()).toEqual([invitation]);
    expect(invitation?.state).toBe(InvitationState.PENDING);
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.AlreadyExists,
    );

    rs.setSystemTime(expiry);
    expect(await listInvitations()).toEqual([]);
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.NotFound,
    );

    rs.setSystemTime(expiry + 1);
    expect(await listInvitations()).toEqual([]);
    expect(await members.listMembers({ organizationId })).toEqual(before);

    rs.setSystemTime(expiry - 1);
    expect(await listInvitations()).toEqual([invitation]);
  });

  test("revokes a pending invitation just before expiry and rejects a repeat", async () => {
    const { members } = start();
    const organizationId = MOCK_IDS.acme;
    const existing = await members.listInvitations({ organizationId });
    expect(existing.invitations).toHaveLength(1);
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.MEMBER,
    });
    const request = { organizationId, invitationId: invitation!.invitationId };
    rs.setSystemTime(T0 + CREATED_TTL_MS - 1);
    const revoked = await members.revokeInvitation(request);
    expect(revoked.invitation).toEqual({
      ...invitation,
      state: InvitationState.REVOKED,
    });

    const memberships = await members.listMembers({ organizationId });
    await expect(members.revokeInvitation(request)).rejects.toMatchObject({
      code: Code.FailedPrecondition,
      rawMessage: "only a pending invitation can be revoked",
    });
    // The seeded invitation has expired by this point. Rewind to check that
    // the revoked invitation stays hidden while the other one is still pending.
    rs.setSystemTime(T0);
    expect(await members.listInvitations({ organizationId })).toEqual(existing);
    expect(await members.listMembers({ organizationId })).toEqual(memberships);
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.NotFound,
    );
  });

  test.each([0, 1])(
    "rejects revocation at expiry plus %i ms",
    async (offset) => {
      const { members } = start();
      const organizationId = MOCK_IDS.acme;
      const { invitation } = await members.createInvitation({
        organizationId,
        role: Role.MEMBER,
      });
      const memberships = await members.listMembers({ organizationId });
      const expiry = T0 + CREATED_TTL_MS;
      rs.setSystemTime(expiry + offset);
      await expect(
        members.revokeInvitation({
          organizationId,
          invitationId: invitation!.invitationId,
        }),
      ).rejects.toMatchObject({
        code: Code.FailedPrecondition,
        rawMessage: "only a pending invitation can be revoked",
      });
      expect(await members.listMembers({ organizationId })).toEqual(
        memberships,
      );

      rs.setSystemTime(expiry - 1);
      const { invitations } = await members.listInvitations({ organizationId });
      expect(invitations).toEqual([invitation]);
      expect(invitations[0]?.state).toBe(InvitationState.PENDING);
    },
  );

  test("rejects revocation of an accepted invitation and preserves the new membership", async () => {
    const userId = "user-admin-jun";
    const { members } = start(userId);
    const organizationId = MOCK_IDS.acme;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.ADMIN,
    });
    await members.removeMember({ organizationId, userId });
    const accepted = await members.acceptInvitation({ invitationSecret });
    expect(accepted.member).toMatchObject({ userId, role: Role.ADMIN });
    const memberships = await members.listMembers({ organizationId });

    await expect(
      members.revokeInvitation({
        organizationId,
        invitationId: invitation!.invitationId,
      }),
    ).rejects.toMatchObject({
      code: Code.FailedPrecondition,
      rawMessage: "only a pending invitation can be revoked",
    });
    expect(await members.listMembers({ organizationId })).toEqual(memberships);
  });

  test("checks access before looking up the invitation ID", async () => {
    const { members } = start();
    const member = createClient(
      MemberService,
      createMockTransport({ userId: "user-member-ada" }),
    );
    const request = {
      organizationId: MOCK_IDS.acme,
      invitationId: "0193a000-0000-7000-8000-0000000000ff",
    };

    await expect(members.revokeInvitation(request)).rejects.toMatchObject({
      code: Code.NotFound,
      rawMessage: "invitation not found",
    });
    expect(await codeOf(member.revokeInvitation(request))).toBe(
      Code.PermissionDenied,
    );
    expect(
      await codeOf(
        members.revokeInvitation({
          ...request,
          organizationId: MOCK_IDS.contoso,
        }),
      ),
    ).toBe(Code.NotFound);
  });

  test("requires organization membership and an owner or admin role to list invitations", async () => {
    const { members } = start();
    expect(
      await codeOf(
        members.listInvitations({ organizationId: MOCK_IDS.contoso }),
      ),
    ).toBe(Code.NotFound);
    expect(
      await codeOf(
        members.listInvitations({ organizationId: MOCK_IDS.northwind }),
      ),
    ).toBe(Code.PermissionDenied);

    const admin = createClient(
      MemberService,
      createMockTransport({ userId: "user-admin-jun" }),
    );
    const { invitations } = await admin.listInvitations({
      organizationId: MOCK_IDS.acme,
    });
    expect(invitations).toHaveLength(1);
  });
});
