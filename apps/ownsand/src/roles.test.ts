// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { Role } from "@delino/ownsand-sdk/gen/common_pb";
import {
  ConsoleRole,
  Permission,
  can,
  canManageMember,
  fromProtoRole,
  grantableRoles,
  toProtoRole,
} from "./roles.ts";
import { ORGANIZATION_ROUTES, RouteId } from "./routes.ts";

/** Expected permissions, transcribed from the control plane authorization matrix. */
const MATRIX: Record<Permission, Record<ConsoleRole, boolean>> = {
  [Permission.ViewOrganization]: { owner: true, admin: true, member: true },
  [Permission.ViewSandboxes]: { owner: true, admin: true, member: true },
  [Permission.ViewDevices]: { owner: true, admin: true, member: true },
  [Permission.ViewMembers]: { owner: true, admin: true, member: true },
  [Permission.ViewAgentVersionPolicy]: {
    owner: true,
    admin: true,
    member: true,
  },
  [Permission.LeaveOrganization]: { owner: false, admin: true, member: true },
  [Permission.ManageInvitations]: { owner: true, admin: true, member: false },
  [Permission.ManageMembers]: { owner: true, admin: true, member: false },
  [Permission.ManageEnrollmentTokens]: {
    owner: true,
    admin: true,
    member: false,
  },
  [Permission.ManageDevices]: { owner: true, admin: true, member: false },
  [Permission.ManageApiKeys]: { owner: true, admin: true, member: false },
  [Permission.ChangeAgentVersionPolicy]: {
    owner: true,
    admin: true,
    member: false,
  },
  [Permission.TransferOwnership]: { owner: true, admin: false, member: false },
  [Permission.DeleteOrganization]: { owner: true, admin: false, member: false },
};

describe("can", () => {
  for (const [permission, roles] of Object.entries(MATRIX)) {
    for (const [role, allowed] of Object.entries(roles)) {
      test(`${role} ${allowed ? "may" : "may not"} ${permission}`, () => {
        expect(can(role as ConsoleRole, permission as Permission)).toBe(
          allowed,
        );
      });
    }
  }

  test("an unknown role may do nothing", () => {
    for (const permission of Object.values(Permission)) {
      expect(can(undefined, permission)).toBe(false);
    }
  });
});

describe("protobuf role mapping", () => {
  test("maps every role both ways", () => {
    for (const role of Object.values(ConsoleRole)) {
      expect(fromProtoRole(toProtoRole(role))).toBe(role);
    }
  });

  test("maps ROLE_UNSPECIFIED and unknown numbers to undefined", () => {
    expect(fromProtoRole(Role.UNSPECIFIED)).toBeUndefined();
    expect(fromProtoRole(99 as Role)).toBeUndefined();
  });
});

describe("canManageMember", () => {
  const admin = { userId: "a2", role: ConsoleRole.Admin };
  const owner = { userId: "o1", role: ConsoleRole.Owner };
  const member = { userId: "m1", role: ConsoleRole.Member };

  test("an admin manages members and other admins but not the owner", () => {
    expect(canManageMember(ConsoleRole.Admin, "a1", member)).toBe(true);
    expect(canManageMember(ConsoleRole.Admin, "a1", admin)).toBe(true);
    expect(canManageMember(ConsoleRole.Admin, "a1", owner)).toBe(false);
  });

  test("nobody manages their own membership", () => {
    expect(canManageMember(ConsoleRole.Admin, "a2", admin)).toBe(false);
  });

  test("a member manages nobody", () => {
    expect(canManageMember(ConsoleRole.Member, "m2", member)).toBe(false);
  });

  test("only member and admin can be granted", () => {
    expect(grantableRoles(ConsoleRole.Owner)).toEqual([
      ConsoleRole.Member,
      ConsoleRole.Admin,
    ]);
    expect(grantableRoles(ConsoleRole.Member)).toEqual([]);
  });
});

describe("route visibility", () => {
  function visibleTabs(role: ConsoleRole) {
    return ORGANIZATION_ROUTES.filter(
      (route) => !route.nested && can(role, route.permission),
    ).map((route) => route.id);
  }

  test("a member sees the read-only routes", () => {
    expect(visibleTabs(ConsoleRole.Member)).toEqual([
      RouteId.Sandboxes,
      RouteId.Devices,
      RouteId.Members,
      RouteId.Settings,
    ]);
  });

  test("an admin and the owner also see enrollment tokens and API keys", () => {
    const expected = [
      RouteId.Sandboxes,
      RouteId.Devices,
      RouteId.Members,
      RouteId.EnrollmentTokens,
      RouteId.ApiKeys,
      RouteId.Settings,
    ];
    expect(visibleTabs(ConsoleRole.Admin)).toEqual(expected);
    expect(visibleTabs(ConsoleRole.Owner)).toEqual(expected);
  });
});
