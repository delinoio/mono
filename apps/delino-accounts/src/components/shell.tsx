// SPDX-License-Identifier: Apache-2.0
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { createContext, type ReactNode, useContext } from "react";
import { useAuth } from "../auth.tsx";
import type { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/** The top bar: product name, billing link and sign-out. */
export function TopBar() {
  const auth = useAuth();
  const { route } = useRouter();
  const inBilling =
    route.id === RouteId.Billing || route.id === RouteId.BillingAccount;
  return (
    <header className="topbar">
      <Link to={routePath({ id: RouteId.Home })} className="brand">
        Delino Accounts
      </Link>
      <nav className="topbar-links" aria-label="Main">
        <Link
          to={routePath({ id: RouteId.Billing })}
          aria-current={route.id === RouteId.Billing ? "page" : undefined}
          className={inBilling ? "topbar-link-active" : undefined}
        >
          Billing
        </Link>
        {auth.isAuthenticated ? (
          <Button
            variant={ButtonVariant.Secondary}
            size={ButtonSize.Small}
            onClick={() => auth.signOut()}
          >
            Sign out
          </Button>
        ) : null}
      </nav>
    </header>
  );
}

/** A page with the top bar. */
export function Page({
  children,
  narrow = false,
}: {
  children: ReactNode;
  narrow?: boolean;
}) {
  return (
    <>
      <TopBar />
      <main className={narrow ? "content content-narrow" : "content"}>
        {children}
      </main>
    </>
  );
}

export interface BillingAccountContextValue {
  billingAccountId: string;
  name: string;
  /** The role of the signed-in user in the billing account. */
  role: BillingAccountRole;
}

const BillingAccountContext = createContext<
  BillingAccountContextValue | undefined
>(undefined);

export const BillingAccountProvider = BillingAccountContext.Provider;

/** The billing account of the current page and the caller's role in it. */
export function useBillingAccount(): BillingAccountContextValue {
  const value = useContext(BillingAccountContext);
  if (value === undefined) {
    throw new Error("useBillingAccount needs a billing account page");
  }
  return value;
}
