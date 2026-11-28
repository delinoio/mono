// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge, BadgeVariant } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../../api.ts";
import { useActionFailure } from "../../components/action-failure.tsx";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  RefetchNotice,
  FormDialog,
  formInvalid,
  formSubmitted,
  Loading,
} from "../../components/ui.tsx";
import {
  checkTopUpAmount,
  TOP_UP_MAX_USD_MICROS,
  TOP_UP_MIN_USD_MICROS,
} from "../../forms.ts";
import { formatDate, labels } from "../../format.ts";
import {
  CreditService,
  type Subscription,
  SubscriptionStatus,
} from "@delino/accounts-protos/v1/credit_pb";
import { useI18n } from "../../i18n.ts";
import { formatUsd, usd } from "../../money.ts";
import { useCheckout } from "../../paddle.tsx";
import { can, Permission } from "../../roles.ts";
import { uuidV7 } from "../../uuid.ts";

/**
 * How often the overview reads a `PENDING` subscription again. A subscription
 * leaves `PENDING` only when the verified payment webhook reaches the server,
 * which the page cannot observe except by reading the subscription.
 */
export const PENDING_SUBSCRIPTION_POLL_MS = 2000;

/**
 * The React Query `refetchInterval` of the subscription read: poll only while
 * the cached subscription is `PENDING`. React Query pauses the interval while
 * the tab is in the background (`refetchIntervalInBackground` is off), stops
 * it when the status changes and drops it when the component unmounts.
 */
function pendingSubscriptionInterval(query: {
  state: { data?: { subscription?: Subscription } };
}): number | false {
  return query.state.data?.subscription?.status === SubscriptionStatus.PENDING
    ? PENDING_SUBSCRIPTION_POLL_MS
    : false;
}

/** The subscription price when the server does not send one. */
const DEFAULT_PRICE_USD_MICROS = usd(10n);

/** The overview tab: balance, subscription and credit grants. */
export function OverviewSection() {
  return (
    <div className="stack">
      <BalanceCard />
      <SubscriptionCard />
      <CreditGrants />
    </div>
  );
}

function BalanceCard() {
  const { billingAccountId, role } = useBillingAccount();
  const balance = useQuery(CreditService.method.getBalance, {
    billingAccountId,
  });
  const subscription = useQuery(CreditService.method.getSubscription, {
    billingAccountId,
  });
  const [toppingUp, setToppingUp] = useState(false);
  const { lang, t } = useI18n();
  const active =
    subscription.data?.subscription?.status === SubscriptionStatus.ACTIVE;
  const canPay = can(role, Permission.ManagePayments);
  return (
    <section className="section" aria-labelledby="balance-title">
      <h2 id="balance-title">{t("balance.title")}</h2>
      <RefetchNotice
        error={balance.isRefetchError ? balance.error : undefined}
        onRetry={() => void balance.refetch()}
      />
      {balance.isPending ? (
        <Loading label={t("balance.loading")} />
      ) : balance.isLoadingError ? (
        <ErrorNotice
          error={balance.error}
          onRetry={() => void balance.refetch()}
        />
      ) : (
        <>
          <p
            className={
              balance.data.balanceUsdMicros < 0n
                ? "balance balance-debt"
                : "balance"
            }
          >
            {formatUsd(balance.data.balanceUsdMicros, lang)}
          </p>
          {balance.data.balanceUsdMicros < 0n ? (
            <p className="muted">{t("balance.negative")}</p>
          ) : null}
        </>
      )}
      {canPay ? (
        <div className="actions">
          <Button disabled={!active} onClick={() => setToppingUp(true)}>
            {t("balance.topUp")}
          </Button>
          {!active && subscription.data !== undefined ? (
            <span className="muted">{t("balance.topUpNeedsSubscription")}</span>
          ) : null}
        </div>
      ) : null}
      {canPay ? (
        <TopUpDialog open={toppingUp} onOpenChange={setToppingUp} />
      ) : null}
    </section>
  );
}

/**
 * The manual top-up dialog. It generates one UUID v7 idempotency key each
 * time it opens and sends that key with every retry of the same top-up, so a
 * retry after a lost response does not charge twice.
 */
function TopUpDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { billingAccountId } = useBillingAccount();
  const queryClient = useQueryClient();
  const topUp = useMutation(CreditService.method.createTopUp);
  const i18n = useI18n();
  const { lang, t } = i18n;
  const [amount, setAmount] = useState("25.00");
  const [error, setError] = useState<string>();
  const [idempotencyKey, setIdempotencyKey] = useState("");
  // Adjust state while rendering: a new key for each opening of the dialog.
  const [wasOpen, setWasOpen] = useState(false);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setIdempotencyKey(uuidV7());
      setAmount("25.00");
      setError(undefined);
    }
  }
  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t("topUp.title")}
      description={t("topUp.description", {
        min: formatUsd(TOP_UP_MIN_USD_MICROS, lang),
        max: formatUsd(TOP_UP_MAX_USD_MICROS, lang),
      })}
      submitLabel={t("topUp.submit")}
      onSubmit={async () => {
        const checked = checkTopUpAmount(amount, i18n);
        if (!checked.ok) {
          setError(checked.error);
          return formInvalid();
        }
        setError(undefined);
        await topUp.mutateAsync({
          billingAccountId,
          idempotencyKey,
          amountUsdMicros: checked.value,
        });
        // The balance refreshes for every accepted top-up, and the dialog
        // does not wait for the refetch. The close and the toast belong to
        // the opening that sent the top-up.
        void invalidateServices(queryClient, CreditService);
        return formSubmitted(() => {
          onOpenChange(false);
          toast.success(
            t("topUp.accepted", { amount: formatUsd(checked.value, lang) }),
          );
        });
      }}
    >
      <TextField
        label={t("topUp.amount")}
        value={amount}
        // The key stays the same when the amount changes: after a request
        // whose result is unknown, the server rejects the same key with
        // another amount instead of charging a second time.
        onChange={(event) => setAmount(event.target.value)}
        error={error}
        autoFocus
        inputMode="decimal"
        autoComplete="off"
      />
    </FormDialog>
  );
}

function SubscriptionBadge({ subscription }: { subscription: Subscription }) {
  const { t } = useI18n();
  const variant =
    subscription.status === SubscriptionStatus.ACTIVE
      ? BadgeVariant.Accent
      : subscription.status === SubscriptionStatus.PAST_DUE
        ? BadgeVariant.Danger
        : BadgeVariant.Neutral;
  return (
    <Badge variant={variant}>
      {t(labels.subscriptionStatus[subscription.status])}
    </Badge>
  );
}

