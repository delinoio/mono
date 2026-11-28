// SPDX-License-Identifier: Apache-2.0
import type { LogtoSettings } from "../config.ts";

function base64Url(value: object): string {
  return btoa(JSON.stringify(value))
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll("=", "");
}

/** Seeds the ID token that the pinned SDK reads to restore a test session. */
export function seedLogtoIdToken({
  appId,
  endpoint,
}: Pick<LogtoSettings, "appId" | "endpoint">): void {
  const idToken = [
    base64Url({ alg: "ES384", typ: "JWT" }),
    base64Url({ sub: "user-owner", iss: `${endpoint}/oidc` }),
    "signature",
  ].join(".");
  window.localStorage.setItem(`logto:${appId}:idToken`, idToken);
}
