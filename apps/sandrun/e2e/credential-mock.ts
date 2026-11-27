// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import type { MockTransportOptions } from "../src/mock/mock-api.ts";
import { createMockTransport as createInvitationMock } from "./invitation-mock.ts";

export const CREDENTIAL_FIXTURE = "sandrun-e2e-credential";
export const FIXTURE_KEY = "sandrun_e2e_fixture_key";
export const FIXTURE_INVITATION = "sri_e2e_fixture_invitation";

type RequestKind = "list" | "create";

interface CredentialFixture {
  holdNext?: RequestKind;
  release: Partial<Record<RequestKind, (fail?: boolean) => void>>;
  pageTokens: string[];
  oldPageDelivered: boolean;
}

declare global {
  interface Window {
    sandrunCredentialFixture?: CredentialFixture;
  }
}

export function createMockTransport(options?: MockTransportOptions): Transport {
  const base = createInvitationMock(options);
  const mode = sessionStorage.getItem(CREDENTIAL_FIXTURE);
  if (mode === null) return base;

  const fixture: CredentialFixture = {
    holdNext: mode === "cold-key" ? "list" : undefined,
    release: {},
    pageTokens: [],
    oldPageDelivered: false,
  };
  window.sandrunCredentialFixture = fixture;

  async function waitForRelease(kind: RequestKind) {
    if (fixture.holdNext !== kind) return;
    fixture.holdNext = undefined;
    const fail = await new Promise<boolean | undefined>((resolve) => {
      fixture.release[kind] = resolve;
    });
    delete fixture.release[kind];
    if (fail) {
      throw new ConnectError(
        `Fixture ${kind} failed`,
        kind === "create" ? Code.InvalidArgument : Code.Internal,
      );
    }
  }

  const listMethod = mode === "invitation" ? "ListInvitations" : "ListApiKeys";
  const createMethod =
    mode === "invitation" ? "CreateInvitation" : "CreateApiKey";
  return {
    ...base,
    async unary(method, ...args) {
      if (method.name === listMethod) {
        const response = await base.unary(method, ...args);
        // Strict Mode can cancel an initial request before it reaches the fixture.
        args[0]?.throwIfAborted();
        const token = (args[3] as { pageToken?: string }).pageToken ?? "";
        const isOldPage =
          mode === "cold-key" && fixture.pageTokens.length === 0;
        fixture.pageTokens.push(token);
        if (isOldPage) {
          Object.assign(response.message, { apiKeys: [], nextPageToken: "" });
        }
        await waitForRelease("list");
        if (isOldPage) fixture.oldPageDelivered = true;
        return response;
      }

      if (method.name === createMethod) {
        await waitForRelease("create");
        const response = await base.unary(method, ...args);
        Object.assign(
          response.message,
          mode === "invitation"
            ? { invitationSecret: FIXTURE_INVITATION }
            : { secret: FIXTURE_KEY },
        );
        return response;
      }

      return base.unary(method, ...args);
    },
  };
}
