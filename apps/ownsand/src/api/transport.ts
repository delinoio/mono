// SPDX-License-Identifier: Apache-2.0
import type { Interceptor, Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

/**
 * Adds the Logto access token as a bearer credential. The token is read for
 * every call, so the Logto SDK can refresh it, and it is never logged.
 */
export function bearerTokenInterceptor(
  getAccessToken: () => Promise<string>,
): Interceptor {
  return (next) => async (request) => {
    request.header.set("Authorization", `Bearer ${await getAccessToken()}`);
    return next(request);
  };
}

/**
 * Creates the Connect transport to ownsand-api. The console calls the control
 * plane only through its absolute origin, with the Connect protocol and JSON
 * encoding over `fetch`.
 */
export function createApiTransport(
  apiOrigin: string,
  getAccessToken: () => Promise<string>,
): Transport {
  return createConnectTransport({
    baseUrl: apiOrigin,
    interceptors: [bearerTokenInterceptor(getAccessToken)],
  });
}
