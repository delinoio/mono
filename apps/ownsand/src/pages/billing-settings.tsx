// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
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
import { ACCOUNTS_HOST } from "../components/billing.tsx";
import { ACCOUNTS_URL } from "../config.ts";
import {
  formatByteCount,
  formatCount,
  formatGibMonths,
  formatUsageCostUsdMicros,
  formatUsdMicros,
  labels,
} from "../format.ts";
import { useI18n } from "../i18n.ts";
import { can, Permission } from "../roles.ts";
import { enumText, type Text } from "../text.ts";

/** The usage quantity of a meter in its display unit. */
function meterQuantity(usage: BillingMeterUsage, text: Text): string {
  switch (usage.meter) {
    case BillingMeter.RELAY_EGRESS:
      return formatByteCount(usage.quantity, text.lang);
    case BillingMeter.R2_STORAGE:
      return formatGibMonths(usage.quantity, text);
    case BillingMeter.CLASS_A_RPC:
    case BillingMeter.CLASS_B_RPC:
      // The plural form follows the count; the shown digits stay exact for
      // an int64 count above 2^53.
      return text.plural("billing.calls", Number(usage.quantity), {
        count: formatCount(usage.quantity, text.lang),
      });
    default:
      return formatCount(usage.quantity, text.lang);
  }
}

/** The label of the cost of the UTC calendar month of a timestamp. */
function monthCostLabel(ts: Timestamp | undefined, text: Text): string {
  if (ts === undefined) return text.t("billing.costThisMonth");
  const month = new Date(Number(ts.seconds) * 1000).toLocaleDateString(
    text.lang,
    { month: "long", year: "numeric", timeZone: "UTC" },
  );
  return text.t("billing.costInMonth", { month });
}

