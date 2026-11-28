// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { NumberInput, type NumberInputDraft } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import {
  AgentVendor,
  GuestOs,
  NetworkMode,
  Priority,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  type RunnerProfile,
  RunnerProfileService,
} from "@delino/sandrun-protos/v1/runner_profile_pb";
import { useMemo, useRef, useState } from "react";
import { useAgentIntegrations } from "../api.ts";
import { SetupEmpty, SetupStep } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  CopyValue,
  FormDialog,
  Loading,
  PageHeader,
  QueryBoundary,
  showsFullError,
} from "../components/ui.tsx";
import {
  CODEX_GUEST_OS_UNAVAILABLE,
  emptyProfileForm,
  formFromProfile,
  MAX_WARM_COUNT,
  type ProfileErrors,
  profileErrors,
  type ProfileFormValues,
  profileFromForm,
  profileUpdateMask,
  SourceKind,
} from "../forms.ts";
import { formatDuration, guestOsName, labels, vendorName } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { can, Permission } from "../roles.ts";
import {
  refreshRunnerProfiles,
  useRunnerProfiles,
} from "../runner-profiles.ts";
import { getVerifiedCodexGuestOses } from "../verified-codex-guest-oses.ts";

const KINDS = [
  WorkloadKind.GITHUB_JOB,
  WorkloadKind.AGENT_SESSION,
  WorkloadKind.DEV_SESSION,
];
const GUEST_OSES = [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS];
const PRIORITIES = [Priority.HIGH, Priority.NORMAL, Priority.LOW];
const RESOURCE_FIELDS = [
  ["vcpuCount", "profiles.dialog.vcpus"],
  ["memoryMib", "profiles.dialog.memory"],
  ["diskGib", "profiles.dialog.disk"],
] as const satisfies readonly (readonly [
  keyof ProfileFormValues,
  MessageKey,
])[];
const DURATION_FIELDS = [
  ["idleTtlMinutes", "profiles.dialog.idleTtl"],
  ["maxTtlHours", "profiles.dialog.maxTtl"],
  ["maxQueueWaitMinutes", "profiles.dialog.maxQueueWait"],
] as const satisfies readonly (readonly [
  keyof ProfileFormValues,
  MessageKey,
])[];

/** The priority part of a profile summary, such as "priority high". */
const PRIORITY_SUMMARY = {
  [Priority.UNSPECIFIED]: "profiles.item.priority.unspecified",
  [Priority.HIGH]: "profiles.item.priority.high",
  [Priority.NORMAL]: "profiles.item.priority.normal",
  [Priority.LOW]: "profiles.item.priority.low",
} as const satisfies Record<Priority, MessageKey>;

