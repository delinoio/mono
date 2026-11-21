// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery } from "@connectrpc/connect-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { useState } from "react";
import { LIST_PAGE_SIZE } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import { Empty, ErrorNotice, Loading } from "../../components/ui.tsx";
import {
  addMonths,
  formatMonth,
  formatQuantity,
  formatTime,
  labels,
  meterFromId,
  meterLabel,
  serviceLabel,
  type UtcMonth,
  utcMonthOf,
} from "../../format.ts";
import {
  CreditService,
  type LedgerEntry,
  LedgerEntryKind,
} from "../../gen/delino/accounts/v1/credit_pb.ts";
import { BillingUsageService } from "../../gen/delino/accounts/v1/usage_pb.ts";
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
  const [now] = useState(() => utcMonthOf(new Date()));
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
  const total = summary.data?.pages[0]?.totalAmountUsdMicros;
  return (
    <section className="section" aria-labelledby="usage-title">
      <div className="section-header">
        <h2 id="usage-title">Usage</h2>
        <div className="month-picker">
          <Button
            variant={ButtonVariant.Secondary}
            size={ButtonSize.Small}
            aria-label="Previous month"
            disabled={month.year <= 2000 && month.month <= 1}
            onClick={() => setMonth(addMonths(month, -1))}
          >
            ‹
          </Button>
          <span aria-live="polite" className="month-label">
            {formatMonth(month)} (UTC)
          </span>
          <Button
            variant={ButtonVariant.Secondary}
            size={ButtonSize.Small}
            aria-label="Next month"
            disabled={monthIndex(month) >= monthIndex(now)}
            onClick={() => setMonth(addMonths(month, 1))}
          >
            ›
          </Button>
        </div>
      </div>
      {summary.isPending ? (
        <Loading label="Loading usage" />
      ) : summary.error ? (
        <ErrorNotice
          error={summary.error}
          onRetry={() => void summary.refetch()}
        />
      ) : (
        <>
          <p className="usage-total">
            Total{" "}
            <strong aria-label="Usage total">
              {formatUsageCost(total ?? 0n)}
            </strong>
          </p>
          {rows.length === 0 ? (
            <Empty title="No usage in this month" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <caption className="visually-hidden">
                  Usage by service, organization and meter
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Service</th>
                    <th scope="col">Organization</th>
                    <th scope="col">Meter</th>
                    <th scope="col" className="numeric">
                      Quantity
                    </th>
                    <th scope="col" className="numeric">
                      Cost
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr
                      key={`${row.service}/${row.organizationId}/${row.meter}`}
                    >
                      <td>{labels.service[row.service]}</td>
                      <td>
                        <code>{row.organizationId}</code>
                      </td>
                      <td>{labels.meter[row.meter]}</td>
                      <td className="numeric">
                        {formatQuantity(row.quantity, row.meter)}
                      </td>
                      <td className="numeric">
                        {formatUsageCost(row.amountUsdMicros)}
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
              onClick={() => summary.fetchNextPage()}
            >
              Show more usage
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}

function ledgerDetails(entry: LedgerEntry): string {
  if (entry.kind !== LedgerEntryKind.USAGE) {
    return entry.paddleTransactionId !== ""
      ? `Paddle ${entry.paddleTransactionId}`
      : "";
  }
  return [
    serviceLabel(entry.service),
    entry.organizationId,
    meterLabel(entry.meter),
    formatQuantity(entry.quantity, meterFromId(entry.meter)),
  ]
    .filter((part) => part !== "")
    .join(" · ");
}

function ledgerAmount(entry: LedgerEntry): string {
  const text =
    entry.kind === LedgerEntryKind.USAGE
      ? formatUsageCost(entry.amountUsdMicros)
      : formatUsd(entry.amountUsdMicros);
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
  return (
    <section className="section" aria-labelledby="ledger-title">
      <h2 id="ledger-title">Ledger</h2>
      <p className="muted">Every change of the balance, newest first.</p>
      {ledger.isPending ? (
        <Loading label="Loading the ledger" />
      ) : ledger.error ? (
        <ErrorNotice
          error={ledger.error}
          onRetry={() => void ledger.refetch()}
        />
      ) : entries.length === 0 ? (
        <Empty title="No ledger entries yet" />
      ) : (
        <>
          <ul className="list" aria-label="Ledger entries">
            {entries.map((entry) => (
              <li key={entry.ledgerEntryId} className="list-row">
                <div className="list-main">
                  <span className="list-title">
                    {labels.ledgerKind[entry.kind]}
                  </span>
                  <span className="muted">
                    {formatTime(entry.createTime)}
                    {ledgerDetails(entry) !== ""
                      ? ` · ${ledgerDetails(entry)}`
                      : ""}
                  </span>
                </div>
                <div className="list-amount">
                  <span className="amount">{ledgerAmount(entry)}</span>
                  <span className="muted">
                    Balance {formatUsd(entry.balanceAfterUsdMicros)}
                  </span>
                </div>
              </li>
            ))}
          </ul>
          {ledger.hasNextPage ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => ledger.fetchNextPage()}
            >
              Show older entries
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}
