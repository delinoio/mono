// SPDX-License-Identifier: Apache-2.0
import { Badge } from "delino-ui/badge";
import { TopBar } from "../components/shell.tsx";
import { Link, RouteId, routePath } from "../router.tsx";

/** Route `home`: introduces the account and billing sections. */
export function HomePage() {
  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <header className="page-header">
          <div>
            <h1>Delino Accounts</h1>
            <p className="muted">
              Manage your Delino account and billing information.
            </p>
          </div>
        </header>
        <ul className="home-sections" role="list">
          <li className="section">
            <h2>Account</h2>
            <p className="muted">Manage your profile and preferences.</p>
            <Badge>Coming soon</Badge>
          </li>
          <li className="section">
            <h2>Billing</h2>
            <p className="muted">
              Manage billing accounts, subscriptions, credit and invoices.
            </p>
            <Link to={routePath({ id: RouteId.Billing })}>Manage billing</Link>
          </li>
        </ul>
      </main>
    </>
  );
}
