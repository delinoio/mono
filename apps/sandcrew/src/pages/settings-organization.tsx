// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { durationFromMs, FieldMaskSchema } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { CredentialScope } from "@delino/sandcrew-protos/v1/common_pb";
import {
  type OrganizationSettings,
  type OrganizationSettingsSchema,
  OrganizationService,
  type RunnerProfile,
} from "@delino/sandcrew-protos/v1/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { NumberInput } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { type FormEvent, useState } from "react";
import { invalidateServices } from "../api.ts";
import { VendorCredentialSection } from "../components/credentials.tsx";
import { useOrg } from "../components/shell.tsx";
import { ErrorNotice, PageHeader, QueryBoundary } from "../components/ui.tsx";
import { sandrunProfilesUrl } from "../config.ts";
import { durationMs, labels, profileSummary } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { useRunnerProfiles } from "../queries.ts";

const MINUTE_MS = 60_000;
const DAY_MS = 24 * 3600_000;

/** The pause delay range of the control plane, in minutes. */
export const PAUSE_DELAY_MIN_MINUTES = 1;
export const PAUSE_DELAY_MAX_MINUTES = 24 * 60;
/** The paused-session retention range of the control plane, in days. */
export const RETENTION_MIN_DAYS = 1;
export const RETENTION_MAX_DAYS = 365;

/** The retention choice of the form. */
enum RetentionKind {
  Days = "days",
  Unlimited = "unlimited",
}

/** The editable values of the organization settings. */
export interface SettingsDraft {
  allowedRunnerProfileIds: string[];
  pauseDelayMinutes: number | null;
  retention: RetentionKind;
  retentionDays: number | null;
}

export function draftFromSettings(
  settings: OrganizationSettings,
): SettingsDraft {
  const retention = settings.pausedSessionRetention?.retention;
  return {
    allowedRunnerProfileIds: [...settings.allowedRunnerProfileIds],
    pauseDelayMinutes:
      settings.pauseDelay === undefined
        ? 10
        : durationMs(settings.pauseDelay)! / MINUTE_MS,
    retention:
      retention?.case === "unlimited" && retention.value
        ? RetentionKind.Unlimited
        : RetentionKind.Days,
    retentionDays:
      retention?.case === "duration"
        ? durationMs(retention.value)! / DAY_MS
        : 30,
  };
}

/** The values of the validation messages. */
const LIMITS = {
  minMinutes: PAUSE_DELAY_MIN_MINUTES,
  maxMinutes: PAUSE_DELAY_MAX_MINUTES,
  minDays: RETENTION_MIN_DAYS,
  maxDays: RETENTION_MAX_DAYS,
};

/**
 * Validates a draft. Returns the catalog keys of field errors; an empty
 * object means valid.
 */
export function validateDraft(draft: SettingsDraft): {
  pauseDelay?: MessageKey;
  retentionDays?: MessageKey;
} {
  const errors: { pauseDelay?: MessageKey; retentionDays?: MessageKey } = {};
  const delay = draft.pauseDelayMinutes;
  if (
    delay === null ||
    !Number.isInteger(delay) ||
    delay < PAUSE_DELAY_MIN_MINUTES ||
    delay > PAUSE_DELAY_MAX_MINUTES
  ) {
    errors.pauseDelay = "orgSettings.error.pauseDelay";
  }
  const days = draft.retentionDays;
  if (
    draft.retention === RetentionKind.Days &&
    (days === null ||
      !Number.isInteger(days) ||
      days < RETENTION_MIN_DAYS ||
      days > RETENTION_MAX_DAYS)
  ) {
    errors.retentionDays = "orgSettings.error.retentionDays";
  }
  return errors;
}

/**
 * The update of the changed fields only, so a save does not overwrite
 * newer changes of other fields. Undefined when nothing changed.
 */
