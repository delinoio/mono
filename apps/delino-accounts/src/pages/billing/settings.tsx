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
  RefetchNotice,
  Loading,
} from "../../components/ui.tsx";
import {
  type AutoTopUpErrors,
  type AutoTopUpInput,
  type AutoTopUpValues,
  checkAccountName,
  checkAlertThreshold,
  checkAutoTopUp,
  DEFAULT_ALERT_THRESHOLD_USD_MICROS,
} from "../../forms.ts";
import { formatCount } from "../../format.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import {
  type AutoTopUpSettings,
  CreditService,
} from "@delino/accounts-protos/v1/credit_pb";
import { BillingUsageService } from "@delino/accounts-protos/v1/usage_pb";
import { formatUsd, toAmountInput } from "../../money.ts";
import { can, Permission } from "../../roles.ts";
import { parseRoute, RouteId, routePath, useRouter } from "../../router.tsx";

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
  const [draft, setDraft] = useState<{ value: string }>();
  const [savedName, setSavedName] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const editable = can(role, Permission.ManageMembers);
  const awaitingSavedName = savedName !== undefined && savedName !== name;
  if (savedName === name) setSavedName(undefined);
  // While saving or waiting for the saved name to arrive, the fetched name
  // can be stale. A draft that matches it may still be an unsaved edit.
  if (!saving && !awaitingSavedName && draft?.value === name) {
    setDraft(undefined);
  }
  const value = draft?.value ?? savedName ?? name;
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
        setSaving(true);
        try {
          await update.mutateAsync({ billingAccountId, name: checked.value });
          setSavedName(checked.value);
          // Each edit creates a new draft, even if the user types the same text.
          setDraft((latest) => (latest === draft ? undefined : latest));
          await invalidateServices(queryClient, BillingAccountService);
          toast.success("The name is saved.");
        } finally {
          setSaving(false);
        }
      }}
    >
      <TextField
        label="Billing account name"
        value={value}
        onChange={(event) => setDraft({ value: event.target.value })}
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
  ) : settings.isLoadingError ? (
    <section className="section" aria-label="Automatic top-up">
      <ErrorNotice
        error={settings.error}
        onRetry={() => void settings.refetch()}
      />
    </section>
  ) : (
    // The notice and the form are siblings in a fragment in every loaded
    // state, so that a failed refetch does not remount the form and discard
    // its draft.
    <>
      <RefetchNotice
        error={settings.isRefetchError ? settings.error : undefined}
        onRetry={() => void settings.refetch()}
      />
      <AutoTopUpForm settings={settings.data.settings} />
    </>
  );
}

