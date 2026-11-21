// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  Role,
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "ownsand-sdk/gen/common_pb";
import { sandboxSettling, snapshotSettling } from "./api.ts";
import { clearInvitationOnLeave } from "./App.tsx";
import { AuthMode, DEFAULT_API_ORIGIN, readConsoleConfig } from "./config.ts";
import {
  effectiveVcpuLimit,
  formatDuration,
  formatMib,
  OVERCOMMIT_PERCENTAGES,
} from "./format.ts";
import { parseLabels, ttlErrors } from "./forms.ts";
import {
  invitationLink,
  PENDING_INVITATION_KEY,
  pendingInvitation,
  secretFromFragment,
  secretFromInput,
  takeSecretFromAddress,
} from "./invitation.ts";
import { can, canManageMember, Permission } from "./roles.ts";
import { parseRoute, RouteId, routePath } from "./router.tsx";

describe("console configuration", () => {
  test("defaults to the production API origin and Logto mode", () => {
    const config = readConsoleConfig({}, { production: true });
    expect(config.apiOrigin).toBe(DEFAULT_API_ORIGIN);
    expect(config.authMode).toBe(AuthMode.Logto);
    expect(config.logto).toBeUndefined();
  });

  test("reads the Logto registration and scopes", () => {
    const config = readConsoleConfig(
      {
        PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app/",
        PUBLIC_LOGTO_APP_ID: "app123",
        PUBLIC_LOGTO_SCOPES: " read:sandboxes  write:sandboxes ",
      },
      { production: true },
    );
    expect(config.logto).toEqual({
      endpoint: "https://tenant.logto.app",
      appId: "app123",
      scopes: ["ownsand:use", "read:sandboxes", "write:sandboxes"],
    });
  });

  test("always requests the ownsand:use scope", () => {
    const config = readConsoleConfig(
      {
        PUBLIC_LOGTO_ENDPOINT: "https://tenant.logto.app",
        PUBLIC_LOGTO_APP_ID: "app123",
      },
      { production: true },
    );
    expect(config.logto?.scopes).toEqual(["ownsand:use"]);
  });

  test.each([
    "https://api.test/path",
    "ftp://api.test",
    "https://u:p@api.test",
    "nope",
  ])("rejects the API origin %s", (value) => {
    expect(() =>
      readConsoleConfig({ PUBLIC_API_ORIGIN: value }, { production: false }),
    ).toThrow("PUBLIC_API_ORIGIN");
  });

  test("enables the mock API only outside production", () => {
    expect(
      readConsoleConfig(
        { PUBLIC_OWNSAND_MOCK_API: "true" },
        { production: false },
      ).authMode,
    ).toBe(AuthMode.Mock);
    expect(() =>
      readConsoleConfig(
        { PUBLIC_OWNSAND_MOCK_API: "true" },
        { production: true },
      ),
    ).toThrow("production");
    expect(() =>
      readConsoleConfig(
        { PUBLIC_OWNSAND_MOCK_API: "yes" },
        { production: false },
      ),
    ).toThrow("true or false");
  });
});

