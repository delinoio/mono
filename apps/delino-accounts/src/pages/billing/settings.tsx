// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useMutation,
  useQuery,
  useTransport,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Switch } from "delino-ui/switch";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import {
  type FormEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";
import { invalidateServices } from "../../api.ts";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  RefetchNotice,
  Loading,
} from "../../components/ui.tsx";
import { useCurrentUtcMonth } from "../../current-utc-month.ts";
import {
  type AutoTopUpErrors,
  type AutoTopUpInput,
  type AutoTopUpValues,
  AUTO_TOP_UP_MIN_AMOUNT_USD_MICROS,
  AUTO_TOP_UP_MIN_THRESHOLD_USD_MICROS,
  checkAccountName,
  checkAlertThreshold,
  checkAutoTopUp,
  DEFAULT_ALERT_THRESHOLD_USD_MICROS,
  TOP_UP_MAX_USD_MICROS,
} from "../../forms.ts";
import { formatCount } from "../../format.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import {
  type AutoTopUpSettings,
  CreditService,
} from "@delino/accounts-protos/v1/credit_pb";
import { BillingUsageService } from "@delino/accounts-protos/v1/usage_pb";
import { useI18n } from "../../i18n.ts";
import { formatUsd, toAmountInput } from "../../money.ts";
import { can, Permission } from "../../roles.ts";
import { parseRoute, RouteId, routePath, useRouter } from "../../router.tsx";

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
  const { t } = useI18n();
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
              {t("common.save")}
            </Button>
          </div>
        ) : (
          <p className="muted">{t("settings.readOnly")}</p>
        )}
      </form>
    </section>
  );
}

function NameSettings() {
  const { billingAccountId, name, role, queryKey, readVersion } =
    useBillingAccount();
  const queryClient = useQueryClient();
  const update = useMutation(BillingAccountService.method.updateBillingAccount);
  const i18n = useI18n();
  const { t } = i18n;
  const [draft, setDraft] = useState<{ value: string }>();
  const [savedName, setSavedName] = useState<{
    value: string;
    readVersion: number;
  }>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const editable = can(role, Permission.ManageMembers);
  const awaitingRead =
    savedName !== undefined && readVersion <= savedName.readVersion;
  if (savedName !== undefined && !awaitingRead) {
    setSavedName(undefined);
  }
  const value = draft?.value ?? (awaitingRead ? savedName.value : name);
  return (
    <SettingsForm
      title={t("settings.name.title")}
      id="name-title"
      editable={editable}
      onSubmit={async () => {
        const checked = checkAccountName(value, i18n);
        if (!checked.ok) {
          setError(checked.error);
          return;
        }
        setError(undefined);
        setSaving(true);
        try {
          await update.mutateAsync({ billingAccountId, name: checked.value });
          // Snapshot the cache after the mutation, before invalidation replaces
          // any in-flight read. A failed read does not advance the count.
          setSavedName({
            value: checked.value,
            readVersion:
              queryClient.getQueryState(queryKey)?.dataUpdateCount ??
              readVersion,
          });
          // Each edit creates a new draft, even if the user types the same text.
          setDraft((latest) => (latest === draft ? undefined : latest));
          await invalidateServices(queryClient, BillingAccountService);
          toast.success(t("settings.name.saved"));
        } finally {
          setSaving(false);
        }
      }}
    >
      <TextField
        label={t("settings.name.label")}
        value={value}
        onChange={(event) => {
          const value = event.target.value;
          // During a save or its refresh, matching the fetched name can
          // still be a new edit because the fetched name may be stale.
          setDraft(
            !saving && !awaitingRead && value === name ? undefined : { value },
          );
        }}
        error={error}
        disabled={!editable}
        autoComplete="off"
      />
    </SettingsForm>
  );
}

