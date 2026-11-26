// SPDX-License-Identifier: Apache-2.0
import { createRouterTransport, type Transport } from "@connectrpc/connect";
import {
  EnrollmentTokenState,
  InvitationState,
  Role,
} from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";

export const FIRST_REVOCATION_ID = "0194993a-5800-7000-8000-000000000002";
export const SECOND_REVOCATION_ID = "0194993a-5800-7000-8000-000000000003";
export const TOKEN_PREFIX = "ose_12ab";
export const INVITATION_PREFIX = "osi_12ab";

export function revokeCollisionTransport(
  base: Transport,
  {
    variant = "same",
    waitForContinuation,
  }: {
    variant?: "same" | "cross-role" | "unique" | "duplicate" | "inactive";
    waitForContinuation?: () => Promise<void>;
  } = {},
) {
  const requests: Array<
    | { organizationId: string; enrollmentTokenId: string }
    | { organizationId: string; invitationId: string }
  > = [];
  const revoked = new Set<string>();

  async function page<T>(pageToken: string, first: T, second: T) {
    if (!waitForContinuation) {
      return { rows: [first, second], nextPageToken: "" };
    }
    if (!pageToken) return { rows: [first], nextPageToken: "second" };
    await waitForContinuation();
    return { rows: [second], nextPageToken: "" };
  }

  const timestamp = { seconds: 1760000000n };
  const secondId =
    variant === "duplicate" ? FIRST_REVOCATION_ID : SECOND_REVOCATION_ID;
  const fixture = createRouterTransport((router) => {
    router.rpc(DeviceService.method.listEnrollmentTokens, async (request) => {
      const token = (id: string, tokenPrefix: string, inactive = false) => ({
        organizationId: request.organizationId,
        enrollmentTokenId: id,
        tokenPrefix,
        state:
          inactive || revoked.has(id)
            ? EnrollmentTokenState.REVOKED
            : EnrollmentTokenState.PENDING,
        createTime: timestamp,
        expireTime: timestamp,
      });
      const { rows, nextPageToken } = await page(
        request.pageToken,
        token(FIRST_REVOCATION_ID, TOKEN_PREFIX),
        token(
          secondId,
          variant === "unique" ? "ose_34cd" : TOKEN_PREFIX,
          variant === "inactive",
        ),
      );
      return { tokens: rows, nextPageToken };
    });
    router.rpc(MemberService.method.listInvitations, async (request) => {
      const invitation = (
        id: string,
        secretPrefix: string,
        role: Role,
        inactive = false,
      ) => ({
        organizationId: request.organizationId,
        invitationId: id,
        secretPrefix,
        role,
        state:
          inactive || revoked.has(id)
            ? InvitationState.REVOKED
            : InvitationState.PENDING,
        createTime: timestamp,
        expireTime: timestamp,
      });
      const { rows, nextPageToken } = await page(
        request.pageToken,
        invitation(FIRST_REVOCATION_ID, INVITATION_PREFIX, Role.MEMBER),
        invitation(
          secondId,
          variant === "unique" ? "osi_34cd" : INVITATION_PREFIX,
          variant === "cross-role" ? Role.ADMIN : Role.MEMBER,
          variant === "inactive",
        ),
      );
      return { invitations: rows, nextPageToken };
    });
    router.rpc(DeviceService.method.revokeEnrollmentToken, (request) => {
      const { organizationId, enrollmentTokenId } = request;
      requests.push({ organizationId, enrollmentTokenId });
      revoked.add(enrollmentTokenId);
      return {};
    });
    router.rpc(MemberService.method.revokeInvitation, (request) => {
      const { organizationId, invitationId } = request;
      requests.push({ organizationId, invitationId });
      revoked.add(invitationId);
      return {};
    });
  });
  const transport: Transport = {
    ...base,
    unary(method, ...args) {
      switch (method.name) {
        case "ListEnrollmentTokens":
        case "ListInvitations":
        case "RevokeEnrollmentToken":
        case "RevokeInvitation":
          return fixture.unary(method, ...args);
        default:
          return base.unary(method, ...args);
      }
    },
  };
  return { transport, requests };
}
