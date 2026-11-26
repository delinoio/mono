// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import {
  ApiKeyService,
  type RevokeApiKeyRequest,
} from "@delino/ownsand-protos/v1/api_key_pb";
import { ApiKeyState } from "@delino/ownsand-protos/v1/common_pb";
import { MOCK_IDS } from "../src/mock/mock-api.ts";

export const FIRST_KEY_ID = "0194993a-5800-7000-8000-000000000002";
export const SECOND_KEY_ID = "0194993a-5800-7000-8000-000000000003";
export const KEY_PREFIX = "osk_12ab";
export const OTHER_KEY_PREFIX = "osk_34cd";

/** Serve fixed keys and track exactly which key the console revokes. */
export function apiKeyCollisionTransport(
  base: Transport,
  {
    secondPrefix = KEY_PREFIX,
    beforeContinuation,
    onRevoke,
  }: {
    secondPrefix?: string;
    beforeContinuation?: () => Promise<void>;
    onRevoke: (request: RevokeApiKeyRequest) => void;
  },
): Transport {
  const metadata = {
    organizationId: MOCK_IDS.acme,
    name: "ci-key",
    state: ApiKeyState.ACTIVE,
    createTime: { seconds: 1737737600n },
  };
  const keys = [
    { ...metadata, apiKeyId: FIRST_KEY_ID, keyPrefix: KEY_PREFIX },
    { ...metadata, apiKeyId: SECOND_KEY_ID, keyPrefix: secondPrefix },
  ];
  const nextPageToken = "page-2";
  const fixture = createRouterTransport((router) => {
    router.rpc(ApiKeyService.method.listApiKeys, async ({ pageToken }) => {
      if (beforeContinuation === undefined) return { apiKeys: keys };
      if (pageToken === "") {
        return { apiKeys: keys.slice(0, 1), nextPageToken };
      }
      if (pageToken !== nextPageToken) {
        throw new ConnectError("Unexpected page token", Code.InvalidArgument);
      }
      await beforeContinuation();
      return { apiKeys: keys.slice(1) };
    });
    router.rpc(ApiKeyService.method.revokeApiKey, (request) => {
      onRevoke(request);
      const key = keys.find((key) => key.apiKeyId === request.apiKeyId);
      if (key === undefined) {
        throw new ConnectError("Unknown API key", Code.NotFound);
      }
      key.state = ApiKeyState.REVOKED;
      return { apiKey: key };
    });
  });
  return {
    ...base,
    unary(method, signal, timeout, header, input, contextValues) {
      const request = input as typeof input & { organizationId?: string };
      const transport =
        request.organizationId === MOCK_IDS.acme &&
        method.parent.typeName === ApiKeyService.typeName &&
        (method.name === "ListApiKeys" || method.name === "RevokeApiKey")
          ? fixture
          : base;
      return transport.unary(
        method,
        signal,
        timeout,
        header,
        input,
        contextValues,
      );
    },
  };
}
