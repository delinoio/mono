// SPDX-License-Identifier: Apache-2.0
import { Permission } from "./roles.ts";

/** Stable route identifiers of the console route table. */
export enum RouteId {
  Organizations = "organizations",
  Invitation = "invitation",
  SignInCallback = "sign-in-callback",
  Sandboxes = "sandboxes",
  Devices = "devices",
  Device = "device",
  Members = "members",
  EnrollmentTokens = "enrollment-tokens",
  ApiKeys = "api-keys",
  Settings = "settings",
  NotFound = "not-found",
}

/** A matched route with its path parameters. */
export type RouteMatch =
  | { readonly id: RouteId.Organizations }
  | { readonly id: RouteId.Invitation }
  | { readonly id: RouteId.SignInCallback }
  | { readonly id: RouteId.NotFound }
  | {
      readonly id: OrganizationRouteId;
      readonly organizationId: string;
      readonly deviceId?: string;
    };

/** Routes below `/orgs/:organizationId/`. */
export type OrganizationRouteId =
  | RouteId.Sandboxes
  | RouteId.Devices
  | RouteId.Device
  | RouteId.Members
  | RouteId.EnrollmentTokens
  | RouteId.ApiKeys
  | RouteId.Settings;

/** Organization routes in navigation order, with the permission that shows them. */
export const ORGANIZATION_ROUTES: readonly {
  readonly id: OrganizationRouteId;
  readonly segment: string;
  readonly label: string;
  readonly permission: Permission;
  /** Hidden from the navigation bar (reached through another page). */
  readonly nested?: boolean;
}[] = [
  {
    id: RouteId.Sandboxes,
    segment: "sandboxes",
    label: "Sandboxes",
    permission: Permission.ViewSandboxes,
  },
  {
    id: RouteId.Devices,
    segment: "devices",
    label: "Devices",
    permission: Permission.ViewDevices,
  },
  {
    id: RouteId.Device,
    segment: "devices",
    label: "Device",
    permission: Permission.ViewDevices,
    nested: true,
  },
  {
    id: RouteId.Members,
    segment: "members",
    label: "Members",
    permission: Permission.ViewMembers,
  },
  {
    id: RouteId.EnrollmentTokens,
    segment: "enrollment-tokens",
    label: "Enrollment tokens",
    permission: Permission.ManageEnrollmentTokens,
  },
  {
    id: RouteId.ApiKeys,
    segment: "api-keys",
    label: "API keys",
    permission: Permission.ManageApiKeys,
  },
  {
    id: RouteId.Settings,
    segment: "settings",
    label: "Settings",
    permission: Permission.ViewAgentVersionPolicy,
  },
];

/** Returns the permission that a route needs, or `undefined` for routes outside an organization. */
export function routePermission(id: RouteId): Permission | undefined {
  return ORGANIZATION_ROUTES.find((route) => route.id === id)?.permission;
}

function decodeSegment(segment: string): string | undefined {
  try {
    const value = decodeURIComponent(segment);
    return value === "" || value.includes("/") ? undefined : value;
  } catch {
    return undefined;
  }
}

/** Matches a pathname against the route table. */
export function matchRoute(pathname: string): RouteMatch {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  if (path === "/") {
    return { id: RouteId.Organizations };
  }
  if (path === "/invitations/accept") {
    return { id: RouteId.Invitation };
  }
  if (path === "/callback") {
    return { id: RouteId.SignInCallback };
  }
  const parts = path.split("/").slice(1);
  if (parts[0] !== "orgs" || parts.length < 3) {
    return { id: RouteId.NotFound };
  }
  const organizationId = decodeSegment(parts[1] ?? "");
  if (organizationId === undefined) {
    return { id: RouteId.NotFound };
  }
  if (parts.length === 3) {
    const route = ORGANIZATION_ROUTES.find(
      (candidate) => !candidate.nested && candidate.segment === parts[2],
    );
    return route ? { id: route.id, organizationId } : { id: RouteId.NotFound };
  }
  if (parts.length === 4 && parts[2] === "devices") {
    const deviceId = decodeSegment(parts[3] ?? "");
    return deviceId === undefined
      ? { id: RouteId.NotFound }
      : { id: RouteId.Device, organizationId, deviceId };
  }
  return { id: RouteId.NotFound };
}

/** Builds the path of an organization route. */
export function organizationPath(
  organizationId: string,
  id: OrganizationRouteId,
  deviceId?: string,
): string {
  const base = `/orgs/${encodeURIComponent(organizationId)}`;
  if (id === RouteId.Device) {
    return `${base}/devices/${encodeURIComponent(deviceId ?? "")}`;
  }
  const route = ORGANIZATION_ROUTES.find((candidate) => candidate.id === id);
  return `${base}/${route?.segment ?? ""}`;
}