/** Route `profiles`: runner profiles; create, change and delete for `admin`. */
export function ProfilesPage() {
  const i18n = useI18n();
  const { t, rich } = i18n;
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const admin = can(role, Permission.ManageRunnerProfiles);
  const list = useRunnerProfiles(organizationId);
  const agentIntegrations = useAgentIntegrations(organizationId);
  const { integrations, query: integrationQuery } = agentIntegrations;
  const integrationsUnavailable = showsFullError(integrationQuery);
  const integrationNames = useMemo(
    () =>
      new Map(
        integrationsUnavailable
          ? []
          : integrations.map((i) => [
              i.agentIntegrationId,
              t("profiles.integrationName", {
                name: i.name,
                vendor: vendorName(i.vendor, t),
              }),
            ]),
      ),
    [integrations, integrationsUnavailable, t],
  );
  const remove = useMutation(RunnerProfileService.method.deleteRunnerProfile, {
    onSuccess: () => refreshRunnerProfiles(queryClient),
  });
  const [editing, setEditing] = useState<RunnerProfile | "new">();
  const [deleting, setDeleting] = useState<RunnerProfile>();

  return (
    <>
      <PageHeader
        title={t("profiles.title")}
        description={t("profiles.description", { label: "runs-on" })}
        action={
          admin ? (
            <Button onClick={() => setEditing("new")}>
              {t("profiles.create")}
            </Button>
          ) : undefined
        }
      />
      {integrationQuery.error ? (
        <QueryBoundary
          query={integrationQuery}
          loading={t("profiles.loadingIntegrations")}
        >
          {() => null}
        </QueryBoundary>
      ) : null}
      <QueryBoundary
        query={list}
        loading={t("profiles.loading")}
        errorTitle={t("profiles.loadFailed")}
      >
        {(profiles) => {
          return profiles.length === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title={t("profiles.empty.title")}
              hiddenStep={admin ? undefined : SetupStep.CreateRunnerProfile}
            >
              {admin ? t("profiles.empty.admin") : t("profiles.empty.member")}
            </SetupEmpty>
          ) : (
            <ul className="list" aria-label={t("profiles.title")}>
              {profiles.map((p) => (
                <li key={p.runnerProfileId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {p.name} · {t(labels.workloadKind[p.workloadKind])} ·{" "}
                      {guestOsName(p.guestOs, t)}
                    </span>
                    <span className="muted">
                      {[
                        p.source.case === "ownsandTemplateId"
                          ? t("profiles.item.template", { id: p.source.value })
                          : t("profiles.item.baseImage"),
                        t("profiles.item.vcpus", {
                          count: p.resources?.vcpuCount ?? 0,
                        }),
                        t("profiles.item.memory", {
                          mib: String(p.resources?.memoryMib ?? 0n),
                        }),
                        t("profiles.item.disk", {
                          gib: String(p.resources?.diskGib ?? 0n),
                        }),
                        t(PRIORITY_SUMMARY[p.priority]),
                        t("profiles.item.warm", { count: p.warmCount }),
                        t("profiles.item.maxQueueWait", {
                          duration: formatDuration(p.maxQueueWait, i18n),
                        }),
                      ].join(" · ")}
                    </span>
                    {p.workloadKind === WorkloadKind.GITHUB_JOB &&
                    p.githubLabel !== "" ? (
                      <span>
                        runs-on:{" "}
                        <CopyValue
                          label={t("profiles.item.runsOnLabel", {
                            name: p.name,
                          })}
                          value={p.githubLabel}
                        />
                      </span>
                    ) : null}
                    {p.workloadKind === WorkloadKind.AGENT_SESSION ? (
                      <span className="muted">
                        {t("profiles.item.integration", {
                          name:
                            integrationNames.get(p.agentIntegrationId) ??
                            p.agentIntegrationId,
                        })}
                      </span>
                    ) : null}
                    {p.workloadKind === WorkloadKind.DEV_SESSION ? (
                      <span className="muted">
                        {rich("profiles.item.workspace", {
                          path: (
                            <code className="profile-workspace-path">
                              {p.workspacePath}
                            </code>
                          ),
                        })}
                      </span>
                    ) : null}
                  </div>
                  {admin ? (
                    <div className="list-actions">
                      <Button
                        variant={ButtonVariant.Secondary}
                        size={ButtonSize.Small}
                        onClick={() => setEditing(p)}
                      >
                        {t("profiles.edit")}
                      </Button>
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        onClick={() => setDeleting(p)}
                      >
                        {t("common.delete")}
                      </Button>
                    </div>
                  ) : null}
                </li>
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      {editing !== undefined ? (
        <ProfileDialog
          profile={editing === "new" ? undefined : editing}
          agentIntegrations={agentIntegrations}
          onClose={() => setEditing(undefined)}
        />
      ) : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={
          deleting === undefined
            ? t("profiles.deleteDialog.titleUnnamed")
            : t("profiles.deleteDialog.title", { name: deleting.name })
        }
        description={t("profiles.deleteDialog.description")}
        confirmLabel={t("profiles.deleteDialog.confirm")}
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            runnerProfileId: deleting!.runnerProfileId,
          })
        }
      />
    </>
  );
}

/** The create and edit dialog of a runner profile. */
function ProfileDialog({
  profile,
  agentIntegrations,
  onClose,
}: {
  profile: RunnerProfile | undefined;
  agentIntegrations: ReturnType<typeof useAgentIntegrations>;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const initial = useMemo(
    () =>
      profile === undefined ? emptyProfileForm() : formFromProfile(profile),
    [profile],
  );
  const [values, setValues] = useState<ProfileFormValues>(initial);
  const [errors, setErrors] = useState<ProfileErrors>({});
  const warmCountDraft = useRef<NumberInputDraft>(null);
  const { integrations, hasNextPage, isLoadingMore, query } = agentIntegrations;
  const refresh = () => refreshRunnerProfiles(queryClient);
  const createProfile = useMutation(
    RunnerProfileService.method.createRunnerProfile,
    { onSuccess: refresh },
  );
  const updateProfile = useMutation(
    RunnerProfileService.method.updateRunnerProfile,
    { onSuccess: refresh },
  );
  const set = <K extends keyof ProfileFormValues>(
    key: K,
    value: ProfileFormValues[K],
  ) => {
    setValues((v) => ({ ...v, [key]: value }));
    setErrors((e) => ({ ...e, [key]: undefined }));
  };
  const integrationsUnavailable = showsFullError(query);
  const integrationVendor = (id: string) =>
    integrationsUnavailable
      ? undefined
      : integrations.find((i) => i.agentIntegrationId === id)?.vendor;
  const vendor = integrationVendor(values.agentIntegrationId);
  const agentSession = values.workloadKind === WorkloadKind.AGENT_SESSION;
  const codexGuestOses = getVerifiedCodexGuestOses();
  const guestOsOptions =
    agentSession && vendor === AgentVendor.OPENAI_CODEX
      ? codexGuestOses
      : GUEST_OSES;
  const guestOsUnavailable = guestOsOptions.length === 0;
  const setAgentSelection = (
    workloadKind: WorkloadKind,
    integrationId: string,
  ) => {
    setValues((current) => ({
      ...current,
      workloadKind,
      agentIntegrationId: integrationId,
      guestOs:
        workloadKind === WorkloadKind.AGENT_SESSION &&
        integrationVendor(integrationId) === AgentVendor.OPENAI_CODEX &&
        !codexGuestOses.includes(current.guestOs)
          ? GuestOs.UNSPECIFIED
          : current.guestOs,
    }));
    setErrors((current) => ({
      ...current,
      workloadKind: undefined,
      agentIntegrationId: undefined,
      guestOs: undefined,
      maxQueueWaitMinutes: undefined,
    }));
  };
  const creating = profile === undefined;

  return (
    <FormDialog
      open
      wide
      onOpenChange={(open) => !open && onClose()}
      title={
        creating
          ? t("profiles.dialog.createTitle")
          : t("profiles.dialog.editTitle", { name: profile.name })
      }
      description={
        creating
          ? t("profiles.dialog.createDescription")
          : t("profiles.dialog.editDescription")
      }
      submitLabel={
        creating ? t("profiles.dialog.createSubmit") : t("common.saveChanges")
      }
      submitDisabled={
        agentSession &&
        (query.isPending ||
          integrationsUnavailable ||
          (values.agentIntegrationId !== "" && vendor === undefined) ||
          guestOsUnavailable)
      }
      onSubmit={async (opening) => {
        const submitted = warmCountDraft.current?.isInvalid()
          ? { ...values, warmCount: null }
          : values;
        const found = profileErrors(submitted, vendor, t);
        if (Object.keys(found).length > 0) {
          setErrors(found);
          return;
        }
        if (creating) {
          await createProfile.mutateAsync({
            organizationId,
            profile: profileFromForm(submitted),
          });
        } else {
          const mask = profileUpdateMask(initial, submitted);
          if (mask.paths.length > 0) {
            await updateProfile.mutateAsync({
              organizationId,
              runnerProfileId: profile.runnerProfileId,
              profile: profileFromForm(submitted),
              updateMask: mask,
            });
          }
        }
        if (opening.isCurrent()) onClose();
      }}
    >
      <TextField
        label={t("profiles.dialog.name")}
        description={t("profiles.dialog.nameDescription", {
          label: "sandrun-<name>",
        })}
        value={values.name}
        onValueChange={(v) => set("name", v)}
        error={errors.name}
        autoFocus
        maxLength={40}
        autoComplete="off"
      />
      <div className="field-row">
        <Select
          label={t("profiles.dialog.workloadKind")}
          value={String(values.workloadKind)}
          onValueChange={(v) =>
            v &&
            setAgentSelection(
              Number(v) as WorkloadKind,
              values.agentIntegrationId,
            )
          }
          disabled={!creating}
        >
          {KINDS.map((k) => (
            <SelectOption key={k} value={String(k)}>
              {t(labels.workloadKind[k])}
            </SelectOption>
          ))}
        </Select>
        <Select
          label={t("common.column.guestOs")}
          value={
            guestOsOptions.includes(values.guestOs)
              ? String(values.guestOs)
              : null
          }
          placeholder={
            guestOsUnavailable
              ? t("profiles.dialog.guestOsUnavailable")
              : t("profiles.dialog.guestOsPlaceholder")
          }
          onValueChange={(v) => v && set("guestOs", Number(v) as GuestOs)}
          disabled={guestOsUnavailable}
          description={
            guestOsUnavailable ? t(CODEX_GUEST_OS_UNAVAILABLE) : undefined
          }
          error={errors.guestOs}
        >
          {guestOsOptions.map((g) => (
            <SelectOption key={g} value={String(g)}>
              {guestOsName(g, t)}
            </SelectOption>
          ))}
        </Select>
      </div>
      {agentSession ? (
        <QueryBoundary
          query={query}
          loading={t("profiles.loadingIntegrations")}
        >
          {() => (
            <>
              {isLoadingMore ? (
                <Loading label={t("profiles.loadingIntegrations")} />
              ) : null}
              {!creating &&
              values.agentIntegrationId !== "" &&
              vendor === undefined &&
              !hasNextPage &&
              !query.isFetching &&
              query.error === null ? (
                <div className="notice notice-error" role="alert">
                  <p>{t("profiles.dialog.savedIntegrationMissing")}</p>
                </div>
              ) : null}
              <Select
                label={t("profiles.dialog.agentIntegration")}
                value={
                  values.agentIntegrationId === ""
                    ? null
                    : values.agentIntegrationId
                }
                onValueChange={(v) =>
                  v && setAgentSelection(values.workloadKind, v)
                }
                error={errors.agentIntegrationId}
                disabled={!creating}
              >
                {integrations.map((i) => (
                  <SelectOption
                    key={i.agentIntegrationId}
                    value={i.agentIntegrationId}
                  >
                    {t("profiles.integrationName", {
                      name: i.name,
                      vendor: vendorName(i.vendor, t),
                    })}
                  </SelectOption>
                ))}
              </Select>
            </>
          )}
        </QueryBoundary>
      ) : null}
      {values.workloadKind === WorkloadKind.DEV_SESSION ? (
        <TextField
          label={t("profiles.dialog.workspacePath")}
          description={t("profiles.dialog.workspacePathDescription")}
          value={values.workspacePath}
          onValueChange={(v) => set("workspacePath", v)}
          error={errors.workspacePath}
          required
          autoComplete="off"
          placeholder={
            values.guestOs === GuestOs.WINDOWS ? "C:\\work" : "/home/dev/work"
          }
        />
      ) : null}
      <div className="field-row">
        <Select
          label={t("profiles.dialog.source")}
          value={values.sourceKind}
          onValueChange={(v) => v && set("sourceKind", v as SourceKind)}
        >
          <SelectOption value={SourceKind.BaseImage}>
            {t("profiles.dialog.sourceBaseImage")}
          </SelectOption>
          <SelectOption value={SourceKind.Template}>
            {t("profiles.dialog.sourceTemplate")}
          </SelectOption>
        </Select>
        {values.sourceKind === SourceKind.Template ? (
          <TextField
            label={t("profiles.dialog.templateId")}
            value={values.templateId}
            onValueChange={(v) => set("templateId", v)}
            error={errors.templateId}
            autoComplete="off"
          />
        ) : null}
      </div>
      <div className="field-row">
        {RESOURCE_FIELDS.map(([field, label]) => (
          <TextField
            key={field}
            label={t(label)}
            type="number"
            inputMode="decimal"
            step="any"
            defaultValue={String(initial[field] ?? "")}
            onChange={(event) => {
              const value = event.currentTarget.valueAsNumber;
              set(field, Number.isNaN(value) ? null : value);
            }}
            error={errors[field]}
          />
        ))}
      </div>
      <div className="field-row">
        {DURATION_FIELDS.map(([field, label]) => (
          <TextField
            key={field}
            label={t(label)}
            type="number"
            inputMode="decimal"
            step="any"
            defaultValue={String(initial[field] ?? "")}
            onChange={(event) => {
              const value = event.currentTarget.valueAsNumber;
              set(field, Number.isNaN(value) ? null : value);
            }}
            error={errors[field]}
          />
        ))}
      </div>
      <div className="field-row">
        <Select
          label={t("common.column.priority")}
          value={String(values.priority)}
          onValueChange={(v) => v && set("priority", Number(v) as Priority)}
        >
          {creating ? (
            // Only CreateRunnerProfile resolves UNSPECIFIED to the default
            // of the workload kind; UpdateRunnerProfile requires a priority.
            <SelectOption value={String(Priority.UNSPECIFIED)}>
              {t("profiles.dialog.priorityDefault")}
            </SelectOption>
          ) : null}
          {PRIORITIES.map((p) => (
            <SelectOption key={p} value={String(p)}>
              {t(labels.priority[p])}
            </SelectOption>
          ))}
        </Select>
        <NumberInput
          label={t("profiles.dialog.warmCount")}
          description={t("profiles.dialog.warmCountDescription")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
          value={values.warmCount}
          onValueChange={(v) => set("warmCount", v)}
          draftRef={warmCountDraft}
          validateDraftBounds
          min={0}
          max={MAX_WARM_COUNT}
          error={errors.warmCount}
        />
      </div>
      <p className="muted">
        {t("profiles.dialog.network", {
          mode: t(labels.networkMode[NetworkMode.OUTBOUND]),
        })}
      </p>
    </FormDialog>
  );
}
