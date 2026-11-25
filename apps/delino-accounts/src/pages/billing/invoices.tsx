// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery, useTransport } from "@connectrpc/connect-query";
import { createClient } from "@connectrpc/connect";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { useLayoutEffect, useMemo, useRef } from "react";
import { LIST_PAGE_SIZE } from "../../api.ts";
import { useActionFailure } from "../../components/action-failure.tsx";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  InfiniteListNotice,
  Loading,
} from "../../components/ui.tsx";
import { formatDate, labels } from "../../format.ts";
import {
  CreditService,
  type Invoice,
} from "@delino/accounts-protos/v1/credit_pb";
import { formatUsd } from "../../money.ts";

/**
 * Opens an empty tab synchronously, inside the click, so that pop-up
 * blockers allow it, and returns functions that navigate it to the invoice
 * once the link arrives or close it on failure. A blocked pop-up can use the
 * current tab while the initiating view is still active.
 */
const invoiceWindow = {
  prepare(): {
    navigate(url: string, canNavigateCurrentTab: boolean): void;
    close(): void;
  } {
    const opened = window.open("", "_blank");
    if (opened !== null) {
      // The invoice page must not reach back into the app.
      opened.opener = null;
    }
    return {
      navigate(url, canNavigateCurrentTab) {
        if (opened === null && canNavigateCurrentTab) {
          window.location.assign(url);
        } else if (opened !== null && !opened.closed) {
          opened.location.replace(url);
        }
      },
      close() {
        opened?.close();
      },
    };
  },
};

/**
 * The invoices tab (owner and admin only): completed Paddle transactions and
 * a short-lived link to each invoice PDF. The link is fetched only when the
 * user opens it and is never stored or logged.
 */
export function InvoicesSection() {
  const { billingAccountId } = useBillingAccount();
  const transport = useTransport();
  // A direct client instead of a cached query: the link expires soon, so it
  // must not live in the React Query cache.
  const client = useMemo(
    () => createClient(CreditService, transport),
    [transport],
  );
  const invoices = useInfiniteQuery(
    CreditService.method.listInvoices,
    { billingAccountId, pageSize: LIST_PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const list = invoices.data?.pages.flatMap((page) => page.invoices) ?? [];
  const actionFailure = useActionFailure();
  const viewSignal = useRef<AbortSignal | undefined>(undefined);
  useLayoutEffect(() => {
    const controller = new AbortController();
    viewSignal.current = controller.signal;
    // Revoke same-tab navigation as soon as this view leaves the screen.
    return () => controller.abort();
  }, [billingAccountId]);

  const open = async (invoice: Invoice) => {
    const initiatingView = viewSignal.current;
    const target = invoiceWindow.prepare();
    try {
      const res = await client.getInvoiceUrl({
        billingAccountId,
        paddleTransactionId: invoice.paddleTransactionId,
      });
      target.navigate(res.invoiceUrl, initiatingView?.aborted === false);
      actionFailure.clear();
    } catch (error) {
      target.close();
      actionFailure.report(error);
    }
  };
  return (
    <section className="section" aria-labelledby="invoices-title">
      <h2 id="invoices-title">Invoices</h2>
      <p className="muted">
        Paddle is the merchant of record and issues the invoices. Amounts
        exclude tax.
      </p>
      <InfiniteListNotice list={invoices} />
      {actionFailure.notice}
      {invoices.isPending ? (
        <Loading label="Loading invoices" />
      ) : invoices.isLoadingError ? (
        <ErrorNotice
          error={invoices.error}
          onRetry={() => void invoices.refetch()}
        />
      ) : list.length === 0 ? (
        <Empty title="No invoices yet" />
      ) : (
        <>
          <ul className="list" aria-label="Invoices">
            {list.map((invoice) => (
              <li key={invoice.paddleTransactionId} className="list-row">
                <div className="list-main">
                  <span className="list-title">
                    {labels.ledgerKind[invoice.kind]} ·{" "}
                    {formatUsd(invoice.amountUsdMicros)}
                  </span>
                  <span className="muted">
                    {formatDate(invoice.createTime)} ·{" "}
                    <code>{invoice.paddleTransactionId}</code>
                  </span>
                </div>
                <Button
                  variant={ButtonVariant.Secondary}
                  size={ButtonSize.Small}
                  aria-label={`Open the invoice of ${formatDate(invoice.createTime)} for ${formatUsd(invoice.amountUsdMicros)}`}
                  onClick={() => open(invoice)}
                >
                  Open invoice
                </Button>
              </li>
            ))}
          </ul>
          {invoices.hasNextPage ? (
            <Button
              variant={ButtonVariant.Secondary}
              // A refresh of the loaded pages (a window-focus refetch, for
              // example) holds the continuation token of its first page
              // until it finishes. Reading on with the cached token would
              // cancel the refresh or skip rows, so wait for the refresh,
              // and after a failed one wait for its retry. `cancelRefetch:
              // false` keeps a click that races with a refresh from
              // cancelling it.
              disabled={invoices.isFetching || invoices.isRefetchError}
              onClick={() =>
                void invoices.fetchNextPage({ cancelRefetch: false })
              }
            >
              Show older invoices
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}
