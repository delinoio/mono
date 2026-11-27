// SPDX-License-Identifier: Apache-2.0
import { isMessage } from "@bufbuild/protobuf";
import type { Transport } from "@connectrpc/connect";
import {
  AcceptInvitationResponseSchema,
  MemberService,
} from "@delino/sandrun-protos/v1/member_pb";
import {
  createMockTransport as createBaseMock,
  type MockTransportOptions,
} from "../src/mock/mock-api.ts";

export function createMockTransport(options?: MockTransportOptions): Transport {
  const base = createBaseMock(options);
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
