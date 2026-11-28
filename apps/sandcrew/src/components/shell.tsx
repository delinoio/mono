// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { Role } from "@delino/sandcrew-protos/v1/common_pb";
import {
  type Organization,
  OrganizationService,
} from "@delino/sandcrew-protos/v1/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
} from "react";
import { invalidateServices, useAllPages } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { ACCOUNTS_URL, SANDRUN_CONSOLE_URL } from "../config.ts";
import { labels } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import {
  Link,
  type OrgRouteId,
  orgPath,
  RouteId,
  useRouter,
} from "../router.tsx";
import { writeSelectedOrganization } from "../storage.ts";
import { LanguageSelect } from "./language-select.tsx";
import {
  ErrorNotice,
  Loading,
  RefreshFailedNotice,
  showsFullError,
} from "./ui.tsx";

/** The query string that tells the picker why the console returned to it. */
export const LEFT_ORGANIZATION_PARAM = "left";

/** Whether a role changes SandCrew organization settings. */
export function canManageOrganization(role: Role | undefined): boolean {
  return role === Role.OWNER || role === Role.ADMIN;
}

/** Every organization of the caller, from all pages. */
export function useOrganizations(options: { enabled?: boolean } = {}) {
  return useAllPages(
    OrganizationService.method.listOrganizations,
    {},
    (r) => ({ items: r.organizations, nextPageToken: r.nextPageToken }),
    options,
  );
}

interface OrgContextValue {
  organizationId: string;
  organization: Organization;
  role: Role;
  /** The Logto user ID of the signed-in user. */
  userId: string | undefined;
  /**
   * Returns to the organization picker when an error says that the caller
   * no longer belongs to the organization. Returns true when it did.
   */
  leaveOnNotFound: (error: unknown) => boolean;
}

const OrgContext = createContext<OrgContextValue | undefined>(undefined);

/** The organization of the current route and the caller's role in it. */
export function useOrg(): OrgContextValue {
  const value = useContext(OrgContext);
  if (value === undefined)
    throw new Error("useOrg needs an organization route");
  return value;
}

/** Provides an organization context; tests use it to render one page. */
export function OrgContextProvider({
  value,
  children,
}: {
  value: OrgContextValue;
  children: ReactNode;
}) {
  return <OrgContext.Provider value={value}>{children}</OrgContext.Provider>;
}

const SECTIONS: {
  id: OrgRouteId;
  title: MessageKey;
  /** Also current for these routes. */
  covers: RouteId[];
  managersOnly: boolean;
}[] = [
  {
    id: RouteId.SessionList,
    title: "nav.sessions",
    covers: [RouteId.SessionChat],
    managersOnly: false,
  },
  {
    id: RouteId.SessionNew,
    title: "nav.newSession",
    covers: [],
    managersOnly: false,
  },
  {
    id: RouteId.SettingsUser,
    title: "nav.settings",
    covers: [],
    managersOnly: false,
  },
  {
    id: RouteId.SettingsOrg,
    title: "nav.organizationSettings",
    covers: [],
    managersOnly: true,
  },
];

/** The sections of the navigation that a role sees. */
export function visibleSections(role: Role | undefined) {
  return SECTIONS.filter((s) => !s.managersOnly || canManageOrganization(role));
}

