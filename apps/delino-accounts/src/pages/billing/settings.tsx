// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Switch } from "delino-ui/switch";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { type FormEvent, type ReactNode, useState } from "react";
import { invalidateServices } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  Loading,
} from "../../components/ui.tsx";
import {
  type AutoTopUpErrors,
  checkAccountName,
  checkAlertThreshold,
  checkAutoTopUp,
  DEFAULT_ALERT_THRESHOLD_USD_MICROS,
} from "../../forms.ts";
import { formatCount } from "../../format.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import {
  type AlertSettings,
  type AutoTopUpSettings,
  CreditService,
} from "@delino/accounts-protos/v1/credit_pb";
import { BillingUsageService } from "@delino/accounts-protos/v1/usage_pb";
import { formatUsd, toAmountInput } from "../../money.ts";
import { can, Permission } from "../../roles.ts";
import { RouteId, routePath, useRouter } from "../../router.tsx";

const READ_ONLY_NOTE = "Only owners and admins can change these settings.";

/** The settings tab: name, automatic top-up, alerts and deletion. */
export function SettingsSection() {
  const { role } = useBillingAccount();
  return (
    <div className="stack">
      <NameSettings />
      <AutoTopUpSection />
      <AlertSection />
      {can(role, Permission.DeleteAccount) ? <DeleteSection /> : null}
    </div>
  );
}

/** A settings form; read-only for roles that may not change it. */
function SettingsForm({
  title,
  id,
  description,
  editable,
  onSubmit,
  children,
}: {
  title: string;
  id: string;
  description?: ReactNode;
  editable: boolean;
  onSubmit: () => Promise<unknown>;
  children: ReactNode;
}) {
  const [error, setError] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (pending) return;
    setPending(true);
    setError(undefined);
    try {
      await onSubmit();
    } catch (e) {
      setError(e);
    } finally {
      setPending(false);
    }
  };
  return (
    <section className="section" aria-labelledby={id}>
      <h2 id={id}>{title}</h2>
      {description ? <p className="muted">{description}</p> : null}
      <form className="form" onSubmit={submit} noValidate aria-labelledby={id}>
        {children}
        {error !== undefined ? <ErrorNotice error={error} /> : null}
        {editable ? (
          <div className="actions">
            <Button type="submit" disabled={pending}>
              Save
            </Button>
          </div>
        ) : (
          <p className="muted">{READ_ONLY_NOTE}</p>
        )}
      </form>
    </section>
  );
}