export function settingsUpdate(
  saved: SettingsDraft,
  draft: SettingsDraft,
):
  | {
      paths: string[];
      settings: MessageInitShape<typeof OrganizationSettingsSchema>;
    }
  | undefined {
  const paths: string[] = [];
  const settings: MessageInitShape<typeof OrganizationSettingsSchema> = {};
  const sameProfiles =
    saved.allowedRunnerProfileIds.length ===
      draft.allowedRunnerProfileIds.length &&
    saved.allowedRunnerProfileIds.every((id) =>
      draft.allowedRunnerProfileIds.includes(id),
    );
  if (!sameProfiles) {
    paths.push("allowed_runner_profile_ids");
    settings.allowedRunnerProfileIds = draft.allowedRunnerProfileIds;
  }
  if (saved.pauseDelayMinutes !== draft.pauseDelayMinutes) {
    paths.push("pause_delay");
    settings.pauseDelay = durationFromMs(draft.pauseDelayMinutes! * MINUTE_MS);
  }
  if (
    saved.retention !== draft.retention ||
    (draft.retention === RetentionKind.Days &&
      saved.retentionDays !== draft.retentionDays)
  ) {
    paths.push("paused_session_retention");
    settings.pausedSessionRetention = {
      retention:
        draft.retention === RetentionKind.Unlimited
          ? { case: "unlimited", value: true }
          : {
              case: "duration",
              value: durationFromMs(draft.retentionDays! * DAY_MS),
            },
    };
  }
  return paths.length === 0 ? undefined : { paths, settings };
}

/** Route `SETTINGS_ORG`: owners and admins only. */
export function OrganizationSettingsPage() {
  const { organizationId } = useOrg();
  const { t } = useI18n();
  const settings = useQuery(
    OrganizationService.method.getOrganizationSettings,
    {
      organizationId,
    },
  );
  return (
    <>
      <PageHeader
        title={t("nav.organizationSettings")}
        description={t("orgSettings.description")}
      />
      <section className="section" aria-labelledby="org-credentials-heading">
        <h2 id="org-credentials-heading">
          {t("credentials.organizationList")}
        </h2>
        <p className="muted">{t("orgSettings.credentialsHelp")}</p>
        <VendorCredentialSection scope={CredentialScope.ORGANIZATION} />
      </section>
      <QueryBoundary query={settings} loading={t("orgSettings.loading")}>
        {(data) => (
          <SettingsForm
            // Two saves can happen in the same second, so the key uses the
            // complete update time.
            key={`${data.settings?.updateTime?.seconds ?? ""}.${data.settings?.updateTime?.nanos ?? ""}`}
            settings={data.settings!}
          />
        )}
      </QueryBoundary>
    </>
  );
}

