// SPDX-License-Identifier: Apache-2.0
import { useInfiniteQuery, useTransport } from "@connectrpc/connect-query";
import { createClient } from "@connectrpc/connect";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { toast } from "delino-ui/toast";
import { useMemo } from "react";
import { LIST_PAGE_SIZE } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  Loading,
  RefetchNotice,
} from "../../components/ui.tsx";
import { errorMessage, formatDate, labels } from "../../format.ts";
import {
  CreditService,
  type Invoice,
} from "@delino/accounts-protos/v1/credit_pb";
import { formatUsd } from "../../money.ts";

/**
 * Opens an empty tab synchronously, inside the click, so that pop-up
 * blockers allow it, and returns functions that navigate it to the invoice
 * once the link arrives or close it on failure.
 */
const invoiceWindow = {
  prepare(): { navigate(url: string): void; close(): void } {
    const opened = window.open("", "_blank");
    if (opened !== null) {
      // The invoice page must not reach back into the app.
      opened.opener = null;
    }
    return {
      navigate(url) {
        if (opened === null || opened.closed) {
          // A blocked pop-up: open the invoice in this tab instead.
          window.location.assign(url);
        } else {
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
  const open = async (invoice: Invoice) => {
    const target = invoiceWindow.prepare();
    try {
      const res = await client.getInvoiceUrl({
        billingAccountId,
        paddleTransactionId: invoice.paddleTransactionId,
      });
      target.navigate(res.invoiceUrl);
    } catch (error) {
      target.close();
      toast.error(errorMessage(error));
    }
  };
  return (
    <section className="section" aria-labelledby="invoices-title">
      <h2 id="invoices-title">Invoices</h2>
      <p className="muted">
        Paddle is the merchant of record and issues the invoices. Amounts
        exclude tax.
      </p>
      <RefetchNotice
        error={invoices.isRefetchError ? invoices.error : undefined}
        onRetry={() => void invoices.refetch()}
      />
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
              onClick={() => invoices.fetchNextPage()}
            >
              Show older invoices
            </Button>
          ) : null}
        </>
      )}
    </section>
  );
}
