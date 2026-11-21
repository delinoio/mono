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

/** Stable route IDs of the web app contract. */
export enum RouteId {
  Home = "home",
  Billing = "billing",
  BillingAccount = "billing-account",
  BillingInvitation = "billing-invitation",
  Callback = "callback",
  NotFound = "not-found",
}

export type Route =
  | { id: RouteId.Home }
  | { id: RouteId.Billing }
  | { id: RouteId.BillingAccount; billingAccountId: string }
  | { id: RouteId.BillingInvitation }
  | { id: RouteId.Callback }
  | { id: RouteId.NotFound };

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
  if (parts.length === 0) return { id: RouteId.Home };
  if (parts.length === 1 && parts[0] === "callback") {
    return { id: RouteId.Callback };
  }
  if (parts[0] !== "billing") return { id: RouteId.NotFound };
  if (parts.length === 1) return { id: RouteId.Billing };
  if (
    parts.length === 3 &&
    parts[1] === "invitations" &&
    parts[2] === "accept"
  ) {
    return { id: RouteId.BillingInvitation };
  }
  // `invitations` is the invitation path segment, never a billing account
  // ID (accounts-api generates UUID v7 IDs).
  if (parts.length === 2 && parts[1] !== "invitations") {
    return { id: RouteId.BillingAccount, billingAccountId: parts[1]! };
  }
  return { id: RouteId.NotFound };
}

/** Builds the path of a route. */
export function routePath(route: Route): string {
  switch (route.id) {
    case RouteId.Home:
    case RouteId.NotFound:
      return "/";
    case RouteId.Billing:
      return "/billing";
    case RouteId.BillingAccount:
      return `/billing/${encodeURIComponent(route.billingAccountId)}`;
    case RouteId.BillingInvitation:
      return "/billing/invitations/accept";
    case RouteId.Callback:
      return "/callback";
  }
}

interface RouterValue {
  route: Route;
  navigate: (path: string, options?: { replace?: boolean }) => void;
}

const RouterContext = createContext<RouterValue | undefined>(undefined);

/** Listeners that run before every in-app navigation. */
type NavigationListener = (from: Route, to: Route) => void;

/**
 * A small history-API router. The app has a fixed set of routes, so a
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
    const onPop = () => {
      const to = window.location.pathname;
      onNavigate?.(parseRoute(pathname), parseRoute(to));
      setPathname(to);
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [onNavigate, pathname]);
  const navigate = useCallback(
    (path: string, options: { replace?: boolean } = {}) => {
      const from = parseRoute(window.location.pathname);
      const url = new URL(path, window.location.origin);
      onNavigate?.(from, parseRoute(url.pathname));
      const target = url.pathname + url.search + url.hash;
      if (options.replace) {
        window.history.replaceState(null, "", target);
      } else {
        window.history.pushState(null, "", target);
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

/** An in-app link that navigates without a page load. */
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
