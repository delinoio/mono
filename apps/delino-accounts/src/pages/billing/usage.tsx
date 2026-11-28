// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { useState } from "react";
import { LIST_PAGE_SIZE } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import { useCurrentUtcMonth } from "../../current-utc-month.ts";
import {
  Empty,
  ErrorNotice,
  InfiniteListNotice,
  Loading,
} from "../../components/ui.tsx";
import {
  addMonths,
  formatMonth,
  formatQuantity,
  formatTime,
  labels,
  meterFromId,
  meterLabel,
  serviceLabel,
  serviceName,
  type UtcMonth,
} from "../../format.ts";
import { useI18n } from "../../i18n.ts";
import type { Text } from "../../text.ts";
import {
  CreditService,
  type LedgerEntry,
  LedgerEntryKind,
} from "@delino/accounts-protos/v1/credit_pb";
import { BillingUsageService } from "@delino/accounts-protos/v1/usage_pb";
import { formatUsageCost, formatUsd } from "../../money.ts";

/** The usage tab: the monthly usage summary and the ledger. */
export function UsageSection() {
  return (
    <div className="stack">
      <UsageSummary />
      <Ledger />
    </div>
  );
}

function monthIndex(value: UtcMonth): number {
  return value.year * 12 + value.month;
}

function UsageSummary() {
  const { billingAccountId } = useBillingAccount();
  // The upper limit of the picker follows the clock; the selected month does not.
  const now = useCurrentUtcMonth();
  const [month, setMonth] = useState<UtcMonth>(now);
  const summary = useInfiniteQuery(
    BillingUsageService.method.listUsageSummary,
    {
      billingAccountId,
      year: month.year,
      month: month.month,
      pageSize: LIST_PAGE_SIZE,
      pageToken: "",
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const rows = summary.data?.pages.flatMap((page) => page.rows) ?? [];
  const total = summary.data?.pages.at(-1)?.totalAmountUsdMicros;
  const i18n = useI18n();
  const { lang, t, rich } = i18n;
  return (
    <section className="section" aria-labelledby="usage-title">
      <div className="section-header">
        <h2 id="usage-title">{t("usage.title")}</h2>
        <div className="month-picker">
          <Button
            variant={ButtonVariant.Secondary}
            size={ButtonSize.Small}
            aria-label={t("usage.previousMonth")}
            disabled={month.year <= 2000 && month.month <= 1}
            onClick={() => setMonth(addMonths(month, -1))}
          >
            ‹
          </Button>
          <span aria-live="polite" className="month-label">
            {t("usage.month", { month: formatMonth(month, lang) })}
          </span>
          <Button
            variant={ButtonVariant.Secondary}
            size={ButtonSize.Small}
            aria-label={t("usage.nextMonth")}
            disabled={monthIndex(month) >= monthIndex(now)}
            onClick={() => setMonth(addMonths(month, 1))}
          >
            ›
          </Button>
        </div>
      </div>
      <InfiniteListNotice list={summary} />
      {summary.isPending ? (
        <Loading label={t("usage.loading")} />
      ) : summary.isLoadingError ? (
        <ErrorNotice
          error={summary.error}
          onRetry={() => void summary.refetch()}
        />
      ) : (
        <>
          <p className="usage-total">
            {rich("usage.total", {
              amount: (
                <strong aria-label={t("usage.totalLabel")}>
                  {formatUsageCost(total ?? 0n, lang)}
                </strong>
              ),
            })}
          </p>
          {rows.length === 0 ? (
            <Empty title={t("usage.empty")} />
          ) : (
            <div
              className="table-wrap"
              role="region"
              aria-labelledby="usage-table-caption"
              tabIndex={0}
            >
              <table className="table">
                <caption id="usage-table-caption" className="visually-hidden">
                  {t("usage.caption")}
                </caption>
                <thead>
                  <tr>
                    <th scope="col">{t("usage.column.service")}</th>
                    <th scope="col">{t("usage.column.organization")}</th>
                    <th scope="col">{t("usage.column.meter")}</th>
                    <th scope="col" className="numeric">
                      {t("usage.column.quantity")}
                    </th>
                    <th scope="col" className="numeric">
                      {t("usage.column.cost")}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr
                      key={`${row.service}/${row.organizationId}/${row.meter}`}
                    >
                      <td>{serviceName(row.service, i18n)}</td>
                      <td>
                        <code>{row.organizationId}</code>
                      </td>
                      <td>{t(labels.meter[row.meter])}</td>
                      <td className="numeric">
                        {formatQuantity(row.quantity, row.meter, i18n)}
                      </td>
                      <td className="numeric">
                        {formatUsageCost(row.amountUsdMicros, lang)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {summary.hasNextPage ? (
            <Button
              variant={ButtonVariant.Secondary}
              // A refresh of the loaded pages (a window-focus refetch, for
              // example) holds the continuation token of its first page
              // until it finishes. Reading on with the cached token would
              // cancel the refresh or skip rows, so wait for the refresh,
              // and after a failed one wait for its retry. `cancelRefetch:
              // false` keeps a click that races with a refresh from
              // cancelling it.
              disabled={summary.isFetching || summary.isRefetchError}
              onClick={() =>
                void summary.fetchNextPage({ cancelRefetch: false })
              }
            >
              {t("usage.showMore")}
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}

function ledgerDetails(entry: LedgerEntry, text: Text): string {
  if (entry.kind !== LedgerEntryKind.USAGE) {
    // A product name and an identifier: nothing to translate.
    return entry.paddleTransactionId !== ""
      ? `Paddle ${entry.paddleTransactionId}`
      : "";
  }
  return [
    serviceLabel(entry.service, text),
    entry.organizationId,
    meterLabel(entry.meter, text),
    formatQuantity(entry.quantity, meterFromId(entry.meter), text),
  ]
    .filter((part) => part !== "")
    .join(" · ");
}

function ledgerAmount(entry: LedgerEntry, lang: string): string {
  const text =
    entry.kind === LedgerEntryKind.USAGE
      ? formatUsageCost(entry.amountUsdMicros, lang)
      : formatUsd(entry.amountUsdMicros, lang);
  return entry.amountUsdMicros > 0n ? `+${text}` : text;
}

function Ledger() {
  const { billingAccountId } = useBillingAccount();
  const ledger = useInfiniteQuery(
    CreditService.method.listLedgerEntries,
    { billingAccountId, pageSize: 50, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const entries =
    ledger.data?.pages.flatMap((page) => page.ledgerEntries) ?? [];
  const i18n = useI18n();
  const { lang, t } = i18n;
  return (
    <section className="section" aria-labelledby="ledger-title">
      <h2 id="ledger-title">{t("ledger.title")}</h2>
      <p className="muted">{t("ledger.description")}</p>
      <InfiniteListNotice list={ledger} />
      {ledger.isPending ? (
        <Loading label={t("ledger.loading")} />
      ) : ledger.isLoadingError ? (
        <ErrorNotice
          error={ledger.error}
          onRetry={() => void ledger.refetch()}
        />
      ) : entries.length === 0 ? (
        <Empty title={t("ledger.empty")} />
      ) : (
        <>
          <ul className="list" aria-label={t("ledger.list")}>
            {entries.map((entry) => {
              const details = ledgerDetails(entry, i18n);
              return (
                <li key={entry.ledgerEntryId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {t(labels.ledgerKind[entry.kind])}
                    </span>
                    <span className="muted">
                      {formatTime(entry.createTime, lang)}
                      {details !== "" ? ` · ${details}` : ""}
                    </span>
                  </div>
                  <div className="list-amount">
                    <span className="amount">{ledgerAmount(entry, lang)}</span>
                    <span className="muted">
                      {t("ledger.balance", {
                        amount: formatUsd(entry.balanceAfterUsdMicros, lang),
                      })}
                    </span>
                  </div>
                </li>
              );
            })}
          </ul>
          {ledger.hasNextPage ? (
            <Button
              variant={ButtonVariant.Secondary}
              // A refresh of the loaded pages (a window-focus refetch, for
              // example) holds the continuation token of its first page
              // until it finishes. Reading on with the cached token would
              // cancel the refresh or skip rows, so wait for the refresh,
              // and after a failed one wait for its retry. `cancelRefetch:
              // false` keeps a click that races with a refresh from
              // cancelling it.
              disabled={ledger.isFetching || ledger.isRefetchError}
              onClick={() =>
                void ledger.fetchNextPage({ cancelRefetch: false })
              }
            >
              {t("ledger.showOlder")}
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}
