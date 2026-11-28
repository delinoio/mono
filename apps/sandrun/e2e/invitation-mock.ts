// SPDX-License-Identifier: Apache-2.0
import { isMessage } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import {
  AcceptInvitationResponseSchema,
  type ListInvitationsRequest,
  MemberService,
  type RevokeInvitationRequest,
} from "@delino/sandrun-protos/v1/member_pb";
import {
  createMockTransport as createBaseMock,
  type MockTransportOptions,
} from "../src/mock/mock-api.ts";

declare global {
  interface Window {
    sandrunInvitationRevokeFixture?: Omit<
      ReturnType<typeof invitationRevokeFixture>,
      "transport"
    >;
  }
}

function invitationRevokeFixture(base: Transport) {
  const client = createClient(MemberService, base);
  const listRequests: Pick<
    ListInvitationsRequest,
    "organizationId" | "pageToken"
  >[] = [];
  const revokeRequests: Pick<
    RevokeInvitationRequest,
    "organizationId" | "invitationId"
  >[] = [];
  let failNextList = false;
  const transport: Transport = {
    ...base,
    async unary(method, ...args) {
      if (method.name === MemberService.method.listInvitations.name) {
        const { organizationId, pageToken = "" } =
          args[3] as ListInvitationsRequest;
        listRequests.push({ organizationId, pageToken });
        if (failNextList) {
          failNextList = false;
          throw new ConnectError("Fixture list failed", Code.Internal);
        }
      }
      if (method.name === MemberService.method.revokeInvitation.name) {
        const { organizationId, invitationId } =
          args[3] as RevokeInvitationRequest;
        revokeRequests.push({ organizationId, invitationId });
      }
      return base.unary(method, ...args);
    },
  };
  return {
    transport,
    listRequests,
    revokeRequests,
    async seed(organizationId: string) {
      const { invitation: member } = await client.createInvitation({
        organizationId,
        role: Role.MEMBER,
      });
      await client.createInvitation({
        organizationId,
        role: Role.ADMIN,
      });
      return member!.invitationId;
    },
    async stale(
      organizationId: string,
      invitationId: string,
      failRefresh: boolean,
    ) {
      await client.revokeInvitation({ organizationId, invitationId });
      listRequests.length = 0;
      failNextList = failRefresh;
    },
  };
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const base = createBaseMock(options);
  if (sessionStorage.getItem("sandrun-e2e-invitation-revoke") === "true") {
    const { transport, ...fixture } = invitationRevokeFixture(base);
    window.sandrunInvitationRevokeFixture = fixture;
    return transport;
  }
  return {
    ...base,
    async unary(method, ...args) {
      if (method.name !== MemberService.method.acceptInvitation.name) {
        return base.unary(method, ...args);
      }
      const mode = sessionStorage.getItem("sandrun-e2e-invitation-response");
      const response = await base.unary(method, ...args);
      // Accept through the seeded mock first so the same transport can list
      // the resulting membership after the test changes the response.
      if (mode === "held") {
        await new Promise<void>((resolve) => {
          window.addEventListener(
            "sandrun-e2e-release-invitation",
            () => resolve(),
            {
              once: true,
            },
          );
          sessionStorage.setItem("sandrun-e2e-invitation-held", "true");
        });
      }
      if (isMessage(response.message, AcceptInvitationResponseSchema)) {
        if (mode === "missing" || mode === "held") {
          delete response.message.organization;
        }
        if (
          response.message.organization &&
          (mode === "empty" || mode === "whitespace")
        ) {
          response.message.organization.organizationId =
            mode === "empty" ? "" : "   ";
        }
      }
      return response;
    },
  };
}
