// SPDX-License-Identifier: Apache-2.0
import type { ReactNode } from "react";
import { useI18n } from "../i18n.ts";
import type { useMembers } from "../members.ts";
import { ErrorNotice, QueryBoundary } from "./ui.tsx";

export function MemberListStatus({
  roster: { query },
  children,
}: {
  roster: ReturnType<typeof useMembers>;
  children?: ReactNode;
}) {
  const { t } = useI18n();
  return (
    <QueryBoundary query={query} loading={t("members.loading")}>
      {() => (
        <>
          {query.hasNextPage ? (
            query.error && !query.isFetching ? (
              <p className="muted">{t("members.partial")}</p>
            ) : (
              <p className="muted" role="status">
                {t("members.loadingMore")}
              </p>
            )
          ) : null}
          {query.isFetchNextPageError && !query.isFetching ? (
            <ErrorNotice
              error={query.error}
              onRetry={() => void query.fetchNextPage({ cancelRefetch: false })}
            />
          ) : null}
          {children}
        </>
      )}
    </QueryBoundary>
  );
}
