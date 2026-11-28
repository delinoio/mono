// SPDX-License-Identifier: Apache-2.0
import type { Transport } from "@connectrpc/connect";

export const DISPLAY_NAME_FIXTURE = "sandrun-e2e-display-name-writes";

const displayNameMethods = new Set([
  "CreateOrganization",
  "CreateApiKey",
  "CreateAgentIntegration",
  "UpdateAgentIntegration",
]);

declare global {
  interface Window {
    sandrunDisplayNameWrites?: { method: string; name: string }[];
  }
}

export function observeDisplayNameWrites(base: Transport): Transport {
  if (sessionStorage.getItem(DISPLAY_NAME_FIXTURE) === null) return base;
  const writes: NonNullable<Window["sandrunDisplayNameWrites"]> = [];
  window.sandrunDisplayNameWrites = writes;
  return {
    ...base,
    unary(method, ...args) {
      const request = args[3];
      if (
        displayNameMethods.has(method.name) &&
        "name" in request &&
        typeof request.name === "string"
      ) {
        writes.push({ method: method.name, name: request.name });
      }
      return base.unary(method, ...args);
    },
  };
}
