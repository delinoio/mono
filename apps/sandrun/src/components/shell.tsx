// SPDX-License-Identifier: Apache-2.0
import { useQuery } from "@connectrpc/connect-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import {
  OwnSandLinkState,
  type Role,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  type Organization,
  OrganizationService,
} from "@delino/sandrun-protos/v1/organization_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import { createContext, type ReactNode, useContext } from "react";
import { useAuth } from "../auth.tsx";
import { ACCOUNTS_URL, OWNSAND_CONSOLE_URL } from "../config.ts";
import { labels } from "../format.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import {
  Link,
  type OrgRouteId,
  orgPath,
  RouteId,
  useRouter,
} from "../router.tsx";
import {
  ErrorNotice,
  Loading,
  RefreshFailedNotice,
  showsFullError,
} from "./ui.tsx";

interface OrgContextValue {
  organizationId: string;
  name: string;
  role: Role;
  organization: Organization;
  /** The Logto user ID of the signed-in user. */
  userId: string | undefined;
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
  id: OrgRouteId;
  title: string;
  permission: Permission;
}[] = [
  {
    id: RouteId.Overview,
    title: "Overview",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Requests,
    title: "Requests",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Runners,
    title: "Runners",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Profiles,
    title: "Runner profiles",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Integrations,
    title: "Integrations",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.DevSessions,
    title: "Dev sessions",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Members,
    title: "Members",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.ApiKeys,
    title: "API keys",
    permission: Permission.ManageOwnApiKeys,
  },
  {
    id: RouteId.Audit,
    title: "Audit events",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Settings,
    title: "Settings",
    permission: Permission.ChangeSettings,
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

/** The top bar: product name, organization switcher and account links. */
export function TopBar({ organizationId }: { organizationId?: string }) {
  const auth = useAuth();
  const { route, navigate } = useRouter();
  const orgs = useOrganizations(organizationId !== undefined);
  const section: OrgRouteId =
    "organizationId" in route ? route.id : RouteId.Overview;
  return (
    <header className="topbar">
      <Link to="/" className="brand">
        SandRun
      </Link>
      {organizationId !== undefined && orgs.data !== undefined ? (
        <div className="org-switcher">
          <Select
            label="Organization"
            value={organizationId}
            onValueChange={(value) => {
              if (value === null) return;
              navigate(orgPath(section, value));
            }}
          >
            {orgs.data.map((org) => (
              <SelectOption key={org.organizationId} value={org.organizationId}>
                {org.name}
              </SelectOption>
            ))}
          </Select>
        </div>
      ) : null}
      <nav className="topbar-links" aria-label="Account">
        <a href={OWNSAND_CONSOLE_URL} target="_blank" rel="noreferrer">
          OwnSand console
        </a>
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

/**
 * The banner of every organization route while the OwnSand link is
 * `INVALID`: OwnSand rejected the stored API key and dispatch has stopped.
 */
export function InvalidLinkBanner({
  organizationId,
}: {
  organizationId: string;
}) {
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });
  if (link.data?.link?.state !== OwnSandLinkState.INVALID) return null;
  return (
    <section className="link-banner" role="alert" aria-label="OwnSand link">
      <h2>Dispatch has stopped</h2>
      <p>
        OwnSand rejected the API key of the OwnSand link, so SandRun starts no
        runners. Queued requests wait until their maximum queue wait ends.
        Replace the API key to start dispatch again.
      </p>
      <div className="link-banner-actions">
        <Link to={orgPath(RouteId.Integrations, organizationId)}>
          Go to integrations
        </Link>
      </div>
    </section>
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
  const auth = useAuth();
  const org = useQuery(OrganizationService.method.getOrganization, {
    organizationId,
  });
  let body: ReactNode;
  if (showsFullError(org)) {
    body = (
      <main className="content">
        <ErrorNotice error={org.error} onRetry={() => void org.refetch()} />
      </main>
    );
  } else if (org.data === undefined) {
    body = <Loading label="Loading the organization" />;
  } else {
    const organization = org.data.organization!;
    const role = organization.callerRole;
    body = (
      <OrgContext.Provider
        value={{
          organizationId,
          name: organization.name,
          role,
          organization,
          userId: auth.userId,
        }}
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
                    to={orgPath(s.id, organizationId)}
                    aria-current={route.id === s.id ? "page" : undefined}
                  >
                    {s.title}
                  </Link>
                </li>
              ))}
            </ul>
          </nav>
          <main className="content">
            {org.error ? <RefreshFailedNotice query={org} /> : null}
            <InvalidLinkBanner organizationId={organizationId} />
            {canOpenSection(role, route.id) ? (
              children
            ) : (
              <div className="notice notice-error" role="alert">
                <p>You do not have permission to open this page.</p>
                <Link to={orgPath(RouteId.Overview, organizationId)}>
                  Go to the overview
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
