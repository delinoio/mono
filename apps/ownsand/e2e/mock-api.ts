// SPDX-License-Identifier: Apache-2.0
import type { Browser, WebRoute } from "@e2e-dev/web";

/**
 * Network-layer mock of ownsand-api for the e2e suite. The app calls the API
 * origin below with the Connect protocol and JSON encoding; every request is
 * answered by `browser.route`, so no server runs.
 */
export const E2E_API_ORIGIN = "http://127.0.0.1:18300";

/** `sessionStorage` key of the e2e sign-in stand-in (src/auth/fake-auth.tsx). */
export const FAKE_SESSION_KEY = "ownsand.e2e.fakeSession";

/** `sessionStorage` key of a pending invitation secret (src/invitation.ts). */
export const PENDING_INVITATION_KEY = "ownsand.pendingInvitationSecret";

export type JsonObject = { [key: string]: unknown };

/** A Connect error answer: code name and HTTP status. */
export class MockError {
  constructor(
    readonly code: string,
    readonly status: number,
  ) {}
}

export type Handler = (request: JsonObject) => JsonObject | MockError;

/** One recorded call: `Service/Method` and its JSON body. */
export interface RecordedCall {
  readonly procedure: string;
  readonly body: JsonObject;
  readonly authorization: string | undefined;
}

const CORS_HEADERS = {
  "access-control-allow-origin": "*",
  "access-control-allow-headers": "*",
  "access-control-allow-methods": "POST, OPTIONS",
};

export const ORGANIZATION_ID = "0190a000-0000-7000-8000-000000000001";
export const DEVICE_ID = "0190a000-0000-7000-8000-0000000000d1";

/** Handlers for an organization in which the caller has `role`. */
export function organizationHandlers(
  role: "ROLE_OWNER" | "ROLE_ADMIN" | "ROLE_MEMBER",
): Record<string, Handler> {
  const organization = {
    organizationId: ORGANIZATION_ID,
    name: "Acme",
    callerRole: role,
  };
  const device = {
    organizationId: ORGANIZATION_ID,
    deviceId: DEVICE_ID,
    name: "build-mac",
    state: "DEVICE_STATE_ONLINE",
    capabilities: {
      hostOs: "HOST_OS_MACOS",
      arch: "ARCH_ARM64",
      agentVersion: "1.0.0",
      logicalCpuCount: "10",
      memoryMib: "32768",
    },
    limits: { vcpuCount: "8", memoryMib: "16384", diskGib: "200" },
    allocated: { vcpuCount: "2", memoryMib: "4096", diskGib: "20" },
    agentVersion: "1.0.0",
    updateState: "AGENT_UPDATE_STATE_UP_TO_DATE",
  };
  return {
    "OrganizationService/ListOrganizations": () => ({
      organizations: [organization],
    }),
    "OrganizationService/GetOrganization": () => ({ organization }),
    "SandboxService/ListSandboxes": () => ({
      sandboxes: [
        {
          organizationId: ORGANIZATION_ID,
          sandboxId: "0190a000-0000-7000-8000-0000000000s1",
          deviceId: DEVICE_ID,
          state: "SANDBOX_STATE_RUNNING",
          guestOs: "GUEST_OS_LINUX",
          resources: { vcpuCount: "2", memoryMib: "4096", diskGib: "20" },
          maxDeadline: "2026-11-21T00:00:00Z",
          labels: { team: "infra" },
        },
      ],
    }),
    "DeviceService/ListDevices": () => ({ devices: [device] }),
    "DeviceService/GetDevice": () => ({ device }),
    "DeviceService/GetAgentVersionPolicy": () => ({
      policy: {
        organizationId: ORGANIZATION_ID,
        mode: "AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT",
        defaultVersions: { "linux/amd64": "1.0.0" },
      },
    }),
    "MemberService/ListMembers": () => ({
      members: [
        {
          organizationId: ORGANIZATION_ID,
          userId: "e2e-user",
          role,
          displayName: "E2E User",
        },
        {
          organizationId: ORGANIZATION_ID,
          userId: "other-user",
          role: "ROLE_MEMBER",
        },
      ],
    }),
    "MemberService/ListInvitations": () => ({ invitations: [] }),
    "ApiKeyService/ListApiKeys": () => ({
      apiKeys: [
        {
          organizationId: ORGANIZATION_ID,
          apiKeyId: "0190a000-0000-7000-8000-0000000000k1",
          name: "ci",
          state: "API_KEY_STATE_ACTIVE",
          secretPrefix: "osk_ab",
        },
      ],
    }),
    "ApiKeyService/CreateApiKey": (request) => ({
      apiKey: {
        organizationId: ORGANIZATION_ID,
        apiKeyId: "0190a000-0000-7000-8000-0000000000k2",
        name: request.name,
        state: "API_KEY_STATE_ACTIVE",
        secretPrefix: "osk_e2",
      },
      apiKeySecret: "osk_e2e_one_time_secret",
    }),
    "ApiKeyService/RevokeApiKey": () => ({}),
    "DeviceService/ListEnrollmentTokens": () => ({ enrollmentTokens: [] }),
  };
}

/** Installs the mock and returns the list that records every call. */
export async function installMockApi(
  browser: Browser,
  handlers: Record<string, Handler>,
): Promise<RecordedCall[]> {
  const calls: RecordedCall[] = [];
  await browser.route(`${E2E_API_ORIGIN}/**`, async (route: WebRoute) => {
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers: CORS_HEADERS });
      return;
    }
    const path = new URL(route.request.url).pathname;
    // `/delino.ownsand.v1.<Service>/<Method>` -> `<Service>/<Method>`.
    const procedure = path.replace(/^\/delino\.ownsand\.v1\./, "");
    const body = route.request.postData
      ? (JSON.parse(route.request.postData) as JsonObject)
      : {};
    calls.push({
      procedure,
      body,
      authorization: route.request.headers["authorization"],
    });
    const handler = handlers[procedure];
    const result = handler
      ? handler(body)
      : new MockError("unimplemented", 501);
    if (result instanceof MockError) {
      await route.fulfill({
        status: result.status,
        headers: CORS_HEADERS,
        json: { code: result.code, message: "mock error" },
      });
      return;
    }
    await route.fulfill({
      status: 200,
      headers: CORS_HEADERS,
      json: result as never,
    });
  });
  return calls;
}

/** Signs the e2e stand-in user in for every document of the attempt. */
export async function signInBeforeLoad(browser: Browser): Promise<void> {
  await browser.addInitScript((key: string) => {
    try {
      window.sessionStorage.setItem(key, "1");
    } catch {
      // Opaque documents such as about:blank have no sessionStorage.
    }
  }, FAKE_SESSION_KEY);
}