function SubscriptionCard() {
  const { billingAccountId, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const checkout = useCheckout();
  const actionFailure = useActionFailure();
  const { lang, t } = useI18n();
  const subscription = useQuery(
    CreditService.method.getSubscription,
    { billingAccountId },
    { refetchInterval: pendingSubscriptionInterval },
  );
  const refresh = () => invalidateServices(queryClient, CreditService);

  // The payment webhook confirms the subscription and adds the first credit
  // grant on the server. When the polled status leaves PENDING, read the
  // balance and the credit grants again so that Top up and the grants update
  // without a reload. This runs once per transition: the previous status is
  // kept per billing account, so switching accounts and a read that has no
  // data yet never count as a transition.
  const polledStatus = subscription.data?.subscription?.status;
  const previous = useRef<
    { accountId: string; status?: SubscriptionStatus } | undefined
  >(undefined);
  useEffect(() => {
    const last = previous.current;
    previous.current = { accountId: billingAccountId, status: polledStatus };
    if (
      last?.accountId === billingAccountId &&
      last.status === SubscriptionStatus.PENDING &&
      polledStatus !== undefined &&
      polledStatus !== SubscriptionStatus.PENDING
    ) {
      void invalidateServices(queryClient, CreditService);
    }
  }, [billingAccountId, polledStatus, queryClient]);
  const start = useMutation(CreditService.method.startSubscription);
  const cancel = useMutation(CreditService.method.cancelSubscription, {
    onSuccess: refresh,
  });
  const resume = useMutation(CreditService.method.resumeSubscription, {
    onSuccess: refresh,
  });
  const [canceling, setCanceling] = useState(false);
  const [startError, setStartError] = useState<unknown>();
  const canPay = can(role, Permission.ManagePayments);
  const cancelOpen = canPay && canceling;
  if (canceling && !cancelOpen) setCanceling(false);

  // One controller per mounted view of this billing account. Leaving the
  // view aborts it, so a subscription start whose response arrives later does
  // not open a checkout for an account the user no longer looks at. The
  // controller is created in the effect so that StrictMode's extra
  // setup and cleanup leaves a live controller for the mounted view.
  const viewAbort = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    const controller = new AbortController();
    viewAbort.current = controller;
    return () => controller.abort();
  }, [billingAccountId]);

  const startCheckout = async () => {
    setStartError(undefined);
    if (checkout === undefined) return;
    const signal = viewAbort.current?.signal;
    try {
      const res = await start.mutateAsync({ billingAccountId });
      // A stale request still refreshes the reads of its own account.
      await refresh();
      if (signal?.aborted) return;
      // The subscription stays PENDING until the verified payment webhook,
      // so the page reads it again when the checkout closes.
      await checkout.open(
        res.paddleTransactionId,
        () => void refresh(),
        signal,
        lang,
      );
    } catch (error) {
      if (!signal?.aborted) setStartError(error);
    }
  };

  if (subscription.isPending) {
    return (
      <section className="section" aria-label={t("subscription.title")}>
        <Loading label={t("subscription.loading")} />
      </section>
    );
  }
  if (subscription.isLoadingError) {
    return (
      <section className="section" aria-label={t("subscription.title")}>
        <ErrorNotice
          error={subscription.error}
          onRetry={() => void subscription.refetch()}
        />
      </section>
    );
  }
  const sub = subscription.data.subscription;
  const status = sub?.status ?? SubscriptionStatus.NONE;
  const price =
    sub !== undefined && sub.priceUsdMicros > 0n
      ? sub.priceUsdMicros
      : DEFAULT_PRICE_USD_MICROS;
  const current =
    status === SubscriptionStatus.ACTIVE ||
    status === SubscriptionStatus.PAST_DUE;
  const startable =
    status === SubscriptionStatus.NONE ||
    status === SubscriptionStatus.CANCELED ||
    status === SubscriptionStatus.PENDING ||
    status === SubscriptionStatus.UNSPECIFIED;

  return (
    <section className="section" aria-labelledby="subscription-title">
      <RefetchNotice
        error={subscription.isRefetchError ? subscription.error : undefined}
        onRetry={() => void subscription.refetch()}
      />
      <div className="section-header">
        <h2 id="subscription-title">{t("subscription.title")}</h2>
        {sub !== undefined ? <SubscriptionBadge subscription={sub} /> : null}
      </div>
      <p className="muted">
        {t("subscription.price", { price: formatUsd(price, lang) })}
      </p>
      {status === SubscriptionStatus.PENDING ? (
        <p role="status">{t("subscription.pending")}</p>
      ) : null}
      {status === SubscriptionStatus.PAST_DUE ? (
        <p role="status">{t("subscription.pastDue")}</p>
      ) : null}
      {current && sub?.currentPeriodEnd !== undefined ? (
        <p>
          {t(
            sub.cancelAtPeriodEnd
              ? "subscription.endsOn"
              : "subscription.renewsOn",
            { date: formatDate(sub.currentPeriodEnd, lang) },
          )}
        </p>
      ) : null}
      {startError !== undefined ? <ErrorNotice error={startError} /> : null}
      {actionFailure.notice}
      {canPay ? (
        <div className="actions">
          {startable ? (
            checkout === undefined ? (
              <p role="alert">{t("subscription.checkoutNotConfigured")}</p>
            ) : (
              <Button onClick={startCheckout}>
                {t(
                  status === SubscriptionStatus.PENDING
                    ? "subscription.continueCheckout"
                    : "subscription.start",
                )}
              </Button>
            )
          ) : null}
          {current && !sub!.cancelAtPeriodEnd ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setCanceling(true)}
            >
              {t("subscription.cancel")}
            </Button>
          ) : null}
          {current && sub!.cancelAtPeriodEnd ? (
            <Button
              onClick={async () => {
                try {
                  await resume.mutateAsync({ billingAccountId });
                  actionFailure.clear();
                  toast.success(t("subscription.resumed"));
                } catch (error) {
                  actionFailure.report(error);
                }
              }}
            >
              {t("subscription.resume")}
            </Button>
          ) : null}
        </div>
      ) : null}
      <ConfirmDialog
        open={cancelOpen}
        onOpenChange={setCanceling}
        title={t("subscription.cancelDialog.title")}
        description={
          sub?.currentPeriodEnd !== undefined
            ? t("subscription.cancelDialog.descriptionWithDate", {
                date: formatDate(sub.currentPeriodEnd, lang),
              })
            : t("subscription.cancelDialog.description")
        }
        confirmLabel={t("subscription.cancel")}
        onConfirm={async () => {
          if (!cancelOpen) return;
          await cancel.mutateAsync({ billingAccountId });
        }}
      />
    </section>
  );
}

function CreditGrants() {
  const { billingAccountId } = useBillingAccount();
  const balance = useQuery(CreditService.method.getBalance, {
    billingAccountId,
  });
  const { lang, t, rich } = useI18n();
  return (
    <section className="section" aria-labelledby="grants-title">
      <h2 id="grants-title">{t("credit.title")}</h2>
      <p className="muted">{t("credit.description")}</p>
      <RefetchNotice
        error={balance.isRefetchError ? balance.error : undefined}
        onRetry={() => void balance.refetch()}
      />
      {balance.isPending ? (
        <Loading label={t("credit.loading")} />
      ) : balance.isLoadingError ? (
        <ErrorNotice
          error={balance.error}
          onRetry={() => void balance.refetch()}
        />
      ) : balance.data.creditGrants.length === 0 ? (
        <Empty title={t("credit.empty")} />
      ) : (
        <ul className="list" aria-label={t("credit.list")}>
          {balance.data.creditGrants.map((grant) => (
            <li key={grant.creditGrantId} className="list-row">
              <div className="list-main">
                <span className="list-title">
                  {t(labels.ledgerKind[grant.source])}
                </span>
                <span className="muted">
                  {t("credit.dates", {
                    bought: formatDate(grant.createTime, lang),
                    expires: formatDate(grant.expireTime, lang),
                  })}
                </span>
              </div>
              <span className="amount">
                {rich("credit.remainingOf", {
                  remaining: formatUsd(grant.remainingUsdMicros, lang),
                  original: (
                    <span className="muted">
                      {formatUsd(grant.originalUsdMicros, lang)}
                    </span>
                  ),
                })}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