describe("invitation secrets", () => {
  test("builds a link with the secret in the fragment and reads it back", () => {
    const link = invitationLink("https://ownsand.delino.io", "osi_a+b/c");
    expect(link).toBe(
      "https://ownsand.delino.io/invitations/accept#secret=osi_a%2Bb%2Fc",
    );
    expect(secretFromInput(link)).toBe("osi_a+b/c");
    expect(secretFromFragment(new URL(link).hash)).toBe("osi_a+b/c");
  });

  test("accepts a bare secret and rejects other text", () => {
    expect(secretFromInput(" osi_123 ")).toBe("osi_123");
    expect(secretFromInput("two words")).toBeUndefined();
    expect(
      secretFromInput("https://ownsand.delino.io/invitations/accept"),
    ).toBeUndefined();
    expect(secretFromInput("")).toBeUndefined();
  });

  test("removes the fragment from the address bar after reading it", () => {
    const calls: string[] = [];
    const secret = takeSecretFromAddress(
      { hash: "#secret=osi_x", pathname: "/invitations/accept", search: "" },
      {
        replaceState: (_s: unknown, _t: string, url?: string | URL | null) =>
          calls.push(String(url)),
      },
    );
    expect(secret).toBe("osi_x");
    expect(calls).toEqual(["/invitations/accept"]);
  });

  test("keeps the pending secret in sessionStorage only", () => {
    window.localStorage.clear();
    pendingInvitation.save("osi_pending");
    expect(window.sessionStorage.getItem(PENDING_INVITATION_KEY)).toBe(
      "osi_pending",
    );
    expect(window.localStorage.length).toBe(0);
    expect(pendingInvitation.read()).toBe("osi_pending");
    pendingInvitation.clear();
    expect(pendingInvitation.read()).toBeUndefined();
  });

  test("navigating away from the invitation flow deletes the pending secret, the sign-in callback does not", () => {
    pendingInvitation.save("osi_pending");
    clearInvitationOnLeave(
      { id: RouteId.Invitation },
      { id: RouteId.Callback },
    );
    expect(pendingInvitation.read()).toBe("osi_pending");
    clearInvitationOnLeave(
      { id: RouteId.Callback },
      { id: RouteId.Invitation },
    );
    expect(pendingInvitation.read()).toBe("osi_pending");
    clearInvitationOnLeave(
      { id: RouteId.Invitation },
      { id: RouteId.Organizations },
    );
    expect(pendingInvitation.read()).toBeUndefined();
  });
});

describe("roles", () => {
  test("follow the authorization matrix", () => {
    expect(can(Role.MEMBER, Permission.ManageApiKeys)).toBe(false);
    expect(can(Role.ADMIN, Permission.ManageApiKeys)).toBe(true);
    expect(can(Role.ADMIN, Permission.DeleteOrganization)).toBe(false);
    expect(can(Role.OWNER, Permission.TransferOwnership)).toBe(true);
    expect(can(Role.OWNER, Permission.LeaveOrganization)).toBe(false);
    expect(can(Role.MEMBER, Permission.LeaveOrganization)).toBe(true);
    expect(can(Role.MEMBER, Permission.UseSandboxes)).toBe(true);
    expect(can(undefined, Permission.ReadOrganization)).toBe(false);
  });

  test("an admin cannot manage the owner or itself", () => {
    expect(
      canManageMember(Role.ADMIN, "me", { userId: "o", role: Role.OWNER }),
    ).toBe(false);
    expect(
      canManageMember(Role.ADMIN, "me", { userId: "me", role: Role.ADMIN }),
    ).toBe(false);
    expect(
      canManageMember(Role.ADMIN, "me", { userId: "a", role: Role.ADMIN }),
    ).toBe(true);
    expect(
      canManageMember(Role.MEMBER, "me", { userId: "a", role: Role.MEMBER }),
    ).toBe(false);
  });
});

describe("formatting and form rules", () => {
  test("computes the effective vCPU limit with integer arithmetic", () => {
    expect(effectiveVcpuLimit(16, 150)).toBe(24);
    expect(effectiveVcpuLimit(3, 125)).toBe(3);
    expect(effectiveVcpuLimit(0, 400)).toBe(0);
    // The live input value can be fractional or invalid before it snaps.
    expect(effectiveVcpuLimit(1.5, 200)).toBe(2);
    expect(effectiveVcpuLimit(Number.NaN, 200)).toBe(0);
    expect(effectiveVcpuLimit(-3, 200)).toBe(0);
    expect(effectiveVcpuLimit(536870912, 400)).toBe(2147483648);
    expect(OVERCOMMIT_PERCENTAGES[0]).toBe(100);
    expect(OVERCOMMIT_PERCENTAGES.at(-1)).toBe(400);
    expect(OVERCOMMIT_PERCENTAGES).toHaveLength(13);
  });

  test("formats sizes and durations", () => {
    expect(formatMib(4096n)).toBe("4 GiB");
    expect(formatMib(512)).toBe("512 MiB");
    expect(formatDuration({ seconds: 5400n, nanos: 0 } as never)).toBe(
      "1 h 30 min",
    );
  });

  test("requires both TTLs, an idle TTL within the maximum and at most 7 days", () => {
    expect(ttlErrors({ idleMinutes: null, maxHours: 1 }).idle).toBeDefined();
    expect(ttlErrors({ idleMinutes: 30, maxHours: null }).max).toBeDefined();
    expect(ttlErrors({ idleMinutes: 120, maxHours: 1 }).idle).toContain(
      "longer",
    );
    expect(ttlErrors({ idleMinutes: 30, maxHours: 169 }).max).toContain(
      "7 days",
    );
    expect(ttlErrors({ idleMinutes: 60, maxHours: 168 })).toEqual({});
  });

  test("parses labels and enforces their limits", () => {
    expect(parseLabels("team=ci\n\n purpose = tests ")).toEqual({
      labels: { team: "ci", purpose: "tests" },
    });
    expect(parseLabels("novalue")).toHaveProperty("error");
    expect(parseLabels(`${"k".repeat(64)}=v`)).toHaveProperty("error");
    expect(
      parseLabels(Array.from({ length: 33 }, (_, i) => `k${i}=v`).join("\n")),
    ).toHaveProperty("error");
  });
});

