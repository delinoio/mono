// SPDX-License-Identifier: Apache-2.0
import { Code, createClient } from "@connectrpc/connect";
import { AuditAction, AuditService } from "@delino/sandrun-protos/v1/audit_pb";
import { InvitationState, Role } from "@delino/sandrun-protos/v1/common_pb";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  rs,
  test,
} from "@rstest/core";
import { timestampMs } from "../format.ts";
import {
  createMockTransport,
  MOCK_IDS,
  MOCK_INVITATION_SECRET,
  MOCK_USER,
} from "./mock-api.ts";

const START_TIME = Date.UTC(2026, 0, 1);
const CONTOSO_EXPIRY = START_TIME + 6 * 24 * 60 * 60 * 1000;

function connectToMock() {
  const transport = createMockTransport();
  return {
    members: createClient(MemberService, transport),
    organizations: createClient(OrganizationService, transport),
    audit: createClient(AuditService, transport),
  };
}

describe("mock invitation lifecycle", () => {
  beforeEach(() => {
    rs.useFakeTimers({ toFake: ["Date"] });
    rs.setSystemTime(START_TIME);
  });

  afterEach(() => {
    rs.useRealTimers();
  });

  test("pages only eligible invitations in order and omits secrets", async () => {
    const { members } = connectToMock();
    const organizationId = MOCK_IDS.acme;
    const invite = () =>
      members.createInvitation({ organizationId, role: Role.MEMBER });
    for (let index = 0; index < 200; index++) {
      const { invitation } = await invite();
      await members.revokeInvitation({
        organizationId,
        invitationId: invitation!.invitationId,
      });
    }
    const { invitation: expired } = await invite();
    rs.setSystemTime(timestampMs(expired?.expireTime)!);
    const { invitation: first } = await invite();
    const fullPage = await members.listInvitations({
      organizationId,
      pageSize: 200,
    });
    expect(fullPage.invitations.map((i) => i.invitationId)).toEqual([
      first!.invitationId,
    ]);
    expect(fullPage.nextPageToken).toBe("");

    const { invitation: second } = await invite();
    const page = await members.listInvitations({ organizationId, pageSize: 1 });
    expect(page.nextPageToken).not.toBe("");
    const nextPage = await members.listInvitations({
      organizationId,
      pageSize: 1,
      pageToken: page.nextPageToken,
    });
    expect(nextPage.nextPageToken).toBe("");
    expect(
      [page, nextPage].map((p) => p.invitations.map((i) => i.invitationId)),
    ).toEqual([[first!.invitationId], [second!.invitationId]]);
    for (const response of [fullPage, page, nextPage]) {
      expect(response).not.toHaveProperty("invitationSecret");
      expect(response.invitations[0]).not.toHaveProperty("secret");
    }
  });

  test("revokes a pending invitation once and preserves audit on rejected revokes", async () => {
    const { members, audit } = connectToMock();
    const organizationId = MOCK_IDS.acme;
    const invite = () =>
      members.createInvitation({ organizationId, role: Role.MEMBER });
    const { invitation, invitationSecret } = await invite();
    const { invitation: expiring } = await invite();
    const revoke = (invitationId: string) =>
      members.revokeInvitation({ organizationId, invitationId });
    const auditEvents = async () =>
      (await audit.listAuditEvents({ organizationId })).events;

    const before = await auditEvents();
    const result = await revoke(invitation!.invitationId);
    expect(result.invitation?.state).toBe(InvitationState.REVOKED);
    const after = await auditEvents();
    expect(after).toHaveLength(before.length + 1);
    expect(after[0]).toMatchObject({
      action: AuditAction.INVITATION_REVOKED,
      targetId: invitation!.invitationId,
    });
    expect(after.slice(1)).toEqual(before);

    await expect(revoke(invitation!.invitationId)).rejects.toMatchObject({
      code: Code.FailedPrecondition,
    });
    await expect(revoke("unknown")).rejects.toMatchObject({
      code: Code.NotFound,
    });
    await expect(
      members.acceptInvitation({ invitationSecret }),
    ).rejects.toMatchObject({ code: Code.NotFound });

    const expiry = timestampMs(expiring?.expireTime)!;
    rs.setSystemTime(expiry - 1);
    expect(
      (await members.listInvitations({ organizationId })).invitations,
    ).toEqual([expiring]);
    rs.setSystemTime(expiry);
    expect(
      (await members.listInvitations({ organizationId })).invitations,
    ).toEqual([]);
    await expect(revoke(expiring!.invitationId)).rejects.toMatchObject({
      code: Code.FailedPrecondition,
    });
    expect(await auditEvents()).toEqual(after);
    rs.setSystemTime(expiry - 1);
    expect(
      (await members.listInvitations({ organizationId })).invitations,
    ).toEqual([expiring]);
  });

  test("rejects revocation of an accepted invitation and preserves audit", async () => {
    const { members, audit } = connectToMock();
    const organizationId = MOCK_IDS.globex;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.ADMIN,
    });
    await members.removeMember({ organizationId, userId: MOCK_USER });
    await members.acceptInvitation({ invitationSecret });
    const before = await audit.listAuditEvents({ organizationId });

    await expect(
      members.revokeInvitation({
        organizationId,
        invitationId: invitation!.invitationId,
      }),
    ).rejects.toMatchObject({ code: Code.FailedPrecondition });
    expect(await audit.listAuditEvents({ organizationId })).toEqual(before);
    expect(
      (await members.listInvitations({ organizationId })).invitations,
    ).toEqual([]);
  });

  test.each([0, 1])(
    "rejects acceptance at an offset of %i ms from expiry without consuming the link",
    async (offset) => {
      const { members, organizations, audit } = connectToMock();
      const accept = (invitationSecret: string) =>
        members.acceptInvitation({ invitationSecret });
      const before = await organizations.listOrganizations({});
      const missing = {
        code: Code.NotFound,
        rawMessage: "invitation not found or no longer valid",
      };

      rs.setSystemTime(CONTOSO_EXPIRY + offset);
      await expect(accept(MOCK_INVITATION_SECRET)).rejects.toMatchObject(
        missing,
      );
      await expect(accept("unknown")).rejects.toMatchObject(missing);
      expect(await organizations.listOrganizations({})).toEqual(before);

      rs.setSystemTime(CONTOSO_EXPIRY - 1);
      const joined = await accept(MOCK_INVITATION_SECRET);
      expect(joined.organization?.organizationId).toBe(MOCK_IDS.contoso);
      expect(joined.member).toMatchObject({
        userId: MOCK_USER,
        role: Role.MEMBER,
      });
      await expect(accept(MOCK_INVITATION_SECRET)).rejects.toMatchObject(
        missing,
      );
      const { events } = await audit.listAuditEvents({
        organizationId: MOCK_IDS.contoso,
      });
      expect(events).toHaveLength(1);
      expect(events[0]).toMatchObject({
        action: AuditAction.MEMBER_JOINED,
        targetId: MOCK_USER,
      });
    },
  );

  test("an existing member cannot consume an eligible link", async () => {
    const { members, audit } = connectToMock();
    const organizationId = MOCK_IDS.acme;
    const { invitation, invitationSecret } = await members.createInvitation({
      organizationId,
      role: Role.MEMBER,
    });
    const beforeMembers = await members.listMembers({ organizationId });
    const beforeAudit = await audit.listAuditEvents({ organizationId });

    await expect(
      members.acceptInvitation({ invitationSecret }),
    ).rejects.toMatchObject({ code: Code.AlreadyExists });
    expect(
      (await members.listInvitations({ organizationId })).invitations,
    ).toEqual([invitation]);
    rs.setSystemTime(timestampMs(invitation?.expireTime)!);
    await expect(
      members.acceptInvitation({ invitationSecret }),
    ).rejects.toMatchObject({ code: Code.NotFound });
    expect(await members.listMembers({ organizationId })).toEqual(
      beforeMembers,
    );
    expect(await audit.listAuditEvents({ organizationId })).toEqual(
      beforeAudit,
    );
  });
});
