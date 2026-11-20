// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { RouteId, matchRoute, organizationPath } from "./routes.ts";

describe("matchRoute", () => {
  test.each([
    ["/", { id: RouteId.Organizations }],
    ["/invitations/accept", { id: RouteId.Invitation }],
    ["/invitations/accept/", { id: RouteId.Invitation }],
    ["/callback", { id: RouteId.SignInCallback }],
    ["/orgs/o1/sandboxes", { id: RouteId.Sandboxes, organizationId: "o1" }],
    ["/orgs/o1/devices", { id: RouteId.Devices, organizationId: "o1" }],
    [
      "/orgs/o1/devices/d1",
      { id: RouteId.Device, organizationId: "o1", deviceId: "d1" },
    ],
    ["/orgs/o1/members", { id: RouteId.Members, organizationId: "o1" }],
    [
      "/orgs/o1/enrollment-tokens",
      { id: RouteId.EnrollmentTokens, organizationId: "o1" },
    ],
    ["/orgs/o1/api-keys", { id: RouteId.ApiKeys, organizationId: "o1" }],
    ["/orgs/o1/settings", { id: RouteId.Settings, organizationId: "o1" }],
  ])("matches %s", (path, expected) => {
    expect(matchRoute(path)).toEqual(expected);
  });

  test.each([
    "/orgs",
    "/orgs/o1",
    "/orgs/o1/unknown",
    "/orgs/o1/settings/x",
    "/orgs/%E0%A4%A/sandboxes",
    "/orgs/a%2Fb/sandboxes",
    "/other",
  ])("does not match %s", (path) => {
    expect(matchRoute(path)).toEqual({ id: RouteId.NotFound });
  });
});

describe("organizationPath", () => {
  test("round-trips through matchRoute", () => {
    expect(matchRoute(organizationPath("o 1", RouteId.ApiKeys))).toEqual({
      id: RouteId.ApiKeys,
      organizationId: "o 1",
    });
    expect(matchRoute(organizationPath("o1", RouteId.Device, "d1"))).toEqual({
      id: RouteId.Device,
      organizationId: "o1",
      deviceId: "d1",
    });
  });
});
