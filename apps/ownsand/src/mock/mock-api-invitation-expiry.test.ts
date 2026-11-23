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

  const start = () => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(T0);
    const transport = createMockTransport();
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

  test("the expiry check runs before the already-member check", async () => {
    const { members } = start();
    const organizationId = MOCK_IDS.acme;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.MEMBER,
    });
    const memberCount = async () =>
      (await members.listMembers({ organizationId })).members.length;
    const before = await memberCount();

    // The caller owns the organization, so a valid invitation conflicts.
    rs.setSystemTime(T0 + CREATED_TTL_MS - 1);
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.AlreadyExists,
    );
    rs.setSystemTime(T0 + CREATED_TTL_MS);
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.NotFound,
    );

    expect(await memberCount()).toBe(before);
    const { invitations } = await members.listInvitations({ organizationId });
    expect(
      invitations.find((i) => i.invitationId === invitation!.invitationId)
        ?.state,
    ).toBe(InvitationState.PENDING);
  });

  test("a revoked invitation stays not found before its expiry", async () => {
    const { members } = start();
    const organizationId = MOCK_IDS.acme;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.MEMBER,
    });
    await members.revokeInvitation({
      organizationId,
      invitationId: invitation!.invitationId,
    });
    expect(await codeOf(members.acceptInvitation({ invitationSecret }))).toBe(
      Code.NotFound,
    );
  });
});