function AutoTopUpSection() {
  const { billingAccountId } = useBillingAccount();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const schema = CreditService.method.getAutoTopUpSettings;
  const settings = useQuery(schema, { billingAccountId });
  const { refetch } = settings;
  const { year, month } = useCurrentUtcMonth();
  const currentMonth = year * 12 + month;
  const previousMonth = useRef(currentMonth);
  const { t } = useI18n();

  useEffect(() => {
    if (previousMonth.current === currentMonth) return;
    previousMonth.current = currentMonth;
    const queryKey = createConnectQueryKey({
      schema,
      transport,
      input: { billingAccountId },
      cardinality: "finite",
    });
    // Refetch replaces a pending refresh but would reuse a pending first read.
    if (queryClient.getQueryData(queryKey) === undefined) {
      void queryClient.cancelQueries({ queryKey, exact: true });
    }
    void refetch();
  }, [billingAccountId, currentMonth, queryClient, refetch, schema, transport]);

  return settings.isPending ? (
    <section className="section" aria-label={t("autoTopUp.title")}>
      <Loading label={t("autoTopUp.loading")} />
    </section>
  ) : settings.isLoadingError ? (
    <section className="section" aria-label={t("autoTopUp.title")}>
      <ErrorNotice error={settings.error} onRetry={() => void refetch()} />
    </section>
  ) : (
    // The notice and the form are siblings in a fragment in every loaded
    // state, so that a failed refetch does not remount the form and discard
    // its draft.
    <>
      <RefetchNotice
        error={settings.isRefetchError ? settings.error : undefined}
        onRetry={() => void refetch()}
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
  saveConfirmation?: { key: string; awaitingResponse: boolean };
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
  const i18n = useI18n();
  const { lang, t } = i18n;
  const editable = can(role, Permission.ManagePayments);
  const settingsKey = autoTopUpSettingsKey(settings);
  const [form, setForm] = useState<AutoTopUpFormState>(() => ({
    settingsKey,
    draft: autoTopUpDraft(settings),
  }));
  const [errors, setErrors] = useState<AutoTopUpErrors>({});
  if (form.settingsKey !== settingsKey) {
    const confirmsSave = settingsKey === form.saveConfirmation?.key;
    setForm({
      settingsKey,
      // Keep identifying reads of the submission until its response arrives.
      saveConfirmation: form.saveConfirmation?.awaitingResponse
        ? form.saveConfirmation
        : undefined,
      draft: confirmsSave ? form.draft : autoTopUpDraft(settings),
    });
    if (!confirmsSave) setErrors({});
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
      title={t("autoTopUp.title")}
      id="auto-top-up-title"
      description={t("autoTopUp.description")}
      editable={editable}
      onSubmit={async () => {
        const checked = checkAutoTopUp(draft, i18n);
        if (!checked.ok) {
          setErrors(checked.errors);
          return;
        }
        setErrors({});
        const pendingConfirmation = {
          key: autoTopUpSettingsKey(checked.value),
          awaitingResponse: true,
        };
        setForm((current) => ({
          ...current,
          saveConfirmation: pendingConfirmation,
        }));
        try {
          const saved = await update.mutateAsync({
            billingAccountId,
            settings: checked.value,
          });
          const savedSettings = saved.settings ?? checked.value;
          const savedKey = autoTopUpSettingsKey(savedSettings);
          setForm((current) => ({
            ...current,
            saveConfirmation:
              savedKey === current.settingsKey
                ? undefined
                : { key: savedKey, awaitingResponse: false },
            // Each edit creates a new draft, even if it restores an earlier value.
            draft:
              current.draft === draft
                ? autoTopUpDraft(savedSettings)
                : current.draft,
          }));
        } catch (error) {
          setForm((current) =>
            current.saveConfirmation === pendingConfirmation
              ? { ...current, saveConfirmation: undefined }
              : current,
          );
          throw error;
        }
        await invalidateServices(queryClient, CreditService);
        toast.success(t("autoTopUp.saved"));
      }}
    >
      <Switch
        label={t("autoTopUp.title")}
        description={
          draft.enabled
            ? undefined
            : t("autoTopUp.disabledHint", { zero: formatUsd(0n, lang) })
        }
        checked={draft.enabled}
        onCheckedChange={(enabled) => editDraft({ enabled })}
        disabled={!editable}
      />
      <TextField
        label={t("autoTopUp.threshold")}
        description={
          draft.enabled
            ? t("autoTopUp.thresholdHint", {
                amount: formatUsd(AUTO_TOP_UP_MIN_THRESHOLD_USD_MICROS, lang),
              })
            : undefined
        }
        value={draft.threshold}
        onChange={(event) => editDraft({ threshold: event.target.value })}
        error={errors.threshold}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label={t("autoTopUp.amount")}
        description={
          draft.enabled
            ? t("autoTopUp.amountHint", {
                min: formatUsd(AUTO_TOP_UP_MIN_AMOUNT_USD_MICROS, lang),
                max: formatUsd(TOP_UP_MAX_USD_MICROS, lang),
              })
            : t("autoTopUp.taxHint")
        }
        value={draft.amount}
        onChange={(event) => editDraft({ amount: event.target.value })}
        error={errors.amount}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <TextField
        label={t("autoTopUp.monthlyCap")}
        description={draft.enabled ? t("autoTopUp.monthlyCapHint") : undefined}
        value={draft.monthlyCap}
        onChange={(event) => editDraft({ monthlyCap: event.target.value })}
        error={errors.monthlyCap}
        disabled={!editable}
        inputMode="decimal"
        autoComplete="off"
      />
      <dl className="details">
        <Detail term={t("autoTopUp.monthlyCapUsage")}>
          {formatUsd(settings?.monthToDateUsdMicros ?? 0n, lang)}
        </Detail>
        <Detail term={t("autoTopUp.failures")}>
          {formatCount(BigInt(settings?.consecutiveFailures ?? 0), lang)}
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
  const { t } = useI18n();
  return settings.isPending ? (
    <section className="section" aria-label={t("alert.title")}>
      <Loading label={t("alert.loading")} />
    </section>
  ) : settings.isLoadingError ? (
    <section className="section" aria-label={t("alert.title")}>
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
  const i18n = useI18n();
  const { lang, t } = i18n;
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
      title={t("alert.title")}
      id="alert-title"
      description={t("alert.description")}
      editable={editable}
      onSubmit={async () => {
        const checked = checkAlertThreshold(threshold, i18n);
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
        toast.success(t("alert.saved"));
      }}
    >
      <TextField
        label={t("alert.threshold")}
        description={t("alert.thresholdHint", {
          min: formatUsd(0n, lang),
          default: formatUsd(DEFAULT_ALERT_THRESHOLD_USD_MICROS, lang),
        })}
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
  const { lang, t } = useI18n();
  return (
    <section className="section danger-zone" aria-labelledby="delete-title">
      <h2 id="delete-title">{t("deleteAccount.title")}</h2>
      <p className="muted">
        {t("deleteAccount.description", { zero: formatUsd(0n, lang) })}
      </p>
      <Button
        variant={ButtonVariant.Danger}
        onClick={() => setConfirming(true)}
      >
        {t("deleteAccount.title")}
      </Button>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={t("deleteAccount.dialog.title", { name })}
        description={t("deleteAccount.dialog.description")}
        confirmLabel={t("deleteAccount.title")}
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