function NameSettings() {
  const { billingAccountId, name, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const update = useMutation(BillingAccountService.method.updateBillingAccount);
  const [value, setValue] = useState(name);
  const [error, setError] = useState<string>();
  const editable = can(role, Permission.ManageMembers);
  return (
    <SettingsForm
      title="Name"
      id="name-title"
      editable={editable}
      onSubmit={async () => {
        const checked = checkAccountName(value);
        if (!checked.ok) {
          setError(checked.error);
          return;
        }
        setError(undefined);
        await update.mutateAsync({ billingAccountId, name: checked.value });
        await invalidateServices(queryClient, BillingAccountService);
        toast.success("The name is saved.");
      }}
    >
      <TextField
        label="Billing account name"
        value={value}
        onChange={(event) => setValue(event.target.value)}
        error={error}
        disabled={!editable}
        autoComplete="off"
      />
    </SettingsForm>
  );
}

function AutoTopUpSection() {
  const { billingAccountId } = useBillingAccount();
  const settings = useQuery(CreditService.method.getAutoTopUpSettings, {
    billingAccountId,
  });
  return settings.isPending ? (
    <section className="section" aria-label="Automatic top-up">
      <Loading label="Loading automatic top-up" />
    </section>
  ) : settings.error ? (
    <section className="section" aria-label="Automatic top-up">
      <ErrorNotice
        error={settings.error}
        onRetry={() => void settings.refetch()}
      />
    </section>
  ) : (
    <AutoTopUpForm
      // Start again from the server values after each save. Only the four
      // editable values identify the form: the read-only counters change
      // without any user action, and a refetch that changes only them must
      // not discard the draft in the inputs.
      key={autoTopUpFormKey(settings.data.settings)}
      settings={settings.data.settings}
    />
  );
}

/** The identity of the saved editable values of the automatic top-up form. */
function autoTopUpFormKey(settings: AutoTopUpSettings | undefined): string {
  return [
    settings?.enabled ?? false,
    settings?.thresholdUsdMicros ?? 0n,
    settings?.amountUsdMicros ?? 0n,
    settings?.monthlyCapUsdMicros ?? 0n,
  ].join("|");
}

/** The input text of a saved amount; zero shows as an empty field. */
function amountText(micros: bigint | undefined): string {
  return micros === undefined || micros === 0n ? "" : toAmountInput(micros);
}

function AutoTopUpForm({
  settings,
}: {
  settings: AutoTopUpSettings | undefined;
}) {
  const { billingAccountId, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const update = useMutation(CreditService.method.updateAutoTopUpSettings);
  const editable = can(role, Permission.ManagePayments);
  const [enabled, setEnabled] = useState(settings?.enabled ?? false);
  const [threshold, setThreshold] = useState(
    amountText(settings?.thresholdUsdMicros),
  );
  const [amount, setAmount] = useState(amountText(settings?.amountUsdMicros));
  const [monthlyCap, setMonthlyCap] = useState(
    amountText(settings?.monthlyCapUsdMicros),
  );
  const [errors, setErrors] = useState<AutoTopUpErrors>({});
  return (
    <SettingsForm
      title="Automatic top-up"
      id="auto-top-up-title"
      description="Buy credit automatically when a usage charge makes the balance fall below the threshold. The monthly cap limits automatic top-ups in one UTC calendar month. After 2 failed top-ups in a row, automatic top-up turns off."
      editable={editable}
      onSubmit={async () => {
        const checked = checkAutoTopUp({
          enabled,
          threshold,
          amount,
          monthlyCap,
        });
        if (!checked.ok) {
          setErrors(checked.errors);
          return;
        }
        setErrors({});
        await update.mutateAsync({
          billingAccountId,
          settings: checked.value,
        });
        await invalidateServices(queryClient, CreditService);
        toast.success("Automatic top-up is saved.");
      }}
    >
      <Switch
        label="Automatic top-up"
        checked={enabled}
        onCheckedChange={setEnabled}
        disabled={!editable}
      />
      <TextField
        label="Threshold (USD)"
        description="At least $10.00."
        value={threshold}
        onChange={(event) => setThreshold(event.target.value)}
        error={errors.threshold}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label="Top-up amount (USD)"
        description="At least $25.00. Tax is added on top."
        value={amount}
        onChange={(event) => setAmount(event.target.value)}
        error={errors.amount}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label="Monthly cap (USD)"
        description="Required. At least the top-up amount."
        value={monthlyCap}
        onChange={(event) => setMonthlyCap(event.target.value)}
        error={errors.monthlyCap}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <dl className="details">
        <Detail term="Charged this month">
          {formatUsd(settings?.monthToDateUsdMicros ?? 0n)}
        </Detail>
        <Detail term="Failed top-ups in a row">
          {formatCount(BigInt(settings?.consecutiveFailures ?? 0))}
        </Detail>
      </dl>
    </SettingsForm>
  );
}

function AlertSection() {
  const { billingAccountId } = useBillingAccount();
  const settings = useQuery(CreditService.method.getAlertSettings, {
    billingAccountId,
  });
  return settings.isPending ? (
    <section className="section" aria-label="Low-balance alert">
      <Loading label="Loading the low-balance alert" />
    </section>
  ) : settings.error ? (
    <section className="section" aria-label="Low-balance alert">
      <ErrorNotice
        error={settings.error}
        onRetry={() => void settings.refetch()}
      />
    </section>
  ) : (
    <AlertForm
      key={String(settings.data.settings?.lowBalanceThresholdUsdMicros)}
      settings={settings.data.settings}
    />
  );
}

function AlertForm({ settings }: { settings: AlertSettings | undefined }) {
  const { billingAccountId, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const update = useMutation(CreditService.method.updateAlertSettings);
  const editable = can(role, Permission.ManagePayments);
  const [threshold, setThreshold] = useState(
    toAmountInput(
      settings?.lowBalanceThresholdUsdMicros ??
        DEFAULT_ALERT_THRESHOLD_USD_MICROS,
    ),
  );
  const [error, setError] = useState<string>();
  return (
    <SettingsForm
      title="Low-balance alert"
      id="alert-title"
      description="While automatic top-up is off, owners and admins get an email when the balance falls below this amount."
      editable={editable}
      onSubmit={async () => {
        const checked = checkAlertThreshold(threshold);
        if (!checked.ok) {
          setError(checked.error);
          return;
        }
        setError(undefined);
        await update.mutateAsync({
          billingAccountId,
          settings: { lowBalanceThresholdUsdMicros: checked.value },
        });
        await invalidateServices(queryClient, CreditService);
        toast.success("The alert is saved.");
      }}
    >
      <TextField
        label="Alert threshold (USD)"
        description="At least $0.00. The default is $10.00."
        value={threshold}
        onChange={(event) => setThreshold(event.target.value)}
        error={error}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
    </SettingsForm>
  );
}

function DeleteSection() {
  const { billingAccountId, name } = useBillingAccount();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const remove = useMutation(BillingAccountService.method.deleteBillingAccount);
  const [confirming, setConfirming] = useState(false);
  return (
    <section className="section danger-zone" aria-labelledby="delete-title">
      <h2 id="delete-title">Delete billing account</h2>
      <p className="muted">
        You can delete the billing account only when its balance is $0.00 or
        more and no organization is linked. Deletion cancels the subscription at
        once, and the remaining credit is not refunded.
      </p>
      <Button
        variant={ButtonVariant.Danger}
        onClick={() => setConfirming(true)}
      >
        Delete billing account
      </Button>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={`Delete ${name}?`}
        description="The subscription ends now, the remaining credit is lost and every member loses access. This cannot be undone."
        confirmLabel="Delete billing account"
        onConfirm={async () => {
          await remove.mutateAsync({ billingAccountId });
          // Leave the deleted account first, so that the invalidation does
          // not refetch its queries (they are now `not_found`).
          navigate(routePath({ id: RouteId.Billing }));
          await invalidateServices(
            queryClient,
            BillingAccountService,
            CreditService,
            BillingUsageService,
          );
        }}
      />
    </section>
  );
}
