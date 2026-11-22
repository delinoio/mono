// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  Role,
  SandboxState,
  SnapshotExportState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import { CreateSandboxRequestSchema } from "@delino/ownsand-protos/v1/sandbox_pb";
import { sandboxSettling, snapshotSettling } from "./api.ts";
import { shortIdText } from "./components/ui.tsx";
import { clearInvitationOnLeave, clearInvitationOutsideFlow } from "./App.tsx";
import {
  AuthMode,
  DEFAULT_API_ORIGIN,
  normalizeMockFlag,
  readConsoleConfig,
} from "./config.ts";
import {
  BYTE_SECONDS_PER_GIB_MONTH,
  deviceAllocatedVcpu,
  effectiveVcpuLimit,
  formatByteCount,
  formatCount,
  formatDuration,
  formatGibMonths,
  formatMib,
  formatUsageCostUsdMicros,
  formatUsdMicros,
  OVERCOMMIT_PERCENTAGES,
} from "./format.ts";
import {
  DeviceLimitField,
  deviceLimitsInput,
  parseInt64Limit,
  parseLabels,
  resourceErrors,
  ttlErrors,
} from "./forms.ts";
import {
  invitationLink,
  openInvitationFromAddress,
  PENDING_INVITATION_KEY,
  pendingInvitation,
  secretFromFragment,
  secretFromInput,
  takeSecretFromAddress,
} from "./invitation.ts";
import { can, canManageMember, Permission } from "./roles.ts";
import {
  isInvitationFlowRoute,
  parseRoute,
  type Route,
  RouteId,
  routePath,
} from "./router.tsx";

describe("shortIdText", () => {
  test("keeps the first 8 and the last 4 characters of a long id", () => {
    expect(shortIdText("0192f4a0-7c1e-7d3b-9a4f-3b8e5c6d7e8f")).toBe(
      "0192f4a0…7e8f",
    );
  });

  test("tells apart UUID v7 ids that share the time prefix", () => {
    const first = "0192f4a0-7c1e-7d3b-9a4f-3b8e5c6d7e8f";
    const second = "0192f4a0-7c20-7a11-8b22-112233441234";
    expect(first.slice(0, 8)).toBe(second.slice(0, 8));
    expect(shortIdText(first)).not.toBe(shortIdText(second));
  });

  test("keeps an id of 13 characters or fewer unchanged", () => {
    expect(shortIdText("abc")).toBe("abc");
    expect(shortIdText("0123456789abc")).toBe("0123456789abc");
  });
});