describe("router", () => {
  test.each([
    ["/", { id: RouteId.Organizations }],
    ["/invitations/accept", { id: RouteId.Invitation }],
    ["/callback", { id: RouteId.Callback }],
    ["/orgs/o1/sandboxes", { id: RouteId.Sandboxes, organizationId: "o1" }],
    [
      "/orgs/o1/sandboxes/s1",
      { id: RouteId.Sandbox, organizationId: "o1", sandboxId: "s1" },
    ],
    [
      "/orgs/o1/devices/d1",
      { id: RouteId.Device, organizationId: "o1", deviceId: "d1" },
    ],
    [
      "/orgs/o1/enrollment-tokens",
      { id: RouteId.EnrollmentTokens, organizationId: "o1" },
    ],
    ["/orgs/o1/api-keys", { id: RouteId.ApiKeys, organizationId: "o1" }],
    ["/orgs/o1/snapshots", { id: RouteId.Snapshots, organizationId: "o1" }],
    ["/orgs/o1/settings", { id: RouteId.Settings, organizationId: "o1" }],
    ["/orgs/o1/members", { id: RouteId.Members, organizationId: "o1" }],
  ])("parses %s and builds it back", (path, route) => {
    expect(parseRoute(path)).toEqual(route);
    expect(routePath(route as never)).toBe(path);
  });

  test("unknown paths are not found", () => {
    expect(parseRoute("/orgs/o1/unknown").id).toBe(RouteId.NotFound);
    expect(parseRoute("/elsewhere").id).toBe(RouteId.NotFound);
  });

  test("malformed percent escapes are not found instead of throwing", () => {
    expect(parseRoute("/orgs/%/sandboxes")).toEqual({ id: RouteId.NotFound });
    expect(parseRoute("/orgs/o1/sandboxes/%E0%A4%A")).toEqual({
      id: RouteId.NotFound,
    });
  });

  test("valid encoded segments are decoded", () => {
    expect(parseRoute("/orgs/o%201/sandboxes")).toEqual({
      id: RouteId.Sandboxes,
      organizationId: "o 1",
    });
  });
});

describe("polling", () => {
  test("polls only while a sandbox or snapshot is in an asynchronous state", () => {
    expect(sandboxSettling({ state: SandboxState.CREATING })).toBe(true);
    expect(sandboxSettling({ state: SandboxState.DELETING })).toBe(true);
    expect(sandboxSettling({ state: SandboxState.RUNNING })).toBe(false);
    expect(sandboxSettling({ state: SandboxState.UNREACHABLE })).toBe(false);
    const ready = {
      state: SnapshotState.READY,
      exportState: SnapshotExportState.NOT_EXPORTED,
    };
    expect(snapshotSettling(ready)).toBe(false);
    expect(snapshotSettling({ ...ready, state: SnapshotState.CREATING })).toBe(
      true,
    );
    expect(
      snapshotSettling({
        ...ready,
        exportState: SnapshotExportState.EXPORTING,
      }),
    ).toBe(true);
  });
});
