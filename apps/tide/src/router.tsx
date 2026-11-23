// SPDX-License-Identifier: Apache-2.0
import {
  createContext,
  type MouseEvent,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

/** Stable route IDs of the console contract. */
export enum RouteId {
  Organizations = "organizations",
  Invitation = "invitation",
  Callback = "callback",
  GitHubSetup = "github-setup",
  Overview = "overview",
  Requests = "requests",
  Runners = "runners",
  Profiles = "profiles",
  Integrations = "integrations",
  Members = "members",
  DevSessions = "dev-sessions",
  ApiKeys = "api-keys",
  Audit = "audit",
  Settings = "settings",
  NotFound = "not-found",
}

/** Routes below `/orgs/:organizationId/<section>`. */
export type OrgSectionId =
  | RouteId.Requests
  | RouteId.Runners
  | RouteId.Profiles
  | RouteId.Integrations
  | RouteId.Members
  | RouteId.DevSessions
  | RouteId.ApiKeys
  | RouteId.Audit
  | RouteId.Settings;

export type OrgRouteId = RouteId.Overview | OrgSectionId;

export type Route =
  | { id: RouteId.Organizations }
  | { id: RouteId.Invitation }
  | { id: RouteId.Callback }
  | { id: RouteId.GitHubSetup }
  | { id: RouteId.NotFound }
  | { id: OrgRouteId; organizationId: string };

// A Map keeps inherited Object.prototype names such as "constructor" or
// "__proto__" from resolving as sections.
const ORG_SECTIONS: ReadonlyMap<string, OrgSectionId> = new Map<
  string,
  OrgSectionId
>([
  ["requests", RouteId.Requests],
  ["runners", RouteId.Runners],
  ["profiles", RouteId.Profiles],
  ["integrations", RouteId.Integrations],
  ["members", RouteId.Members],
  ["dev-sessions", RouteId.DevSessions],
  ["api-keys", RouteId.ApiKeys],
  ["audit", RouteId.Audit],
  ["settings", RouteId.Settings],
]);

/** Parses a path into a route. */
export function parseRoute(pathname: string): Route {
  let parts: string[];
  try {
    parts = pathname
      .split("/")
      .filter((p) => p !== "")
      .map(decodeURIComponent);
  } catch {
    // decodeURIComponent throws URIError on malformed percent escapes; treat
    // such a path as an unknown route instead of crashing the app.
    return { id: RouteId.NotFound };
  }
  if (parts.length === 0) return { id: RouteId.Organizations };
  if (parts.length === 2 && parts[0] === "invitations" && parts[1] === "accept")
    return { id: RouteId.Invitation };
  if (parts.length === 2 && parts[0] === "github" && parts[1] === "setup")
    return { id: RouteId.GitHubSetup };
  if (parts.length === 1 && parts[0] === "callback")
    return { id: RouteId.Callback };
  if (parts[0] === "orgs" && parts[1] !== undefined && parts[1] !== "") {
    const organizationId = parts[1];
    if (parts.length === 2) return { id: RouteId.Overview, organizationId };
    if (parts.length === 3) {
      const section = ORG_SECTIONS.get(parts[2]!);
      if (section !== undefined) return { id: section, organizationId };
    }
  }
  return { id: RouteId.NotFound };
}

/** Builds the path of a route. */
export function routePath(route: Route): string {
  const e = encodeURIComponent;
  switch (route.id) {
    case RouteId.Organizations:
    case RouteId.NotFound:
      return "/";
    case RouteId.Invitation:
      return "/invitations/accept";
    case RouteId.Callback:
      return "/callback";
    case RouteId.GitHubSetup:
      return "/github/setup";
    case RouteId.Overview:
      return `/orgs/${e(route.organizationId)}`;
    default:
      return `/orgs/${e(route.organizationId)}/${route.id}`;
  }
}

/** The path of an organization route. */
export function orgPath(id: OrgRouteId, organizationId: string): string {
  return routePath({ id, organizationId });
}

interface RouterValue {
  route: Route;
  navigate: (path: string, options?: { replace?: boolean }) => void;
}

const RouterContext = createContext<RouterValue | undefined>(undefined);

/**
 * A small history-API router. The console has a fixed set of routes, so a
 * routing library is not needed.
 */
export function RouterProvider({ children }: { children: ReactNode }) {
  const [pathname, setPathname] = useState(() => window.location.pathname);
  useEffect(() => {
    const onPop = () => setPathname(window.location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const navigate = useCallback(
    (path: string, options: { replace?: boolean } = {}) => {
      const url = new URL(path, window.location.origin);
      const target = url.pathname + url.search + url.hash;
      if (options.replace) {
        window.history.replaceState(null, "", target);
      } else {
        window.history.pushState(null, "", target);
      }
      setPathname(url.pathname);
      window.scrollTo?.(0, 0);
    },
    [],
  );
  const value = useMemo(
    () => ({ route: parseRoute(pathname), navigate }),
    [pathname, navigate],
  );
  return (
    <RouterContext.Provider value={value}>{children}</RouterContext.Provider>
  );
}

export function useRouter(): RouterValue {
  const value = useContext(RouterContext);
  if (value === undefined) {
    throw new Error("useRouter needs a RouterProvider");
  }
  return value;
}

/** An in-console link that navigates without a page load. */
export function Link({
  to,
  children,
  className,
  "aria-current": ariaCurrent,
}: {
  to: string;
  children: ReactNode;
  className?: string;
  "aria-current"?: "page" | undefined;
}) {
  const { navigate } = useRouter();
  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    navigate(to);
  };
  return (
    <a
      href={to}
      onClick={onClick}
      className={className}
      aria-current={ariaCurrent}
    >
      {children}
    </a>
  );
}