describe("deviceAllocatedVcpu", () => {
  test("prefers the exact 64-bit total above the int32 range", () => {
    expect(
      deviceAllocatedVcpu({
        allocation: { vcpuCount: 2147483647, vcpuTotalCount: 4294967294n },
      }),
    ).toBe(4294967294n);
    expect(
      deviceAllocatedVcpu({
        allocation: { vcpuCount: 3, vcpuTotalCount: 3n },
      }),
    ).toBe(3n);
  });

  test("a present zero total is exact", () => {
    expect(
      deviceAllocatedVcpu({ allocation: { vcpuCount: 0, vcpuTotalCount: 0n } }),
    ).toBe(0n);
  });

  test("falls back to the int32 count for a server without the total", () => {
    expect(
      deviceAllocatedVcpu({
        allocation: { vcpuCount: 7, vcpuTotalCount: undefined },
      }),
    ).toBe(7n);
  });

  test("is 0 without an allocation", () => {
    expect(deviceAllocatedVcpu({})).toBe(0n);
  });
});

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

  test.each([
    "https://api.example.test\\tenant\\..",
    "https://api.example.test\\",
    "https://api.example.test/tenant/..",
  ])("rejects a hidden path in the API origin %s", (value) => {
    expect(() =>
      readConsoleConfig({ PUBLIC_API_ORIGIN: value }, { production: false }),
    ).toThrow(
      "PUBLIC_API_ORIGIN must be an origin without a path, query or fragment",
    );
  });

  test.each([
    "https://api.example.test\\tenant\\..",
    "https://api.example.test\\",
    "https://api.example.test/tenant/..",
  ])("rejects a hidden path in the Logto endpoint %s", (value) => {
    expect(() =>
      readConsoleConfig(
        { PUBLIC_LOGTO_ENDPOINT: value, PUBLIC_LOGTO_APP_ID: "app123" },
        { production: true },
      ),
    ).toThrow(
      "PUBLIC_LOGTO_ENDPOINT must be an origin without a path, query or fragment",
    );
  });

  test("omits the rejected value from the origin error", () => {
    expect(() =>
      readConsoleConfig(
        { PUBLIC_API_ORIGIN: "https://secret.example.test\\tenant\\.." },
        { production: false },
      ),
    ).not.toThrow(/secret/);
  });

  test.each([
    ["https://api.example.test", "https://api.example.test"],
    ["https://api.example.test/", "https://api.example.test"],
    ["http://127.0.0.1:18300/", "http://127.0.0.1:18300"],
  ])("accepts the origin %s", (value, origin) => {
    expect(
      readConsoleConfig({ PUBLIC_API_ORIGIN: value }, { production: false })
        .apiOrigin,
    ).toBe(origin);
    expect(
      readConsoleConfig(
        { PUBLIC_LOGTO_ENDPOINT: value, PUBLIC_LOGTO_APP_ID: "app123" },
        { production: true },
      ).logto?.endpoint,
    ).toBe(origin);
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

describe("mock mode decision", () => {
  test("a padded value selects the mock mode and normalizes to true", () => {
    const env = { PUBLIC_OWNSAND_MOCK_API: " true " };
    expect(readConsoleConfig(env, { production: false }).authMode).toBe(
      AuthMode.Mock,
    );
    // src/index.tsx compares the inlined value with "true".
    expect(normalizeMockFlag(env.PUBLIC_OWNSAND_MOCK_API)).toBe("true");
  });

  test("false, empty and unset values stay out of the mock mode", () => {
    for (const value of ["false", " false ", "", "  ", undefined]) {
      expect(normalizeMockFlag(value)).not.toBe("true");
      expect(
        readConsoleConfig(
          { PUBLIC_OWNSAND_MOCK_API: value },
          { production: false },
        ).authMode,
      ).toBe(AuthMode.Logto);
    }
  });

  test("a padded mock value is still rejected in a production build", () => {
    expect(() =>
      readConsoleConfig(
        { PUBLIC_OWNSAND_MOCK_API: " true " },
        { production: true },
      ),
    ).toThrow(/production/);
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

  test("drops the stored secret only when the address carries a new one", () => {
    const history = { replaceState: () => {} };
    const at = (hash: string) => ({
      hash,
      pathname: "/invitations/accept",
      search: "",
    });
    window.sessionStorage.clear();
    pendingInvitation.save("osi_old");
    expect(openInvitationFromAddress(at(""), history)).toBeUndefined();
    expect(pendingInvitation.read()).toBe("osi_old");
    expect(openInvitationFromAddress(at("#other=1"), history)).toBeUndefined();
    expect(pendingInvitation.read()).toBe("osi_old");
    expect(openInvitationFromAddress(at("#secret=osi_new"), history)).toBe(
      "osi_new",
    );
    expect(pendingInvitation.read()).toBeUndefined();
    window.sessionStorage.clear();
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

  test("only the invitation and callback routes belong to the invitation flow", () => {
    const flow = [RouteId.Invitation, RouteId.Callback];
    for (const route of [
      { id: RouteId.Organizations },
      { id: RouteId.Invitation },
      { id: RouteId.Callback },
      { id: RouteId.NotFound },
      { id: RouteId.Sandboxes, organizationId: "o" },
      { id: RouteId.Settings, organizationId: "o" },
    ] as Route[]) {
      expect(isInvitationFlowRoute(route)).toBe(flow.includes(route.id));
    }
  });

  test("the start-up check deletes the pending secret outside the invitation flow only", () => {
    pendingInvitation.save("osi_pending");
    clearInvitationOutsideFlow(parseRoute("/invitations/accept"));
    clearInvitationOutsideFlow(parseRoute("/callback"));
    expect(pendingInvitation.read()).toBe("osi_pending");
    clearInvitationOutsideFlow(parseRoute("/orgs/o/members"));
    expect(pendingInvitation.read()).toBeUndefined();
    pendingInvitation.save("osi_pending");
    clearInvitationOutsideFlow(parseRoute("/"));
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
    expect(can(Role.OWNER, Permission.ViewBilling)).toBe(true);
    expect(can(Role.ADMIN, Permission.ViewBilling)).toBe(true);
    expect(can(Role.MEMBER, Permission.ViewBilling)).toBe(false);
    expect(can(Role.OWNER, Permission.ManageBillingLink)).toBe(true);
    expect(can(Role.ADMIN, Permission.ManageBillingLink)).toBe(false);
    expect(can(Role.MEMBER, Permission.ManageBillingLink)).toBe(false);
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

  test("keeps the resources of the create form within the request ranges", () => {
    expect(resourceErrors({ vcpu: 2, memory: 4096, disk: 20 })).toEqual({});
    expect(
      resourceErrors({ vcpu: 2147483647, memory: 9007199254740991, disk: 1 }),
    ).toEqual({});
    // Empty and non-positive values keep their messages.
    expect(resourceErrors({ vcpu: null, memory: null, disk: null })).toEqual({
      vcpu: "Enter at least 1 vCPU.",
      memory: "Enter the memory in MiB.",
      disk: "Enter the disk size in GiB.",
    });
    expect(resourceErrors({ vcpu: 0, memory: -1, disk: 0 })).toEqual({
      vcpu: "Enter at least 1 vCPU.",
      memory: "Enter the memory in MiB.",
      disk: "Enter the disk size in GiB.",
    });
    // The vCPU count is an int32; memory and disk stay exact below 2^53.
    expect(resourceErrors({ vcpu: 2147483648, memory: 1, disk: 1 }).vcpu).toBe(
      "Enter a whole number of vCPUs from 1 to 2147483647.",
    );
    expect(
      resourceErrors({ vcpu: 3000000000, memory: 1, disk: 1 }).vcpu,
    ).toContain("1 to 2147483647");
    expect(
      resourceErrors({ vcpu: 1, memory: 9007199254740992, disk: 1 }).memory,
    ).toContain("1 to 9007199254740991");
    expect(resourceErrors({ vcpu: 1, memory: 1, disk: 1e19 }).disk).toContain(
      "1 to 9007199254740991",
    );
    expect(resourceErrors({ vcpu: 1.5, memory: 1.5, disk: 1.5 })).toEqual({
      vcpu: "Enter a whole number of vCPUs from 1 to 2147483647.",
      memory: "Enter a whole number of MiB from 1 to 9007199254740991.",
      disk: "Enter a whole number of GiB from 1 to 9007199254740991.",
    });
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

  test("keeps labels whose keys match object prototype properties", () => {
    const parsed = parseLabels(
      "__proto__=x\nconstructor=y\ntoString=z\n__proto__=w",
    );
    if ("error" in parsed) throw new Error(parsed.error);
    expect(Object.keys(parsed.labels).sort()).toEqual([
      "__proto__",
      "constructor",
      "toString",
    ]);
    expect(Object.hasOwn(parsed.labels, "__proto__")).toBe(true);
    expect(
      Object.getOwnPropertyDescriptor(parsed.labels, "__proto__")?.value,
    ).toBe("w");
    // The request that the console sends keeps the label through protobuf.
    const request = create(CreateSandboxRequestSchema, {
      labels: parsed.labels,
    });
    const decoded = fromBinary(
      CreateSandboxRequestSchema,
      toBinary(CreateSandboxRequestSchema, request),
    );
    expect(Object.hasOwn(request.labels, "__proto__")).toBe(true);
    expect(Object.hasOwn(decoded.labels, "__proto__")).toBe(true);
    expect(
      Object.getOwnPropertyDescriptor(decoded.labels, "__proto__")?.value,
    ).toBe("w");
  });

  test("counts a prototype-named key toward the label limit", () => {
    const lines = Array.from({ length: 32 }, (_, i) => `k${i}=v`);
    expect(parseLabels([...lines, "__proto__=v"].join("\n"))).toHaveProperty(
      "error",
    );
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

  test("treats inherited object property names as unknown sections", () => {
    for (const name of [
      "constructor",
      "toString",
      "__proto__",
      "hasOwnProperty",
    ]) {
      expect(parseRoute(`/orgs/o1/${name}`)).toEqual({ id: RouteId.NotFound });
    }
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

describe("parseInt64Limit", () => {
  test("keeps every value of the int64 range exact", () => {
    expect(parseInt64Limit("0")).toEqual({ value: 0n });
    expect(parseInt64Limit("1000")).toEqual({ value: 1000n });
    // 2^53 + 1 is not representable as a number.
    expect(parseInt64Limit("9007199254740993")).toEqual({
      value: 9007199254740993n,
    });
    expect(parseInt64Limit("9223372036854775807")).toEqual({
      value: 9223372036854775807n,
    });
  });

  test("trims whitespace and treats an empty input as blank", () => {
    expect(parseInt64Limit(" 42 ")).toEqual({ value: 42n });
    expect(parseInt64Limit("")).toEqual({ error: "blank" });
    expect(parseInt64Limit("   ")).toEqual({ error: "blank" });
  });

  test("rejects values that are not a whole number of the int64 range", () => {
    for (const text of [
      "9223372036854775808",
      "-1",
      "+1",
      "1.5",
      "1e3",
      "1,000",
      "0x10",
      "12 34",
      "abc",
    ]) {
      expect(parseInt64Limit(text)).toEqual({ error: "invalid" });
    }
  });
});

describe("deviceLimitsInput", () => {
  const base = {
    vcpu: 4,
    memoryMib: "8192",
    diskGib: "100",
    overcommitPercent: 150,
  };

  test("sends an explicit zero as a present value", () => {
    const result = deviceLimitsInput({
      vcpu: 0,
      memoryMib: "0",
      diskGib: "0",
      overcommitPercent: 100,
    });
    expect("limits" in result).toBe(true);
    if (!("limits" in result)) return;
    expect(result.limits.vcpuCount).toBe(0);
    expect(result.limits.memoryMib).toBe(0n);
    expect(result.limits.diskGib).toBe(0n);
    expect(result.limits.vcpuOvercommitPercent).toBe(100);
  });

  test("sends memory and disk above 2^53 exactly", () => {
    const result = deviceLimitsInput({
      ...base,
      memoryMib: "9223372036854775807",
      diskGib: "9007199254740993",
    });
    expect("limits" in result).toBe(true);
    if (!("limits" in result)) return;
    expect(result.limits.memoryMib).toBe(9223372036854775807n);
    expect(result.limits.diskGib).toBe(9007199254740993n);
  });

  test("asks for a blank field instead of sending zero", () => {
    expect(deviceLimitsInput({ ...base, vcpu: null })).toEqual({
      error: "vCPU limit is required. Enter 0 to allow none.",
      field: DeviceLimitField.Vcpu,
    });
    expect(deviceLimitsInput({ ...base, memoryMib: "" })).toEqual({
      error: "Memory limit is required. Enter 0 to allow none.",
      field: DeviceLimitField.Memory,
    });
    expect(deviceLimitsInput({ ...base, diskGib: "  " })).toEqual({
      error: "Disk limit is required. Enter 0 to allow none.",
      field: DeviceLimitField.Disk,
    });
  });

  test("rejects an out-of-range memory or disk limit", () => {
    expect(deviceLimitsInput({ ...base, memoryMib: "-1" })).toEqual({
      error:
        "Memory limit must be a whole number from 0 to 9223372036854775807.",
      field: DeviceLimitField.Memory,
    });
    expect(
      deviceLimitsInput({ ...base, diskGib: "9223372036854775808" }),
    ).toEqual({
      error: "Disk limit must be a whole number from 0 to 9223372036854775807.",
      field: DeviceLimitField.Disk,
    });
    expect(deviceLimitsInput({ ...base, diskGib: "1.5" })).toEqual({
      error: "Disk limit must be a whole number from 0 to 9223372036854775807.",
      field: DeviceLimitField.Disk,
    });
  });
});

describe("billing amounts", () => {
  test("formats balances in USD with 2 decimals, half away from zero", () => {
    expect(formatUsdMicros(0n)).toBe("$0.00");
    expect(formatUsdMicros(1n)).toBe("$0.00");
    expect(formatUsdMicros(4_999n)).toBe("$0.00");
    expect(formatUsdMicros(5_000n)).toBe("$0.01");
    expect(formatUsdMicros(9_999n)).toBe("$0.01");
    expect(formatUsdMicros(10_000n)).toBe("$0.01");
    expect(formatUsdMicros(14_999n)).toBe("$0.01");
    expect(formatUsdMicros(15_000n)).toBe("$0.02");
    expect(formatUsdMicros(1_234_567_890n)).toBe("$1,234.57");
  });

  test("formats debt with a minus sign and never shows a negative zero", () => {
    expect(formatUsdMicros(-1_234_567n)).toBe("-$1.23");
    expect(formatUsdMicros(-5_000n)).toBe("-$0.01");
    expect(formatUsdMicros(-4_999n)).toBe("$0.00");
    expect(formatUsdMicros(-1n)).toBe("$0.00");
  });

  test("keeps amounts beyond 2^53 exact", () => {
    // 2^53 + 1 micro-USD is not representable as a number.
    expect(formatUsdMicros(9_007_199_254_740_993n)).toBe("$9,007,199,254.74");
    expect(formatUsdMicros(9_223_372_036_854_775_807n)).toBe(
      "$9,223,372,036,854.78",
    );
    expect(formatUsdMicros(-9_223_372_036_854_775_808n)).toBe(
      "-$9,223,372,036,854.78",
    );
    expect(formatUsdMicros(9_007_199_254_745_000n)).toBe("$9,007,199,254.75");
  });

  test("shows usage costs below $0.01 with up to 6 decimals", () => {
    expect(formatUsageCostUsdMicros(0n)).toBe("$0.00");
    expect(formatUsageCostUsdMicros(1n)).toBe("$0.000001");
    expect(formatUsageCostUsdMicros(120n)).toBe("$0.00012");
    expect(formatUsageCostUsdMicros(5_000n)).toBe("$0.005");
    expect(formatUsageCostUsdMicros(9_999n)).toBe("$0.009999");
    expect(formatUsageCostUsdMicros(-5_000n)).toBe("-$0.005");
  });

  test("shows usage costs of $0.01 and more with 2 decimals", () => {
    expect(formatUsageCostUsdMicros(10_000n)).toBe("$0.01");
    expect(formatUsageCostUsdMicros(15_000n)).toBe("$0.02");
    expect(formatUsageCostUsdMicros(9_007_199_254_740_993n)).toBe(
      "$9,007,199,254.74",
    );
  });

  test("shows R2 storage in GiB-months of 730 hours", () => {
    expect(BYTE_SECONDS_PER_GIB_MONTH).toBe(2n ** 30n * 2_628_000n);
    expect(formatGibMonths(0n)).toBe("0.000 GiB-months");
    expect(formatGibMonths(BYTE_SECONDS_PER_GIB_MONTH)).toBe(
      "1.000 GiB-months",
    );
    // One GiB for 30 days is 720 of 730 hours.
    expect(formatGibMonths(2n ** 30n * 2_592_000n)).toBe("0.986 GiB-months");
    // 0.0005 GiB-months rounds half away from zero.
    expect(formatGibMonths(BYTE_SECONDS_PER_GIB_MONTH / 2_000n)).toBe(
      "0.001 GiB-months",
    );
    expect(formatGibMonths(BYTE_SECONDS_PER_GIB_MONTH / 2_000n - 1n)).toBe(
      "less than 0.001 GiB-months",
    );
    expect(formatGibMonths(1n)).toBe("less than 0.001 GiB-months");
    expect(formatGibMonths(2n ** 62n)).toBe("1,634.310 GiB-months");
  });

  test("shows byte counts in binary units", () => {
    expect(formatByteCount(0n)).toBe("0 B");
    expect(formatByteCount(1_000n)).toBe("1,000 B");
    // 1,023 bytes are 0.999 KiB, which shows as 1.00 KiB.
    expect(formatByteCount(1_023n)).toBe("1.00 KiB");
    expect(formatByteCount(1_024n)).toBe("1.00 KiB");
    expect(formatByteCount(1_536n)).toBe("1.50 KiB");
    // Rounds up into the next unit instead of showing 1024.00 KiB.
    expect(formatByteCount(1_048_575n)).toBe("1.00 MiB");
    expect(formatByteCount(13_421_772_800n)).toBe("12.50 GiB");
    expect(formatByteCount(2n ** 60n)).toBe("1.00 EiB");
    expect(formatByteCount(9_223_372_036_854_775_807n)).toBe("8.00 EiB");
  });

  test("shows counts with thousands separators", () => {
    expect(formatCount(0n)).toBe("0");
    expect(formatCount(1_234_567n)).toBe("1,234,567");
    expect(formatCount(9_223_372_036_854_775_807n)).toBe(
      "9,223,372,036,854,775,807",
    );
  });
});
