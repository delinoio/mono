// SPDX-License-Identifier: Apache-2.0
import { Badge } from "delino-ui/badge";
import { TopBar } from "../components/shell.tsx";
import { useI18n } from "../i18n.ts";
import { Link, RouteId, routePath } from "../router.tsx";

/** Route `home`: introduces the account and billing sections. */
export function HomePage() {
  const { t } = useI18n();
  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <header className="page-header">
          <div>
            <h1>Delino Accounts</h1>
            <p className="muted">{t("home.description")}</p>
          </div>
        </header>
        <ul className="home-sections" role="list">
          <li className="section">
            <h2>{t("home.account.title")}</h2>
            <p className="muted">{t("home.account.description")}</p>
            <Badge>{t("home.account.comingSoon")}</Badge>
          </li>
          <li className="section">
            <h2>{t("home.billing.title")}</h2>
            <p className="muted">{t("home.billing.description")}</p>
            <Link to={routePath({ id: RouteId.Billing })}>
              {t("home.billing.manage")}
            </Link>
          </li>
        </ul>
      </main>
    </>
  );
}
