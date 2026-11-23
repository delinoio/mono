// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { skipToken, useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { RadioGroup, RadioGroupItem } from "delino-ui/radio-group";
import { toast } from "delino-ui/toast";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import {
  BillingMeter,
  type BillingMeterUsage,
  BillingService,
} from "@delino/ownsand-protos/v1/billing_pb";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  FormDialog,
  Loading,
  RefreshFailedNotice,
  showsFullError,
} from "../components/ui.tsx";
import { ACCOUNTS_URL } from "../config.ts";
import {
  formatByteCount,
  formatCount,
  formatGibMonths,
  formatUsageCostUsdMicros,
  formatUsdMicros,
  labels,
} from "../format.ts";
import { can, Permission } from "../roles.ts";

/** The usage quantity of a meter in its display unit. */
function meterQuantity(usage: BillingMeterUsage): string {
  switch (usage.meter) {
    case BillingMeter.RELAY_EGRESS:
      return formatByteCount(usage.quantity);
    case BillingMeter.R2_STORAGE:
      return formatGibMonths(usage.quantity);
    case BillingMeter.CLASS_A_RPC:
    case BillingMeter.CLASS_B_RPC:
      return `${formatCount(usage.quantity)} calls`;
    default:
      return formatCount(usage.quantity);
  }
}

/** The UTC calendar month of a timestamp, for example "November 2026". */
function utcMonth(ts: Timestamp | undefined): string {
  if (ts === undefined) return "This month";
  return new Date(Number(ts.seconds) * 1000).toLocaleDateString("en-US", {
    month: "long",
    year: "numeric",
    timeZone: "UTC",
  });
}

function AccountsLink() {
  return (
    <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
      accounts.delino.io
    </a>
  );
}

/**
 * The billing section of the `settings` route, for the owner and admins:
 * the linked billing account, its balance and this UTC month's usage and
 * cost of each meter. Without a link it shows the link flow (owner) or a
 * notice (admin) and does not call `GetBillingUsage`.
 */
