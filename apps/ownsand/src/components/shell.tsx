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
import { useI18n } from "../i18n.ts";
import { useOrganizations } from "../organizations.ts";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { enumText, type MessageKey } from "../text.ts";
import { BillingBanner } from "./billing.tsx";
import { LanguageSelect } from "./language-select.tsx";
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
  title: MessageKey;
  permission: Permission;
}[] = [
  {
    id: RouteId.Sandboxes,
    title: "nav.sandboxes",
    permission: Permission.UseSandboxes,
  },
  {
    id: RouteId.Snapshots,
    title: "nav.snapshots",
    permission: Permission.UseSandboxes,
  },
  {
    id: RouteId.Devices,
    title: "nav.devices",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.Members,
    title: "nav.members",
    permission: Permission.ReadOrganization,
  },
  {
    id: RouteId.EnrollmentTokens,
    title: "nav.enrollmentTokens",
    permission: Permission.ManageEnrollmentTokens,
  },
  {
    id: RouteId.ApiKeys,
    title: "nav.apiKeys",
    permission: Permission.ManageApiKeys,
  },
  {
    id: RouteId.Settings,
    title: "nav.settings",
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

/**
 * The top bar of the signed-out pages and the invitation flow: the product
 * name and the language menu. `brand` false leaves out the product name, for
 * a page whose heading already shows it.
 */
export function PublicTopBar({ brand = true }: { brand?: boolean }) {
  return (
    <header className="topbar">
      {brand ? <span className="brand">OwnSand</span> : null}
      <LanguageSelect className="topbar-language" />
    </header>
  );
}

/**
 * The top bar: product name, organization switcher, language menu and
 * account links.
 */
export function TopBar({ organizationId }: { organizationId?: string }) {
  const auth = useAuth();
  const { t } = useI18n();
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
            label={t("topBar.organization")}
            placeholder={t("topBar.chooseOrganization")}
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
                {t("topBar.organizationsFailed")}
              </SelectOption>
            ) : orgs.isIncomplete ? (
              <SelectOption value={RETRY_ORGANIZATIONS} disabled>
                {t("topBar.organizationsLoading")}
              </SelectOption>
            ) : null}
          </Select>
        </div>
      ) : null}
      <LanguageSelect className="topbar-language" />
      <nav className="topbar-links" aria-label={t("topBar.account")}>
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

/** The layout of every organization page. */
export function OrgShell({
  organizationId,
  children,
}: {
  organizationId: string;
  children: ReactNode;
}) {
  const { route } = useRouter();
  const i18n = useI18n();
  const { t } = i18n;
  const org = useQuery(OrganizationService.method.getOrganization, {
    organizationId,
  });
  let body: ReactNode;
  if (org.isPending) {
    body = <Loading label={t("orgShell.loading")} />;
  } else if (org.data === undefined || showsFullError(org)) {
    // No loaded organization, or an error that a retry cannot fix
    // (`not_found`, `permission_denied`, `unauthenticated`): replace the
    // layout. A transient failure of a refetch keeps the layout mounted, so
    // open dialogs and typed values survive; the notice below reports it.
    body = (
      <ErrorNotice error={org.error} onRetry={() => void org.refetch()}>
        <Link to="/">{t("common.goToOrganizations")}</Link>
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
          <nav className="sidenav" aria-label={t("orgShell.navigation")}>
            <p className="sidenav-org">
              {organization.name}
              <span className="muted">
                {` · ${enumText(i18n, labels.role, role)}`}
              </span>
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
                    {t(s.title)}
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
                <p>{t("orgShell.noPermission")}</p>
                <Link
                  to={routePath({
                    id: RouteId.Sandboxes,
                    organizationId,
                  })}
                >
                  {t("orgShell.goToSandboxes")}
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