/** Compares saved settings without the read-only counters. */
function autoTopUpSettingsKey(settings: AutoTopUpValues | undefined): string {
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

function autoTopUpDraft(settings: AutoTopUpValues | undefined): AutoTopUpInput {
  return {
    enabled: settings?.enabled ?? false,
    threshold: amountText(settings?.thresholdUsdMicros),
    amount: amountText(settings?.amountUsdMicros),
    monthlyCap: amountText(settings?.monthlyCapUsdMicros),
  };
}

interface AutoTopUpFormState {
  settingsKey: string;
  confirmationKey?: string;
  draft: AutoTopUpInput;
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
  const settingsKey = autoTopUpSettingsKey(settings);
  const [form, setForm] = useState<AutoTopUpFormState>(() => ({
    settingsKey,
    draft: autoTopUpDraft(settings),
  }));
  const [errors, setErrors] = useState<AutoTopUpErrors>({});
  if (form.settingsKey !== settingsKey) {
    // A read that confirms this form's save keeps the current draft.
    if (settingsKey === form.confirmationKey) {
      setForm({ settingsKey, draft: form.draft });
    } else {
      setForm({ settingsKey, draft: autoTopUpDraft(settings) });
      setErrors({});
    }
  }
  const { draft } = form;
  const editDraft = (edit: Partial<AutoTopUpInput>) => {
    setForm((current) => ({
      ...current,
      draft: { ...current.draft, ...edit },
    }));
  };

  return (
    <SettingsForm
      title="Automatic top-up"
      id="auto-top-up-title"
      description="Buy credit automatically when a usage charge makes the balance fall below the threshold. The monthly cap limits automatic top-ups in one UTC calendar month. Automatic top-ups in progress count toward the cap. After 2 failed top-ups in a row, automatic top-up turns off."
      editable={editable}
      onSubmit={async () => {
        const checked = checkAutoTopUp(draft);
        if (!checked.ok) {
          setErrors(checked.errors);
          return;
        }
        setErrors({});
        const saved = await update.mutateAsync({
          billingAccountId,
          settings: checked.value,
        });
        const savedSettings = saved.settings ?? checked.value;
        const savedKey = autoTopUpSettingsKey(savedSettings);
        setForm((current) => ({
          ...current,
          confirmationKey:
            savedKey === current.settingsKey ? undefined : savedKey,
          // Each edit creates a new draft, even if it restores an earlier value.
          draft:
            current.draft === draft
              ? autoTopUpDraft(savedSettings)
              : current.draft,
        }));
        await invalidateServices(queryClient, CreditService);
        toast.success("Automatic top-up is saved.");
      }}
    >
      <Switch
        label="Automatic top-up"
        checked={draft.enabled}
        onCheckedChange={(enabled) => editDraft({ enabled })}
        disabled={!editable}
      />
      <TextField
        label="Threshold (USD)"
        description="At least $10.00."
        value={draft.threshold}
        onChange={(event) => editDraft({ threshold: event.target.value })}
        error={errors.threshold}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label="Top-up amount (USD)"
        description="At least $25.00. Tax is added on top."
        value={draft.amount}
        onChange={(event) => editDraft({ amount: event.target.value })}
        error={errors.amount}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label="Monthly cap (USD)"
        description="Required. At least the top-up amount."
        value={draft.monthlyCap}
        onChange={(event) => editDraft({ monthlyCap: event.target.value })}
        error={errors.monthlyCap}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <dl className="details">
        <Detail term="Monthly cap usage">
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
  ) : settings.isLoadingError ? (
    <section className="section" aria-label="Low-balance alert">
      <ErrorNotice
        error={settings.error}
        onRetry={() => void settings.refetch()}
      />
    </section>
  ) : (
    // A fragment in every loaded state; see AutoTopUpSection.
    <>
      <RefetchNotice
        error={settings.isRefetchError ? settings.error : undefined}
        onRetry={() => void settings.refetch()}
      />
      <AlertForm
        fetchedThreshold={
          settings.data.settings?.lowBalanceThresholdUsdMicros ??
          DEFAULT_ALERT_THRESHOLD_USD_MICROS
        }
        fetchedAt={settings.dataUpdatedAt}
      />
    </>
  );
}

function AlertForm({
  fetchedThreshold,
  fetchedAt,
}: {
  fetchedThreshold: bigint;
  fetchedAt: number;
}) {
  const { billingAccountId, role } = useBillingAccount();
  const queryClient = useQueryClient();
  const update = useMutation(CreditService.method.updateAlertSettings);
  const editable = can(role, Permission.ManagePayments);
  const [draft, setDraft] = useState<{ value: string }>();
  const [lastRead, setLastRead] = useState<{
    threshold: bigint;
    fetchedAt: number;
    savedThreshold?: bigint;
  }>({ threshold: fetchedThreshold, fetchedAt });
  const [error, setError] = useState<string>();
  if (
    lastRead.fetchedAt !== fetchedAt ||
    lastRead.threshold !== fetchedThreshold
  ) {
    // A successful read replaces the saved response without clearing the draft.
    setLastRead({ threshold: fetchedThreshold, fetchedAt });
  }
  const threshold =
    draft?.value ?? toAmountInput(lastRead.savedThreshold ?? fetchedThreshold);
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
        const saved = await update.mutateAsync({
          billingAccountId,
          settings: { lowBalanceThresholdUsdMicros: checked.value },
        });
        setLastRead((current) => ({
          ...current,
          savedThreshold:
            saved.settings?.lowBalanceThresholdUsdMicros ?? checked.value,
        }));
        // Each edit creates a separate draft, even if its text is the same.
        setDraft((latest) => (latest === draft ? undefined : latest));
        await invalidateServices(queryClient, CreditService);
        toast.success("The alert is saved.");
      }}
    >
      <TextField
        label="Alert threshold (USD)"
        description="At least $0.00. The default is $10.00."
        value={threshold}
        onChange={(event) => setDraft({ value: event.target.value })}
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
          // Read the live route after the RPC. Move off this account before
          // refreshing because the caller can no longer read it.
          const route = parseRoute(window.location.pathname);
          if (
            route.id === RouteId.BillingAccount &&
            route.billingAccountId === billingAccountId
          ) {
            navigate(routePath({ id: RouteId.Billing }));
          }
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
