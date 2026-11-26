// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import type { Role } from "@delino/ownsand-protos/v1/common_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { createContext, type ReactNode, useContext } from "react";
import { useAuth } from "../auth.tsx";
import { ACCOUNTS_URL } from "../config.ts";
import { labels } from "../format.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { BillingBanner } from "./billing.tsx";
import {
  ErrorNotice,
  isTerminalError,
  Loading,
  RefreshFailedNotice,
  showsFullError,
} from "./ui.tsx";

interface OrgContextValue {
  organizationId: string;
  name: string;
  role: Role;
}

const OrgContext = createContext<OrgContextValue | undefined>(undefined);

/** The organization of the current route and the caller's role in it. */
export function useOrg(): OrgContextValue {
  const value = useContext(OrgContext);
  if (value === undefined)
    throw new Error("useOrg needs an organization route");
  return value;
}

const SECTIONS: {
  id: RouteId;
  title: string;
  permission: Permission;
}[] = [
  {
    id: RouteId.Sandboxes,
    title: "Sandboxes",
    permission: Permission.UseSandboxes,
  },
  {
    id: RouteId.Snapshots,
    title: "Snapshots",
    permission: Permission.UseSandboxes,
  },
  {
    id: RouteId.Devices,
    title: "Devices",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Members,
    title: "Members",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.EnrollmentTokens,
    title: "Enrollment tokens",
    permission: Permission.ManageEnrollmentTokens,
  },
  {
    id: RouteId.ApiKeys,
    title: "API keys",
    permission: Permission.ManageApiKeys,
  },
  {
    id: RouteId.Settings,
    title: "Settings",
    permission: Permission.ReadOrganization,
  },
];

/** Sections of the navigation that a role may see. */
export function visibleSections(role: Role | undefined) {
  return SECTIONS.filter((s) => can(role, s.permission));
}

/**
 * Whether a role may open a section. The organization switcher keeps the
 * current section and a bookmark can name any section, so the shell checks
 * the role of the destination organization, not only the navigation.
 */
export function canOpenSection(role: Role | undefined, id: RouteId): boolean {
  const section = SECTIONS.find((s) => s.id === id);
  return section === undefined || can(role, section.permission);
}

/**
 * The value of the switcher entry that reports an incomplete organization
 * list. It is not an organization ID, so it never navigates.
 */
const RETRY_ORGANIZATIONS = "retry-organizations";

/** The top bar: product name, organization switcher and account links. */
export function TopBar({ organizationId }: { organizationId?: string }) {
  const auth = useAuth();
  const { route, navigate } = useRouter();
  const orgs = useOrganizations();
  const normalizedOrganizationId = organizationId?.toLowerCase();
  const selectedOrganization =
    normalizedOrganizationId === undefined
      ? undefined
      : orgs.organizations.find(
          (org) =>
            org.organizationId.toLowerCase() === normalizedOrganizationId,
        );
  const section =
    "organizationId" in route &&
    route.id !== RouteId.Sandbox &&
    route.id !== RouteId.Device
      ? route.id
      : route.id === RouteId.Sandbox
        ? RouteId.Sandboxes
        : RouteId.Devices;
  return (
    <header className="topbar">
      <Link to="/" className="brand">
        OwnSand
      </Link>
      {organizationId !== undefined &&
      orgs.isLoaded &&
      !isTerminalError(orgs.error) &&
      orgs.terminalLoadMoreError === null ? (
        <div className="org-switcher">
          <Select
            label="Organization"
            placeholder="Choose an organization"
            value={
              selectedOrganization?.organizationId ??
              // A later page can still contain the route organization.
              (orgs.isIncomplete ? organizationId : null)
            }
            onValueChange={(value) => {
              if (value === null) return;
              if (value === RETRY_ORGANIZATIONS) {
                orgs.retryLoadMore();
                return;
              }
              navigate(
                routePath({ id: section, organizationId: value } as Parameters<
                  typeof routePath
                >[0]),
              );
            }}
          >
            {orgs.organizations.map((org) => (
              <SelectOption key={org.organizationId} value={org.organizationId}>
                {org.name}
              </SelectOption>
            ))}
            {orgs.loadMoreError ? (
              <SelectOption value={RETRY_ORGANIZATIONS}>
                Some organizations did not load. Try again
              </SelectOption>
            ) : orgs.isIncomplete ? (
              <SelectOption value={RETRY_ORGANIZATIONS} disabled>
                Loading more organizations
              </SelectOption>
            ) : null}
          </Select>
        </div>
      ) : null}
      <nav className="topbar-links" aria-label="Account">
        <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
          Account settings
        </a>
        <Button
          variant={ButtonVariant.Secondary}
          size={ButtonSize.Small}
          onClick={() => auth.signOut()}
        >
          Sign out
        </Button>
      </nav>
    </header>
  );
}

/** The layout of every organization page. */
export function OrgShell({
  organizationId,
  children,
}: {
  organizationId: string;
  children: ReactNode;
}) {
  const { route } = useRouter();
  const org = useQuery(OrganizationService.method.getOrganization, {
    organizationId,
  });
  let body: ReactNode;
  if (org.isPending) {
    body = <Loading label="Loading the organization" />;
  } else if (org.data === undefined || showsFullError(org)) {
    // No loaded organization, or an error that a retry cannot fix
    // (`not_found`, `permission_denied`, `unauthenticated`): replace the
    // layout. A transient failure of a refetch keeps the layout mounted, so
    // open dialogs and typed values survive; the notice below reports it.
    body = (
      <ErrorNotice error={org.error} onRetry={() => void org.refetch()}>
        <Link to="/">Go to your organizations</Link>
      </ErrorNotice>
    );
  } else {
    const organization = org.data.organization!;
    const role = organization.callerRole;
    const current =
      route.id === RouteId.Sandbox
        ? RouteId.Sandboxes
        : route.id === RouteId.Device
          ? RouteId.Devices
          : route.id;
    body = (
      <OrgContext.Provider
        value={{ organizationId, name: organization.name, role }}
      >
        <div className="org-layout">
          <nav className="sidenav" aria-label="Organization">
            <p className="sidenav-org">
              {organization.name}
              <span className="muted"> · {labels.role[role]}</span>
            </p>
            <ul>
              {visibleSections(role).map((s) => (
                <li key={s.id}>
                  <Link
                    to={routePath({ id: s.id, organizationId } as Parameters<
                      typeof routePath
                    >[0])}
                    aria-current={current === s.id ? "page" : undefined}
                  >
                    {s.title}
                  </Link>
                </li>
              ))}
            </ul>
          </nav>
          <main className="content">
            {org.error ? <RefreshFailedNotice query={org} /> : null}
            <BillingBanner organizationId={organizationId} role={role} />
            {canOpenSection(role, current) ? (
              children
            ) : (
              <div className="notice notice-error" role="alert">
                <p>You do not have permission to open this page.</p>
                <Link
                  to={routePath({
                    id: RouteId.Sandboxes,
                    organizationId,
                  })}
                >
                  Go to sandboxes
                </Link>
              </div>
            )}
          </main>
        </div>
      </OrgContext.Provider>
    );
  }
  return (
    <>
      <TopBar organizationId={organizationId} />
      {body}
    </>
  );
}
