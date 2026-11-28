// SPDX-License-Identifier: Apache-2.0
import {
  createContext,
  type MouseEvent,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

/** Stable route IDs of the console contract. */
export enum RouteId {
  OrganizationPick = "ORGANIZATION_PICK",
  SessionList = "SESSION_LIST",
  SessionNew = "SESSION_NEW",
  SessionChat = "SESSION_CHAT",
  SettingsUser = "SETTINGS_USER",
  SettingsOrg = "SETTINGS_ORG",
  SignInCallback = "SIGN_IN_CALLBACK",
  NotFound = "NOT_FOUND",
}

/** Routes below `/orgs/:organizationId/`. */
export type OrgRouteId =
  | RouteId.SessionList
  | RouteId.SessionNew
  | RouteId.SettingsUser
  | RouteId.SettingsOrg;

export type Route =
  | { id: RouteId.OrganizationPick }
  | { id: RouteId.SignInCallback }
  | { id: RouteId.NotFound }
  | { id: OrgRouteId; organizationId: string }
  | { id: RouteId.SessionChat; organizationId: string; sessionId: string };

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
  if (parts.length === 0) return { id: RouteId.OrganizationPick };
  if (parts.length === 1 && parts[0] === "callback")
    return { id: RouteId.SignInCallback };
  const [orgs, organizationId, section, item, extra] = parts;
  if (
    orgs !== "orgs" ||
    organizationId === undefined ||
    organizationId === "" ||
    extra !== undefined
  ) {
    return { id: RouteId.NotFound };
  }
  if (section === "sessions") {
    if (item === undefined) return { id: RouteId.SessionList, organizationId };
    if (item === "new") return { id: RouteId.SessionNew, organizationId };
    return { id: RouteId.SessionChat, organizationId, sessionId: item };
  }
  if (section === "settings") {
    if (item === undefined) return { id: RouteId.SettingsUser, organizationId };
    if (item === "organization")
      return { id: RouteId.SettingsOrg, organizationId };
  }
  return { id: RouteId.NotFound };
}

/** Builds the path of a route. */
export function routePath(route: Route): string {
  const e = encodeURIComponent;
  switch (route.id) {
    case RouteId.OrganizationPick:
    case RouteId.NotFound:
      return "/";
    case RouteId.SignInCallback:
      return "/callback";
    case RouteId.SessionList:
      return `/orgs/${e(route.organizationId)}/sessions`;
    case RouteId.SessionNew:
      return `/orgs/${e(route.organizationId)}/sessions/new`;
    case RouteId.SessionChat:
      return `/orgs/${e(route.organizationId)}/sessions/${e(route.sessionId)}`;
    case RouteId.SettingsUser:
      return `/orgs/${e(route.organizationId)}/settings`;
    case RouteId.SettingsOrg:
      return `/orgs/${e(route.organizationId)}/settings/organization`;
  }
}

/** The path of an organization route. */
export function orgPath(id: OrgRouteId, organizationId: string): string {
  return routePath({ id, organizationId });
}

/** The path of a session's chat view. */
export function sessionPath(organizationId: string, sessionId: string): string {
  return routePath({ id: RouteId.SessionChat, organizationId, sessionId });
}

interface RouterValue {
  route: Route;
  navigate: (path: string, options?: { replace?: boolean }) => void;
  isCurrentVisit: () => boolean;
}

const RouterContext = createContext<RouterValue | undefined>(undefined);

/**
 * A small history-API router. The console has a fixed set of routes, so a
 * routing library is not needed.
 */
export function RouterProvider({ children }: { children: ReactNode }) {
  const [visit, setVisit] = useState(() => ({
    pathname: window.location.pathname,
  }));
  const currentVisit = useRef(visit);
  const updateVisit = useCallback((pathname: string) => {
    if (pathname === currentVisit.current.pathname) return;
    const nextVisit = { pathname };
    currentVisit.current = nextVisit;
    setVisit(nextVisit);
  }, []);
  useEffect(() => {
    const onPop = () => updateVisit(window.location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [updateVisit]);
  const navigate = useCallback(
    (path: string, options: { replace?: boolean } = {}) => {
      const url = new URL(path, window.location.origin);
      const target = url.pathname + url.search + url.hash;
      if (options.replace) {
        window.history.replaceState(null, "", target);
      } else {
        window.history.pushState(null, "", target);
      }
      updateVisit(url.pathname);
      window.scrollTo?.(0, 0);
    },
    [updateVisit],
  );
  const value = useMemo(
    () => ({
      route: parseRoute(visit.pathname),
      navigate,
      isCurrentVisit: () =>
        currentVisit.current === visit &&
        window.location.pathname === visit.pathname,
    }),
    [visit, navigate],
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
