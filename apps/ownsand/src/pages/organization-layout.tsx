// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { OrganizationService } from "@delino/ownsand-sdk/gen/organization_pb";
import type { ReactNode } from "react";
import { ErrorState, LoadingState } from "../components/common.tsx";
import { can, fromProtoRole, roleLabel, type ConsoleRole } from "../roles.ts";
import { Link } from "../router.tsx";
import {
  ORGANIZATION_ROUTES,
  RouteId,
  organizationPath,
  routePermission,
  type OrganizationRouteId,
} from "../routes.ts";

/** What every organization page receives. */
export interface OrganizationPageProps {
  readonly organizationId: string;
  readonly organizationName: string;
  readonly role: ConsoleRole | undefined;
}

export function OrganizationLayout({
  organizationId,
  routeId,
  render,
}: {
  readonly organizationId: string;
  readonly routeId: OrganizationRouteId;
  readonly render: (props: OrganizationPageProps) => ReactNode;
}) {
  const query = useQuery(OrganizationService.method.getOrganization, {
    organizationId,
  });
  if (query.isPending) {
    return (
      <main className="page">
        <LoadingState />
      </main>
    );
  }
  if (query.isError || query.data.organization === undefined) {
    return (
      <main className="page">
        <ErrorState error={query.error} />
        <Link className="text-link" to="/">
          Go to your organizations
        </Link>
      </main>
    );
  }
  const organization = query.data.organization;
  const role = fromProtoRole(organization.callerRole);
  const permission = routePermission(routeId);
  const allowed = permission !== undefined && can(role, permission);
  // The device detail page belongs to the Devices tab.
  const activeTab = routeId === RouteId.Device ? RouteId.Devices : routeId;

  return (
    <div className="organization">
      <div className="organization-bar">
        <div className="organization-title">
          <Link className="text-link muted" to="/">
            Organizations
          </Link>
          <span aria-hidden="true" className="muted">
            /
          </span>
          <span className="organization-name">{organization.name}</span>
          <span className="role-chip">{roleLabel(role)}</span>
        </div>
        <nav aria-label="Organization">
          <ul className="tabs">
            {ORGANIZATION_ROUTES.filter(
              (route) => !route.nested && can(role, route.permission),
            ).map((route) => (
              <li key={route.id}>
                <Link
                  className="tab"
                  to={organizationPath(organizationId, route.id)}
                  aria-current={route.id === activeTab ? "page" : undefined}
                >
                  {route.label}
                </Link>
              </li>
            ))}
          </ul>
        </nav>
      </div>
      {allowed ? (
        render({ organizationId, organizationName: organization.name, role })
      ) : (
        <main className="page">
          <p role="alert" className="form-error">
            Your role does not allow access to this page.
          </p>
        </main>
      )}
    </div>
  );
}