function SettingsForm({ settings }: { settings: OrganizationSettings }) {
  const { organizationId } = useOrg();
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const profiles = useRunnerProfiles(organizationId);
  const [saved] = useState(() => draftFromSettings(settings));
  const [draft, setDraft] = useState(saved);
  const [errors, setErrors] = useState<ReturnType<typeof validateDraft>>({});
  const update = useMutation(
    OrganizationService.method.updateOrganizationSettings,
    {
      onSuccess: () => invalidateServices(queryClient, OrganizationService),
    },
  );
  const change = settingsUpdate(saved, draft);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const nextErrors = validateDraft(draft);
    setErrors(nextErrors);
    if (nextErrors.pauseDelay || nextErrors.retentionDays) return;
    if (change === undefined || update.isPending) return;
    try {
      await update.mutateAsync({
        organizationId,
        settings: { organizationId, ...change.settings },
        updateMask: create(FieldMaskSchema, { paths: change.paths }),
      });
      toast.success(t("orgSettings.saved"));
    } catch {
      // The form shows the mutation error and keeps the edits.
    }
  };

  const toggleProfile = (id: string, on: boolean) =>
    setDraft((d) => ({
      ...d,
      allowedRunnerProfileIds: on
        ? [...d.allowedRunnerProfileIds.filter((p) => p !== id), id]
        : d.allowedRunnerProfileIds.filter((p) => p !== id),
    }));

  return (
    <form className="form" onSubmit={submit} noValidate>
      <section className="section" aria-labelledby="profiles-heading">
        <h2 id="profiles-heading">{t("orgSettings.profiles")}</h2>
        <p className="muted">
          {t("orgSettings.profilesHelp")}{" "}
          <a
            href={sandrunProfilesUrl(organizationId)}
            target="_blank"
            rel="noopener noreferrer"
          >
            {t("orgSettings.manageProfiles")}
          </a>
        </p>
        <QueryBoundary query={profiles} loading={t("profiles.loading")}>
          {(list) => (
            <ProfileChoices
              profiles={list}
              allowed={draft.allowedRunnerProfileIds}
              onToggle={toggleProfile}
            />
          )}
        </QueryBoundary>
      </section>
      <section className="section" aria-labelledby="pause-heading">
        <h2 id="pause-heading">{t("orgSettings.pause")}</h2>
        <NumberInput
          label={t("orgSettings.pauseDelay")}
          description={t("orgSettings.pauseDelayHelp")}
          value={draft.pauseDelayMinutes}
          onValueChange={(v: number | null) => {
            setDraft((d) => ({ ...d, pauseDelayMinutes: v }));
            setErrors((e) => ({ ...e, pauseDelay: undefined }));
          }}
          min={PAUSE_DELAY_MIN_MINUTES}
          max={PAUSE_DELAY_MAX_MINUTES}
          error={
            errors.pauseDelay === undefined
              ? undefined
              : t(errors.pauseDelay, LIMITS)
          }
        />
        <Select
          label={t("orgSettings.retention")}
          description={t("orgSettings.retentionHelp")}
          value={draft.retention}
          onValueChange={(v) => {
            if (v === null) return;
            setDraft((d) => ({
              ...d,
              retention:
                v === RetentionKind.Unlimited
                  ? RetentionKind.Unlimited
                  : RetentionKind.Days,
            }));
            setErrors((e) => ({ ...e, retentionDays: undefined }));
          }}
        >
          <SelectOption value={RetentionKind.Days}>
            {t("orgSettings.retentionDaysOption")}
          </SelectOption>
          <SelectOption value={RetentionKind.Unlimited}>
            {t("orgSettings.retentionUnlimited")}
          </SelectOption>
        </Select>
        {draft.retention === RetentionKind.Days ? (
          <NumberInput
            label={t("orgSettings.retentionDays")}
            value={draft.retentionDays}
            onValueChange={(v: number | null) => {
              setDraft((d) => ({ ...d, retentionDays: v }));
              setErrors((e) => ({ ...e, retentionDays: undefined }));
            }}
            min={RETENTION_MIN_DAYS}
            max={RETENTION_MAX_DAYS}
            error={
              errors.retentionDays === undefined
                ? undefined
                : t(errors.retentionDays, LIMITS)
            }
          />
        ) : null}
      </section>
      {update.error ? <ErrorNotice error={update.error} /> : null}
      <div className="form-actions">
        <Button
          type="submit"
          disabled={change === undefined || update.isPending}
        >
          {t("orgSettings.save")}
        </Button>
      </div>
    </form>
  );
}

function ProfileChoices({
  profiles,
  allowed,
  onToggle,
}: {
  profiles: readonly RunnerProfile[];
  allowed: readonly string[];
  onToggle: (id: string, on: boolean) => void;
}) {
  const i18n = useI18n();
  const { t, lang } = i18n;
  const known = new Set(profiles.map((p) => p.runnerProfileId));
  const missing = allowed.filter((id) => !known.has(id));
  if (profiles.length === 0 && missing.length === 0) {
    return <p className="muted">{t("orgSettings.noProfiles")}</p>;
  }
  return (
    <div className="checkbox-list">
      {profiles.map((p) => {
        const reasons = new Intl.ListFormat(lang, { type: "unit" }).format(
          p.ineligibility.map((r) =>
            t(labels.ineligibility[r] ?? "enum.unknown"),
          ),
        );
        const checked = allowed.includes(p.runnerProfileId);
        return (
          <Checkbox
            key={p.runnerProfileId}
            label={p.name}
            description={
              p.selectable
                ? profileSummary(p, i18n)
                : t("orgSettings.notSelectable", {
                    reasons: reasons || t("orgSettings.cannotUse"),
                  })
            }
            checked={checked}
            // A profile that is not selectable can still be removed.
            disabled={!p.selectable && !checked}
            onCheckedChange={(on) => onToggle(p.runnerProfileId, on === true)}
          />
        );
      })}
      {missing.map((id) => (
        <Checkbox
          key={id}
          label={t("orgSettings.missingProfile", { id })}
          description={t("orgSettings.missingProfileHelp")}
          checked
          onCheckedChange={(on) => onToggle(id, on === true)}
        />
      ))}
    </div>
  );
}
