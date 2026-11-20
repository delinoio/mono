// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { createRouterTransport } from "@connectrpc/connect";
import { Role } from "@delino/ownsand-sdk/gen/common_pb";
import { OrganizationService } from "@delino/ownsand-sdk/gen/organization_pb";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { OrganizationLayout } from "./pages/organization-layout.tsx";
import { RouteId, type OrganizationRouteId } from "./routes.ts";
import { TestProviders, fakeAuth } from "./test-support.tsx";

afterEach(() => {
  cleanup();
});

function transportFor(role: Role) {
  return createRouterTransport(({ service }) => {
    service(OrganizationService, {
      getOrganization: (request) => ({
        organization: {
          organizationId: request.organizationId,
          name: "Acme",
          callerRole: role,
        },
      }),
    });
  });
}

async function renderLayout(role: Role, routeId: OrganizationRouteId) {
  await act(async () => {
    render(
      <TestProviders transport={transportFor(role)} auth={fakeAuth()}>
        <OrganizationLayout
          organizationId="o1"
          routeId={routeId}
          render={() => <p>page content</p>}
        />
      </TestProviders>,
    );
  });
  // Let React Query resolve the organization.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function tabNames(): string[] {
  const nav = screen.getByRole("navigation", { name: "Organization" });
  return within(nav)
    .getAllByRole("link")
    .map((link) => link.textContent ?? "");
}

describe("OrganizationLayout role gating", () => {
  test("hides admin routes from a member", async () => {
    await renderLayout(Role.MEMBER, RouteId.Sandboxes);
    expect(tabNames()).toEqual(["Sandboxes", "Devices", "Members", "Settings"]);
    expect(screen.getByText("page content")).toBeTruthy();
  });

  test("shows enrollment tokens and API keys to an admin", async () => {
    await renderLayout(Role.ADMIN, RouteId.Sandboxes);
    expect(tabNames()).toEqual([
      "Sandboxes",
      "Devices",
      "Members",
      "Enrollment tokens",
      "API keys",
      "Settings",
    ]);
  });

  test("does not render an admin page for a member who opens its URL", async () => {
    await renderLayout(Role.MEMBER, RouteId.ApiKeys);
    expect(screen.queryByText("page content")).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain(
      "Your role does not allow access",
    );
  });

  test("marks the current tab", async () => {
    await renderLayout(Role.OWNER, RouteId.Device);
    const current = screen.getByRole("link", { current: "page" });
    expect(current.textContent).toBe("Devices");
  });
});
