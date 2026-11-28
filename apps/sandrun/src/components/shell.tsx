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
import { type MessageKey, useI18n } from "../i18n.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import {
  Link,
  type OrgRouteId,
  orgPath,
  RouteId,
  useRouter,
} from "../router.tsx";
import { LanguageSelect } from "./language-select.tsx";
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
  title: MessageKey;
  permission: Permission;
}[] = [
  {
    id: RouteId.Overview,
    title: "nav.overview",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Requests,
    title: "nav.requests",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Runners,
    title: "nav.runners",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Profiles,
    title: "nav.profiles",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Integrations,
    title: "nav.integrations",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.DevSessions,
    title: "nav.devSessions",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Members,
    title: "nav.members",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.ApiKeys,
    title: "nav.apiKeys",
    permission: Permission.ManageOwnApiKeys,
  },
  {
    id: RouteId.Audit,
    title: "nav.audit",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Settings,
    title: "nav.settings",
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
  const { t } = useI18n();
  const { route, navigate } = useRouter();
  const orgs = useOrganizations(organizationId !== undefined);
  const section: OrgRouteId =
    "organizationId" in route ? route.id : RouteId.Overview;
  let switcher: ReactNode;
  if (organizationId !== undefined) {
    if (showsFullError(orgs)) {
      switcher = (
        <ErrorNotice error={orgs.error} onRetry={() => void orgs.refetch()} />
      );
    } else if (orgs.data !== undefined) {
      switcher = (
        <>
          {orgs.error ? <RefreshFailedNotice query={orgs} /> : null}
          <Select
            label={t("topBar.organization")}
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
        </>
      );
    }
  }

  return (
    <header className="topbar">
      <Link to="/" className="brand">
        SandRun
      </Link>
      {switcher ? <div className="org-switcher">{switcher}</div> : null}
      <nav className="topbar-links" aria-label={t("topBar.account")}>
        <LanguageSelect />
        <a href={OWNSAND_CONSOLE_URL} target="_blank" rel="noreferrer">
          {t("topBar.ownSandConsole")}
        </a>
        <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
          {t("topBar.accountSettings")}
        </a>
        <Button
          variant={ButtonVariant.Secondary}
          size={ButtonSize.Small}
          onClick={() => auth.signOut()}
        >
          {t("topBar.signOut")}
        </Button>
      </nav>
    </header>
  );
}

/**
 * The top bar of the pages before sign-in: only the language menu, so the
 * signed-out pages can switch the language too.
 */
export function SignedOutTopBar() {
  return (
    <header className="topbar topbar-signed-out">
      <div className="topbar-links">
        <LanguageSelect />
      </div>
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
  const { t } = useI18n();
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });
  if (link.data?.link?.state !== OwnSandLinkState.INVALID) return null;
  return (
    <section
      className="link-banner"
      role="alert"
      aria-label={t("linkBanner.label")}
    >
      <h2>{t("linkBanner.title")}</h2>
      <p>{t("linkBanner.description")}</p>
      <div className="link-banner-actions">
        <Link to={orgPath(RouteId.Integrations, organizationId)}>
          {t("common.goToIntegrations")}
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
  const { t } = useI18n();
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
    body = <Loading label={t("orgShell.loading")} />;
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
          <nav className="sidenav" aria-label={t("orgShell.navigation")}>
            <p className="sidenav-org">
              {organization.name}
              <span className="muted">
                {" "}
                {t("orgShell.role", { role: t(labels.role[role]) })}
              </span>
            </p>
            <ul>
              {visibleSections(role).map((s) => (
                <li key={s.id}>
                  <Link
                    to={orgPath(s.id, organizationId)}
                    aria-current={route.id === s.id ? "page" : undefined}
                  >
                    {t(s.title)}
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
                <p>{t("orgShell.noPermission")}</p>
                <Link to={orgPath(RouteId.Overview, organizationId)}>
                  {t("orgShell.goToOverview")}
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