/** The top bar: product name, organization switcher and account links. */
export function TopBar({ organizationId }: { organizationId?: string }) {
  const auth = useAuth();
  const { t } = useI18n();
  const { route, navigate } = useRouter();
  const orgs = useOrganizations({ enabled: organizationId !== undefined });
  const section: OrgRouteId =
    route.id === RouteId.SessionList ||
    route.id === RouteId.SessionNew ||
    route.id === RouteId.SettingsUser ||
    route.id === RouteId.SettingsOrg
      ? route.id
      : RouteId.SessionList;
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
            label={t("shell.organization")}
            value={organizationId}
            onValueChange={(value) => {
              if (value === null || value === organizationId) return;
              const next = orgs.data.find((o) => o.organizationId === value);
              // Members cannot open the organization settings of another
              // organization, so they land on its sessions.
              const target =
                section === RouteId.SettingsOrg &&
                !canManageOrganization(next?.callerRole)
                  ? RouteId.SessionList
                  : section;
              navigate(orgPath(target, value));
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
        SandCrew
      </Link>
      {switcher ? <div className="org-switcher">{switcher}</div> : null}
      <nav className="topbar-links" aria-label={t("shell.account")}>
        <LanguageSelect />
        <a href={ACCOUNTS_URL} target="_blank" rel="noopener noreferrer">
          {t("shell.accountSettings")}
        </a>
        <Button
          variant={ButtonVariant.Secondary}
          size={ButtonSize.Small}
          onClick={() => auth.signOut()}
        >
          {t("shell.signOut")}
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
  const { route, navigate } = useRouter();
  const auth = useAuth();
  const { t, rich } = useI18n();
  const queryClient = useQueryClient();
  const orgs = useOrganizations();
  const organization = orgs.data?.find(
    (o) => o.organizationId === organizationId,
  );
  const missing = orgs.data !== undefined && organization === undefined;

  const leave = useCallback(() => {
    writeSelectedOrganization(undefined);
    void invalidateServices(queryClient, OrganizationService);
    navigate(`/?${LEFT_ORGANIZATION_PARAM}=1`, { replace: true });
  }, [navigate, queryClient]);
  const leaveOnNotFound = useCallback(
    (error: unknown) => {
      if (error instanceof ConnectError && error.code === Code.NotFound) {
        leave();
        return true;
      }
      return false;
    },
    [leave],
  );

  useEffect(() => {
    if (missing) leave();
  }, [missing, leave]);
  useEffect(() => {
    if (organization !== undefined) writeSelectedOrganization(organizationId);
  }, [organization, organizationId]);

  const value = useMemo(
    () =>
      organization === undefined
        ? undefined
        : {
            organizationId,
            organization,
            role: organization.callerRole,
            userId: auth.userId,
            leaveOnNotFound,
          },
    [organization, organizationId, auth.userId, leaveOnNotFound],
  );

  let body: ReactNode;
  if (showsFullError(orgs)) {
    body = (
      <main className="content content-narrow">
        <ErrorNotice error={orgs.error} onRetry={() => void orgs.refetch()} />
      </main>
    );
  } else if (value === undefined) {
    body = <Loading label={t("shell.loadingOrganization")} />;
  } else {
    const allowed =
      route.id !== RouteId.SettingsOrg || canManageOrganization(value.role);
    body = (
      <OrgContext.Provider value={value}>
        <div className="org-layout">
          <nav className="sidenav" aria-label={t("shell.organization")}>
            <p className="sidenav-org">
              {value.organization.name}
              <span className="muted"> · {t(labels.role[value.role])}</span>
            </p>
            <ul>
              {visibleSections(value.role).map((s) => {
                const current =
                  route.id === s.id || s.covers.includes(route.id);
                return (
                  <li key={s.id}>
                    <Link
                      to={orgPath(s.id, organizationId)}
                      aria-current={current ? "page" : undefined}
                    >
                      {t(s.title)}
                    </Link>
                  </li>
                );
              })}
            </ul>
            <p className="sidenav-footer muted">
              {rich("shell.managedInSandRun", {
                link: (
                  <a
                    href={SANDRUN_CONSOLE_URL}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    {t("shell.sandrunConsole")}
                  </a>
                ),
              })}
            </p>
          </nav>
          <main className="content">
            {orgs.error ? <RefreshFailedNotice query={orgs} /> : null}
            {allowed ? (
              children
            ) : (
              <div className="notice notice-error" role="alert">
                <p>{t("shell.managersOnly")}</p>
                <Link to={orgPath(RouteId.SessionList, organizationId)}>
                  {t("shell.goToSessions")}
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
