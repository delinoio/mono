// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge, BadgeVariant } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { useEffect, useRef, useState } from "react";
import { invalidateServices } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
} from "../../components/ui.tsx";
import {
  checkTopUpAmount,
  TOP_UP_MAX_USD_MICROS,
  TOP_UP_MIN_USD_MICROS,
} from "../../forms.ts";
import { errorMessage, formatDate, labels } from "../../format.ts";
import {
  CreditService,
  type Subscription,
  SubscriptionStatus,
} from "@delino/accounts-protos/v1/credit_pb";
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
  const active =
    subscription.data?.subscription?.status === SubscriptionStatus.ACTIVE;
  const canPay = can(role, Permission.ManagePayments);
  return (
    <section className="section" aria-labelledby="balance-title">
      <h2 id="balance-title">Balance</h2>
      {balance.isPending ? (
        <Loading label="Loading the balance" />
      ) : balance.error ? (
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
            {formatUsd(balance.data.balanceUsdMicros)}
          </p>
          {balance.data.balanceUsdMicros < 0n ? (
            <p className="muted">
              The balance is negative. The next credit pays off the debt first.
            </p>
          ) : null}
        </>
      )}
      {canPay ? (
        <div className="actions">
          <Button disabled={!active} onClick={() => setToppingUp(true)}>
            Top up
          </Button>
          {!active && subscription.data !== undefined ? (
            <span className="muted">Top-ups need an active subscription.</span>
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
      title="Top up credit"
      description={`Charge the subscription's payment method once. Enter ${formatUsd(TOP_UP_MIN_USD_MICROS)} to ${formatUsd(TOP_UP_MAX_USD_MICROS)}; tax is added on top. The credit appears when Paddle confirms the payment.`}
      submitLabel="Top up"
      onSubmit={async () => {
        const checked = checkTopUpAmount(amount);
        if (!checked.ok) {
          setError(checked.error);
          return;
        }
        setError(undefined);
        await topUp.mutateAsync({
          billingAccountId,
          idempotencyKey,
          amountUsdMicros: checked.value,
        });
        onOpenChange(false);
        toast.success(
          `Paddle accepted the ${formatUsd(checked.value)} top-up. The credit appears when the payment is confirmed.`,
        );
        await invalidateServices(queryClient, CreditService);
      }}
    >
      <TextField
        label="Amount (USD)"
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

function subscriptionBadge(subscription: Subscription) {
  const variant =
    subscription.status === SubscriptionStatus.ACTIVE
      ? BadgeVariant.Accent
      : subscription.status === SubscriptionStatus.PAST_DUE
        ? BadgeVariant.Danger
        : BadgeVariant.Neutral;
  return (
    <Badge variant={variant}>
      {labels.subscriptionStatus[subscription.status]}
    </Badge>
  );
}

function SubscriptionCard() {
  const { billingAccountId, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const checkout = useCheckout();
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

  const startCheckout = async () => {
    setStartError(undefined);
    if (checkout === undefined) return;
    try {
      const res = await start.mutateAsync({ billingAccountId });
      await refresh();
      // The subscription stays PENDING until the verified payment webhook,
      // so the page reads it again when the checkout closes.
      await checkout.open(res.paddleTransactionId, () => void refresh());
    } catch (error) {
      setStartError(error);
    }
  };

  if (subscription.isPending) {
    return (
      <section className="section" aria-label="Subscription">
        <Loading label="Loading the subscription" />
      </section>
    );
  }
  if (subscription.error) {
    return (
      <section className="section" aria-label="Subscription">
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
      <div className="section-header">
        <h2 id="subscription-title">Subscription</h2>
        {sub !== undefined ? subscriptionBadge(sub) : null}
      </div>
      <p className="muted">
        {formatUsd(price)} per month. Each payment adds {formatUsd(price)} of
        credit. Services need an active subscription.
      </p>
      {status === SubscriptionStatus.PENDING ? (
        <p role="status">
          Waiting for Paddle to confirm the first payment. This page updates
          when the payment is confirmed. If you closed the checkout before
          paying, continue the checkout.
        </p>
      ) : null}
      {status === SubscriptionStatus.PAST_DUE ? (
        <p role="status">
          A renewal payment failed, and Paddle retries it. Check the payment
          method in the email from Paddle.
        </p>
      ) : null}
      {current && sub?.currentPeriodEnd !== undefined ? (
        <p>
          {sub.cancelAtPeriodEnd
            ? `Ends on ${formatDate(sub.currentPeriodEnd)}.`
            : `Renews on ${formatDate(sub.currentPeriodEnd)}.`}
        </p>
      ) : null}
      {startError !== undefined ? <ErrorNotice error={startError} /> : null}
      {canPay ? (
        <div className="actions">
          {startable ? (
            checkout === undefined ? (
              <p role="alert">Checkout is not configured for this build.</p>
            ) : (
              <Button onClick={startCheckout}>
                {status === SubscriptionStatus.PENDING
                  ? "Continue checkout"
                  : "Start subscription"}
              </Button>
            )
          ) : null}
          {current && !sub!.cancelAtPeriodEnd ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setCanceling(true)}
            >
              Cancel subscription
            </Button>
          ) : null}
          {current && sub!.cancelAtPeriodEnd ? (
            <Button
              onClick={async () => {
                try {
                  await resume.mutateAsync({ billingAccountId });
                  toast.success("The subscription continues.");
                } catch (error) {
                  toast.error(errorMessage(error));
                }
              }}
            >
              Resume subscription
            </Button>
          ) : null}
        </div>
      ) : null}
      <ConfirmDialog
        open={canceling}
        onOpenChange={setCanceling}
        title="Cancel the subscription?"
        description={
          sub?.currentPeriodEnd !== undefined
            ? `It stays active until ${formatDate(sub.currentPeriodEnd)}. After that, linked organizations cannot use paid features. You can resume it before then.`
            : "It stays active until the end of the paid period. After that, linked organizations cannot use paid features."
        }
        confirmLabel="Cancel subscription"
        onConfirm={() => cancel.mutateAsync({ billingAccountId })}
      />
    </section>
  );
}

function CreditGrants() {
  const { billingAccountId } = useBillingAccount();
  const balance = useQuery(CreditService.method.getBalance, {
    billingAccountId,
  });
  return (
    <section className="section" aria-labelledby="grants-title">
      <h2 id="grants-title">Credit</h2>
      <p className="muted">
        Credit expires 12 months after purchase. Usage takes the credit that
        expires first.
      </p>
      {balance.isPending ? (
        <Loading label="Loading credit" />
      ) : balance.error ? (
        <ErrorNotice
          error={balance.error}
          onRetry={() => void balance.refetch()}
        />
      ) : balance.data.creditGrants.length === 0 ? (
        <Empty title="No credit left" />
      ) : (
        <ul className="list" aria-label="Credit grants">
          {balance.data.creditGrants.map((grant) => (
            <li key={grant.creditGrantId} className="list-row">
              <div className="list-main">
                <span className="list-title">
                  {labels.ledgerKind[grant.source]}
                </span>
                <span className="muted">
                  Bought {formatDate(grant.createTime)} · Expires{" "}
                  {formatDate(grant.expireTime)}
                </span>
              </div>
              <span className="amount">
                {formatUsd(grant.remainingUsdMicros)}
                <span className="muted">
                  {" "}
                  of {formatUsd(grant.originalUsdMicros)}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
