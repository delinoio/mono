// SPDX-License-Identifier: Apache-2.0
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type AnchorHTMLAttributes,
  type MouseEvent,
  type ReactNode,
} from "react";

/**
 * A minimal history-based router. The console has a small, fixed route
 * table (src/routes.ts), so it uses the History API directly instead of a
 * routing dependency.
 */

interface RouterValue {
  readonly pathname: string;
  readonly navigate: (path: string, options?: { replace?: boolean }) => void;
}

const RouterContext = createContext<RouterValue | undefined>(undefined);

export interface RouterProviderProps {
  readonly children: ReactNode;
  /** Runs for the initial path and after every navigation. */
  readonly onPathChange?: (pathname: string) => void;
}

export function RouterProvider({
  children,
  onPathChange,
}: RouterProviderProps) {
  const [pathname, setPathname] = useState(() => window.location.pathname);

  useEffect(() => {
    const onPopState = () => setPathname(window.location.pathname);
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  useEffect(() => {
    onPathChange?.(pathname);
  }, [pathname, onPathChange]);

  const navigate = useCallback(
    (path: string, options?: { replace?: boolean }) => {
      if (options?.replace) {
        window.history.replaceState(null, "", path);
      } else {
        window.history.pushState(null, "", path);
      }
      setPathname(window.location.pathname);
      window.scrollTo?.(0, 0);
    },
    [],
  );

  const value = useMemo(() => ({ pathname, navigate }), [pathname, navigate]);
  return (
    <RouterContext.Provider value={value}>{children}</RouterContext.Provider>
  );
}

export function useRouter(): RouterValue {
  const value = useContext(RouterContext);
  if (value === undefined) {
    throw new Error("useRouter must be used inside RouterProvider");
  }
  return value;
}

export type LinkProps = Omit<
  AnchorHTMLAttributes<HTMLAnchorElement>,
  "href"
> & {
  readonly to: string;
};

/** An anchor that navigates inside the console without a page load. */
export function Link({ to, onClick, ...props }: LinkProps) {
  const { navigate } = useRouter();
  const handleClick = (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event);
    if (
      event.defaultPrevented ||
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
  return <a href={to} onClick={handleClick} {...props} />;
}
