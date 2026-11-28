// SPDX-License-Identifier: Apache-2.0
import { useLayoutEffect, useState } from "react";
import {
  LEFT_ORGANIZATION_PARAM,
  TopBar,
  useOrganizations,
} from "../components/shell.tsx";
import { Empty, PageHeader, QueryBoundary } from "../components/ui.tsx";
import { SANDRUN_CONSOLE_URL } from "../config.ts";
import { labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { Link, orgPath, RouteId } from "../router.tsx";
import { readSelectedOrganization } from "../storage.ts";

/**
 * Takes the "left the organization" marker from the address, so a reload
 * does not show the notice again.
 */
function takeLeftNotice(): boolean {
  const url = new URL(window.location.href);
  if (!url.searchParams.has(LEFT_ORGANIZATION_PARAM)) return false;
  url.searchParams.delete(LEFT_ORGANIZATION_PARAM);
  window.history.replaceState(
    window.history.state,
    "",
    url.pathname + url.search + url.hash,
  );
  return true;
}

/** Route `ORGANIZATION_PICK`: the caller's SandRun organizations. */
export function OrganizationsPage() {
  const orgs = useOrganizations();
  const { t, rich } = useI18n();
  const [left, setLeft] = useState(false);
  const [lastUsed] = useState(readSelectedOrganization);
  useLayoutEffect(() => {
    if (takeLeftNotice()) setLeft(true);
  }, []);

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title={t("organizations.title")}
          description={rich("organizations.description", {
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
        />
        {left ? (
          <div className="notice" role="status">
            <p>{t("organizations.leftNotice")}</p>
          </div>
        ) : null}
        <QueryBoundary query={orgs} loading={t("organizations.loading")}>
          {(data) => {
            if (data.length === 0) {
              return (
                <Empty title={t("organizations.empty.title")}>
                  {t("organizations.empty.body")}{" "}
                  <a
                    href={SANDRUN_CONSOLE_URL}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    {t("organizations.empty.open")}
                  </a>
                </Empty>
              );
            }
            // The last used organization comes first.
            const sorted = [...data].sort(
              (a, b) =>
                Number(b.organizationId === lastUsed) -
                Number(a.organizationId === lastUsed),
            );
            return (
              <ul className="list" aria-label={t("organizations.title")}>
                {sorted.map((org) => (
                  <li key={org.organizationId} className="list-row">
                    <Link
                      to={orgPath(RouteId.SessionList, org.organizationId)}
                      className="list-link"
                    >
                      <span className="list-title">{org.name}</span>{" "}
                      <span className="muted">
                        {t(labels.role[org.callerRole])}
                        {org.organizationId === lastUsed
                          ? ` · ${t("organizations.lastUsed")}`
                          : ""}
                      </span>
                    </Link>
                  </li>
                ))}
              </ul>
            );
          }}
        </QueryBoundary>
      </main>
    </>
  );
}