export function BillingSection() {
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const status = useQuery(BillingService.method.getBillingStatus, {
    organizationId,
  });
  const linked = status.data?.billing?.linked;
  const usage = useQuery(
    BillingService.method.getBillingUsage,
    linked === true ? { organizationId } : skipToken,
  );
  const unlink = useMutation(BillingService.method.unlinkBillingAccount, {
    onSuccess: () => invalidateServices(queryClient, BillingService),
  });
  const [linking, setLinking] = useState(false);
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const owner = can(role, Permission.ManageBillingLink);

  let body;
  if (status.isPending) {
    body = <Loading label="Loading the billing status" />;
  } else if (status.data === undefined || showsFullError(status)) {
    body = (
      <ErrorNotice error={status.error} onRetry={() => void status.refetch()} />
    );
  } else if (!linked) {
    body = owner ? (
      <div className="form">
        <p className="callout">
          No billing account is linked. OwnSand rejects new sandboxes,
          snapshots, templates and enrollment tokens until you link one.
        </p>
        <div className="actions">
          <Button onClick={() => setLinking(true)}>
            Link a billing account
          </Button>
        </div>
      </div>
    ) : (
      <p className="callout">
        No billing account is linked. The organization owner must link one.
      </p>
    );
  } else {
    // Linked: the usage portion depends on `GetBillingUsage`, but the owner's
    // unlink action must not, so a loading or failed usage request keeps it.
    let usageBody;
    if (usage.isPending) {
      usageBody = <Loading label="Loading the billing usage" />;
    } else if (usage.data === undefined || showsFullError(usage)) {
      usageBody = (
        <ErrorNotice error={usage.error} onRetry={() => void usage.refetch()} />
      );
    } else {
      const data = usage.data;
      const total = data.meterUsages.reduce(
        (sum, u) => sum + u.amountUsdMicros,
        0n,
      );
      usageBody = (
        <>
          <dl className="details details-flat">
            <Detail term="Billing account">{data.billingAccountName}</Detail>
            <Detail term="Balance">
              {formatUsdMicros(data.balanceUsdMicros)}
              {data.balanceUsdMicros < 0n ? " (debt)" : null}
            </Detail>
            <Detail term={`Cost in ${utcMonth(data.monthStart)} (UTC)`}>
              {formatUsdMicros(total)}
            </Detail>
          </dl>
          <div className="table-wrap">
            <table className="table" aria-label="Usage this month">
              <thead>
                <tr>
                  <th scope="col">Meter</th>
                  <th scope="col">Usage</th>
                  <th scope="col">Cost</th>
                </tr>
              </thead>
              <tbody>
                {data.meterUsages.map((u) => (
                  <tr key={u.meter}>
                    <td>{labels.billingMeter[u.meter]}</td>
                    <td>{meterQuantity(u)}</td>
                    <td>{formatUsageCostUsdMicros(u.amountUsdMicros)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted">
            Usage of about the last 10 minutes is not included yet. R2 storage
            shows in GiB-months of 730 hours.
          </p>
        </>
      );
    }
    body = (
      <div className="form">
        {usageBody}
        {owner ? (
          <div className="actions">
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setConfirmUnlink(true)}
            >
              Unlink billing account
            </Button>
          </div>
        ) : null}
      </div>
    );
  }

  return (
    <section className="section" aria-labelledby="billing-title">
      <h2 id="billing-title">Billing</h2>
      <p className="muted">
        OwnSand charges the usage of this organization to a Delino billing
        account. Pay, top up and manage billing accounts at <AccountsLink />.
      </p>
      {[status, usage].map((query, i) =>
        query.error && !showsFullError(query) ? (
          <RefreshFailedNotice key={i} query={query} />
        ) : null,
      )}
      {body}
      {owner ? (
        <>
          <LinkBillingDialog open={linking} onOpenChange={setLinking} />
          <ConfirmDialog
            open={confirmUnlink}
            onOpenChange={setConfirmUnlink}
            title={`Unlink ${usage.data?.billingAccountName ?? "the billing account"}?`}
            description="The organization becomes blocked at once: OwnSand rejects new sandboxes, snapshots, templates and enrollment tokens until you link a billing account again. Usage so far stays on this billing account."
            confirmLabel="Unlink billing account"
            onConfirm={() => unlink.mutateAsync({ organizationId })}
          />
        </>
      ) : null}
    </section>
  );
}

/**
 * The link flow: lists the billing accounts in which the user is owner or
 * admin and links the chosen one.
 */
function LinkBillingDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { organizationId, name } = useOrg();
  const queryClient = useQueryClient();
  const accounts = useQuery(
    BillingService.method.listLinkableBillingAccounts,
    open ? { organizationId, pageSize: 200 } : skipToken,
  );
  const link = useMutation(BillingService.method.linkBillingAccount, {
    onSuccess: () => invalidateServices(queryClient, BillingService),
  });
  const [selected, setSelected] = useState<string | null>(null);
  const [selectionError, setSelectionError] = useState<string>();
  const listRef = useRef<HTMLDivElement>(null);
  const list = accounts.data?.billingAccounts;

  // The account choice is the only input, so it gets the focus. The list
  // can arrive after the dialog opened, when the dialog's own initial focus
  // has already run, so focus the checked or first option once it renders.
  useEffect(() => {
    if (!open || list === undefined || list.length === 0) return;
    const radio =
      listRef.current?.querySelector<HTMLElement>(
        '[role="radio"][aria-checked="true"]',
      ) ?? listRef.current?.querySelector<HTMLElement>('[role="radio"]');
    radio?.focus();
  }, [open, list]);

  const close = (next: boolean) => {
    onOpenChange(next);
    if (!next) {
      setSelected(null);
      setSelectionError(undefined);
    }
  };

  let content;
  if (accounts.isPending) {
    content = <Loading label="Loading your billing accounts" />;
  } else if (accounts.data === undefined || showsFullError(accounts)) {
    content = (
      <ErrorNotice
        error={accounts.error}
        onRetry={() => void accounts.refetch()}
      />
    );
  } else if (list === undefined || list.length === 0) {
    content = (
      <p className="callout">
        You are not an owner or admin of any billing account. Create a billing
        account at <AccountsLink />, then link it here.
      </p>
    );
  } else {
    content = (
      <div ref={listRef}>
        <RadioGroup
          label="Billing account"
          value={selected}
          onValueChange={(v: unknown) => {
            if (typeof v === "string") {
              setSelected(v);
              setSelectionError(undefined);
            }
          }}
          error={selectionError}
        >
          {list.map((a, i) => (
            <RadioGroupItem
              key={a.billingAccountId}
              value={a.billingAccountId}
              data-autofocus={i === 0 ? "" : undefined}
            >
              {a.name} · {labels.billingAccountRole[a.role]}
            </RadioGroupItem>
          ))}
        </RadioGroup>
      </div>
    );
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={close}
      title="Link a billing account"
      description={`OwnSand charges the usage of ${name} to the billing account you choose. You can choose a billing account in which you are an owner or admin.`}
      submitLabel="Link"
      submitDisabled={list === undefined || list.length === 0}
      onSubmit={async (opening) => {
        if (selected === null) {
          setSelectionError("Choose a billing account.");
          return;
        }
        const res = await link.mutateAsync({
          organizationId,
          billingAccountId: selected,
        });
        // The success is real for a dismissed opening too, so its toast
        // stays; only the close of a later opening is guarded.
        if (opening.isCurrent()) close(false);
        toast.success(`${res.billingAccountName} is linked.`);
      }}
    >
      {accounts.error && !showsFullError(accounts) ? (
        <RefreshFailedNotice query={accounts} />
      ) : null}
      {content}
    </FormDialog>
  );
}
