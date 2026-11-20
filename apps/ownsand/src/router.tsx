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
  Sandboxes = "sandboxes",
  Sandbox = "sandbox",
  Devices = "devices",
  Device = "device",
  Members = "members",
  EnrollmentTokens = "enrollment-tokens",
  ApiKeys = "api-keys",
  Snapshots = "snapshots",
  Settings = "settings",
  NotFound = "not-found",
}

export type Route =
  | { id: RouteId.Organizations }
  | { id: RouteId.Invitation }
  | { id: RouteId.Callback }
  | { id: RouteId.NotFound }
  | {
      id:
        | RouteId.Sandboxes
        | RouteId.Devices
        | RouteId.Members
        | RouteId.EnrollmentTokens
        | RouteId.ApiKeys
        | RouteId.Snapshots
        | RouteId.Settings;
      organizationId: string;
    }
  | { id: RouteId.Sandbox; organizationId: string; sandboxId: string }
  | { id: RouteId.Device; organizationId: string; deviceId: string };

const ORG_SECTIONS: Record<string, RouteId> = {
  sandboxes: RouteId.Sandboxes,
  devices: RouteId.Devices,
  members: RouteId.Members,
  "enrollment-tokens": RouteId.EnrollmentTokens,
  "api-keys": RouteId.ApiKeys,
  snapshots: RouteId.Snapshots,
  settings: RouteId.Settings,
};

/** Parses a path into a route. */
export function parseRoute(pathname: string): Route {
  const parts = pathname
    .split("/")
    .filter((p) => p !== "")
    .map(decodeURIComponent);
  if (parts.length === 0) return { id: RouteId.Organizations };
  if (
    parts.length === 2 &&
    parts[0] === "invitations" &&
    parts[1] === "accept"
  ) {
    return { id: RouteId.Invitation };
  }
  if (parts.length === 1 && parts[0] === "callback")
    return { id: RouteId.Callback };
  if (parts[0] === "orgs" && parts.length >= 3) {
    const organizationId = parts[1]!;
    const section = ORG_SECTIONS[parts[2]!];
    if (section === undefined) return { id: RouteId.NotFound };
    if (parts.length === 3) {
      return { id: section, organizationId } as Route;
    }
    if (parts.length === 4 && section === RouteId.Sandboxes) {
      return { id: RouteId.Sandbox, organizationId, sandboxId: parts[3]! };
    }
    if (parts.length === 4 && section === RouteId.Devices) {
      return { id: RouteId.Device, organizationId, deviceId: parts[3]! };
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
    case RouteId.Sandbox:
      return `/orgs/${e(route.organizationId)}/sandboxes/${e(route.sandboxId)}`;
    case RouteId.Device:
      return `/orgs/${e(route.organizationId)}/devices/${e(route.deviceId)}`;
    default:
      return `/orgs/${e(route.organizationId)}/${route.id}`;
  }
}

interface RouterValue {
  route: Route;
  navigate: (path: string, options?: { replace?: boolean }) => void;
}

const RouterContext = createContext<RouterValue | undefined>(undefined);

/** Listeners that run before every in-console navigation. */
type NavigationListener = (from: Route, to: Route) => void;

/**
 * A small history-API router. The console has a fixed set of routes, so a
 * routing library is not needed.
 */
export function RouterProvider({
  children,
  onNavigate,
}: {
  children: ReactNode;
  onNavigate?: NavigationListener;
}) {
  const [pathname, setPathname] = useState(() => window.location.pathname);
  useEffect(() => {
    const onPop = () => setPathname(window.location.pathname);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const navigate = useCallback(
    (path: string, options: { replace?: boolean } = {}) => {
      const from = parseRoute(window.location.pathname);
      const url = new URL(path, window.location.origin);
      onNavigate?.(from, parseRoute(url.pathname));
      if (options.replace) {
        window.history.replaceState(
          null,
          "",
          url.pathname + url.search + url.hash,
        );
      } else {
        window.history.pushState(
          null,
          "",
          url.pathname + url.search + url.hash,
        );
      }
      setPathname(url.pathname);
      window.scrollTo?.(0, 0);
    },
    [onNavigate],
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