function AccountsLink() {
  return (
    <a href={ACCOUNTS_URL} target="_blank" rel="noreferrer">
      {ACCOUNTS_HOST}
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
  const i18n = useI18n();
  const { t, lang, rich } = i18n;
  const queryClient = useQueryClient();
  const status = useQuery(BillingService.method.getBillingStatus, {
    organizationId,
  });
  const linked = status.data?.billing?.linked;
  const billingAccountId = status.data?.billingAccountId;
  const usage = useQuery(
    BillingService.method.getBillingUsage,
    linked === true ? { organizationId } : skipToken,
  );
  const usageAccountMismatch =
    Boolean(billingAccountId) &&
    usage.data !== undefined &&
    usage.data.billingAccountId !== billingAccountId;
  const unlink = useMutation(BillingService.method.unlinkBillingAccount, {
    onSuccess: () => invalidateServices(queryClient, BillingService),
    onError: (error) => {
      if (error.code === Code.FailedPrecondition) {
        return invalidateServices(queryClient, BillingService);
      }
    },
  });
  const [linking, setLinking] = useState(false);
  const [unlinkTarget, setUnlinkTarget] = useState<{
    id: string;
    /** The account name, when the loaded usage names the linked account. */
    name: string | undefined;
  }>();
  const owner = can(role, Permission.ManageBillingLink);

  let body;
  if (status.isPending) {
    body = <Loading label={t("billing.loadingStatus")} />;
  } else if (status.data === undefined || showsFullError(status)) {
    body = (
      <ErrorNotice error={status.error} onRetry={() => void status.refetch()} />
    );
  } else if (!linked) {
    body = owner ? (
      <div className="form">
        <p className="callout">{t("billing.unlinkedOwner")}</p>
        <div className="actions">
          <Button onClick={() => setLinking(true)}>
            {t("billingBanner.linkAccount")}
          </Button>
        </div>
      </div>
    ) : (
      <p className="callout">{t("billing.unlinkedAdmin")}</p>
    );
  } else {
    // Linked: the usage portion depends on `GetBillingUsage`, but the owner's
    // unlink action must not, so a loading or failed usage request keeps it.
    let usageBody;
    if (usage.isPending || (usageAccountMismatch && usage.isFetching)) {
      usageBody = <Loading label={t("billing.loadingUsage")} />;
    } else if (
      usageAccountMismatch ||
      usage.data === undefined ||
      showsFullError(usage)
    ) {
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
            <Detail term={t("billing.account")}>
              {data.billingAccountName}
            </Detail>
            <Detail term={t("billing.balance")}>
              {data.balanceUsdMicros < 0n
                ? t("billing.balanceDebt", {
                    amount: formatUsdMicros(data.balanceUsdMicros, lang),
                  })
                : formatUsdMicros(data.balanceUsdMicros, lang)}
            </Detail>
            <Detail term={monthCostLabel(data.monthStart, i18n)}>
              {formatUsdMicros(total, lang)}
            </Detail>
          </dl>
          <div className="table-wrap">
            <table className="table" aria-label={t("billing.usageTable")}>
              <thead>
                <tr>
                  <th scope="col">{t("billing.meter")}</th>
                  <th scope="col">{t("billing.usage")}</th>
                  <th scope="col">{t("billing.cost")}</th>
                </tr>
              </thead>
              <tbody>
                {data.meterUsages.map((u) => (
                  <tr key={u.meter}>
                    <td>{enumText(i18n, labels.billingMeter, u.meter)}</td>
                    <td>{meterQuantity(u, i18n)}</td>
                    <td>{formatUsageCostUsdMicros(u.amountUsdMicros, lang)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted">{t("billing.usageNote")}</p>
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
              disabled={!billingAccountId || unlink.isPending || status.isError}
              onClick={() => {
                if (!billingAccountId) return;
                const name =
                  !usage.isError &&
                  usage.data?.billingAccountId === billingAccountId
                    ? usage.data.billingAccountName
                    : undefined;
                setUnlinkTarget({ id: billingAccountId, name });
              }}
            >
              {t("billing.unlink")}
            </Button>
          </div>
        ) : null}
      </div>
    );
  }

  return (
    <section className="section" aria-labelledby="billing-title">
      <h2 id="billing-title">{t("billing.title")}</h2>
      <p className="muted">
        {rich("billing.description", { site: <AccountsLink /> })}
      </p>
      {status.error && !showsFullError(status) ? (
        <RefreshFailedNotice query={status} />
      ) : null}
      {usage.error && !usageAccountMismatch && !showsFullError(usage) ? (
        <RefreshFailedNotice query={usage} />
      ) : null}
      {body}
      {owner ? (
        <>
          <LinkBillingDialog open={linking} onOpenChange={setLinking} />
          <ConfirmDialog
            open={unlinkTarget !== undefined}
            onOpenChange={(open) => {
              if (!open) setUnlinkTarget(undefined);
            }}
            title={
              unlinkTarget?.name === undefined
                ? t("billing.unlinkDialog.titleUnnamed")
                : t("billing.unlinkDialog.title", { name: unlinkTarget.name })
            }
            description={t("billing.unlinkDialog.description")}
            confirmLabel={t("billing.unlink")}
            onConfirm={async (opening) => {
              if (unlinkTarget === undefined) return;
              try {
                await unlink.mutateAsync({
                  organizationId,
                  expectedBillingAccountId: unlinkTarget.id,
                });
              } catch (error) {
                if (ConnectError.from(error).code !== Code.FailedPrecondition) {
                  throw error;
                }
                if (opening.isCurrent()) {
                  toast.error(t("billing.unlinkDialog.accountChanged"));
                }
              }
            }}
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
  const i18n = useI18n();
  const { t, rich } = i18n;
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
  const didFocusFirst = useRef(false);
  const focusedRadio = useRef<HTMLElement | null>(null);
  const fullError = showsFullError(accounts);
  const list =
    accounts.isPending || fullError
      ? undefined
      : accounts.data?.billingAccounts;
  const selectedId = list?.find(
    (account) => account.billingAccountId === selected,
  )?.billingAccountId;
  // Keep the choice while the list is hidden. Clear it when the current list omits it.
  if (list !== undefined && selected !== null && selectedId === undefined) {
    setSelected(null);
  }

  useEffect(() => {
    if (!open) didFocusFirst.current = false;
    const options = listRef.current;
    if (!open || !list?.length || options === null) {
      focusedRadio.current = null;
      return;
    }

    // The first response can arrive after the dialog has set its initial focus.
    if (!didFocusFirst.current) {
      const first = options.querySelector<HTMLElement>('[role="radio"]');
      if (first) {
        didFocusFirst.current = true;
        first.focus();
      }
      return;
    }

    const previous = focusedRadio.current;
    if (previous === null || previous.isConnected) return;
    focusedRadio.current = null;
    const { activeElement, body } = options.ownerDocument;
    if (activeElement === body) {
      const fallback =
        options.querySelector<HTMLElement>(
          '[role="radio"][aria-checked="true"]',
        ) ?? options.querySelector<HTMLElement>('[role="radio"]');
      fallback?.focus();
    }
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
    content = <Loading label={t("billing.linkDialog.loading")} />;
  } else if (accounts.data === undefined || fullError) {
    content = (
      <ErrorNotice
        error={accounts.error}
        onRetry={() => void accounts.refetch()}
      />
    );
  } else if (list === undefined || list.length === 0) {
    content = (
      <p className="callout">
        {rich("billing.linkDialog.none", { site: <AccountsLink /> })}
      </p>
    );
  } else {
    content = (
      <div
        ref={listRef}
        onFocus={(event) => {
          focusedRadio.current =
            event.target.closest<HTMLElement>('[role="radio"]');
        }}
        onBlur={(event) => {
          if (event.relatedTarget !== null) {
            focusedRadio.current = null;
            return;
          }
          const radio = focusedRadio.current;
          if (radio === null) return;
          // A blur without a new target can occur while React removes the option.
          queueMicrotask(() => {
            if (
              focusedRadio.current === radio &&
              radio.isConnected &&
              radio.ownerDocument.activeElement !== radio
            ) {
              focusedRadio.current = null;
            }
          });
        }}
      >
        <RadioGroup
          label={t("billing.account")}
          value={selectedId ?? null}
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
              {t("billing.linkDialog.accountOption", {
                name: a.name,
                role: enumText(i18n, labels.billingAccountRole, a.role),
              })}
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
      title={t("billingBanner.linkAccount")}
      description={t("billing.linkDialog.description", { name })}
      submitLabel={t("billing.linkDialog.submit")}
      submitDisabled={list === undefined || list.length === 0}
      onSubmit={async (opening) => {
        if (selectedId === undefined) {
          setSelectionError(t("billing.linkDialog.chooseAccount"));
          return;
        }
        const res = await link.mutateAsync({
          organizationId,
          billingAccountId: selectedId,
        });
        // The success is real for a dismissed opening too, so its toast
        // stays; only the close of a later opening is guarded.
        if (opening.isCurrent()) close(false);
        toast.success(
          t("billing.linkDialog.linked", { name: res.billingAccountName }),
        );
      }}
    >
      {accounts.error && !fullError ? (
        <RefreshFailedNotice query={accounts} />
      ) : null}
      {content}
    </FormDialog>
  );
}
