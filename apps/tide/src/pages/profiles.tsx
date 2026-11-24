// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { NumberInput } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { AgentIntegrationService } from "@delino/tide-protos/v1/agent_integration_pb";
import {
  GuestOs,
  NetworkMode,
  Priority,
  WorkloadKind,
} from "@delino/tide-protos/v1/common_pb";
import {
  type RunnerProfile,
  RunnerProfileService,
} from "@delino/tide-protos/v1/runner_profile_pb";
import { useMemo, useState } from "react";
import { MAX_PAGE_SIZE } from "../api.ts";
import { SetupEmpty } from "../components/setup.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  CopyValue,
  FormDialog,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import {
  emptyProfileForm,
  formFromProfile,
  type ProfileErrors,
  profileErrors,
  type ProfileFormValues,
  profileFromForm,
  profileUpdateMask,
  SourceKind,
} from "../forms.ts";
import { formatDuration, labels } from "../format.ts";
import { can, Permission } from "../roles.ts";
import {
  refreshRunnerProfiles,
  useRunnerProfiles,
} from "../runner-profiles.ts";

const KINDS = [
  WorkloadKind.GITHUB_JOB,
  WorkloadKind.AGENT_SESSION,
  WorkloadKind.DEV_SESSION,
];
const GUEST_OSES = [GuestOs.LINUX, GuestOs.MACOS, GuestOs.WINDOWS];
const PRIORITIES = [Priority.HIGH, Priority.NORMAL, Priority.LOW];

/** Route `profiles`: runner profiles; create, change and delete for `admin`. */
export function ProfilesPage() {
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const admin = can(role, Permission.ManageRunnerProfiles);
  const list = useRunnerProfiles(organizationId);
  const integrations = useQuery(
    AgentIntegrationService.method.listAgentIntegrations,
    { organizationId, pageSize: MAX_PAGE_SIZE },
  );
  const integrationNames = useMemo(
    () =>
      new Map(
        (integrations.data?.integrations ?? []).map((i) => [
          i.agentIntegrationId,
          `${i.name} (${labels.vendor[i.vendor]})`,
        ]),
      ),
    [integrations.data],
  );
  const remove = useMutation(RunnerProfileService.method.deleteRunnerProfile, {
    onSuccess: () => refreshRunnerProfiles(queryClient),
  });
  const [editing, setEditing] = useState<RunnerProfile | "new">();
  const [deleting, setDeleting] = useState<RunnerProfile>();

  return (
    <>
      <PageHeader
        title="Runner profiles"
        description="A runner profile describes one kind of runner. GitHub jobs select a profile with its runs-on label; agent sessions use the profile of their integration; dev sessions name a profile."
        action={
          admin ? (
            <Button onClick={() => setEditing("new")}>
              Create runner profile
            </Button>
          ) : undefined
        }
      />
      <QueryBoundary
        query={list}
        loading="Loading runner profiles"
        errorTitle="Could not load runner profiles."
      >
        {(profiles) => {
          return profiles.length === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title="No runner profiles"
            >
              {admin
                ? "Create a runner profile to start runners for your work."
                : "Ask an admin to create a runner profile."}
            </SetupEmpty>
          ) : (
            <ul className="list" aria-label="Runner profiles">
              {profiles.map((p) => (
                <li key={p.runnerProfileId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {p.name} · {labels.workloadKind[p.workloadKind]} ·{" "}
                      {labels.guestOs[p.guestOs]}
                    </span>
                    <span className="muted">
                      {p.source.case === "ownsandTemplateId"
                        ? `Template ${p.source.value}`
                        : "Base image"}{" "}
                      · {p.resources?.vcpuCount ?? 0} vCPU ·{" "}
                      {String(p.resources?.memoryMib ?? 0n)} MiB ·{" "}
                      {String(p.resources?.diskGib ?? 0n)} GiB · priority{" "}
                      {labels.priority[p.priority].toLowerCase()} · warm{" "}
                      {p.warmCount} · queue wait at most{" "}
                      {formatDuration(p.maxQueueWait)}
                    </span>
                    {p.workloadKind === WorkloadKind.GITHUB_JOB &&
                    p.githubLabel !== "" ? (
                      <span>
                        runs-on:{" "}
                        <CopyValue
                          label={`runs-on label of ${p.name}`}
                          value={p.githubLabel}
                        />
                      </span>
                    ) : null}
                    {p.workloadKind === WorkloadKind.AGENT_SESSION ? (
                      <span className="muted">
                        Integration:{" "}
                        {integrationNames.get(p.agentIntegrationId) ??
                          p.agentIntegrationId}
                      </span>
                    ) : null}
                    {p.workloadKind === WorkloadKind.DEV_SESSION ? (
                      <span className="muted">
                        Workspace: <code>{p.workspacePath}</code>
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
                        Edit
                      </Button>
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        onClick={() => setDeleting(p)}
                      >
                        Delete
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
          onClose={() => setEditing(undefined)}
        />
      ) : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={`Delete ${deleting?.name ?? "this profile"}?`}
        description="Its queued requests end with Canceled, and its warm runners and runners that wait for work are deleted. Busy runners finish their work. This cannot be undone."
        confirmLabel="Delete profile"
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
  onClose,
}: {
  profile: RunnerProfile | undefined;
  onClose: () => void;
}) {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const initial = useMemo(
    () =>
      profile === undefined ? emptyProfileForm() : formFromProfile(profile),
    [profile],
  );
  const [values, setValues] = useState<ProfileFormValues>(initial);
  const [errors, setErrors] = useState<ProfileErrors>({});
  const integrations = useQuery(
    AgentIntegrationService.method.listAgentIntegrations,
    { organizationId, pageSize: MAX_PAGE_SIZE },
  );
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
  const vendor = integrations.data?.integrations.find(
    (i) => i.agentIntegrationId === values.agentIntegrationId,
  )?.vendor;
  const creating = profile === undefined;

  return (
    <FormDialog
      open
      wide
      onOpenChange={(open) => !open && onClose()}
      title={creating ? "Create a runner profile" : `Edit ${profile.name}`}
      description={
        creating
          ? "The workload kind and the agent integration cannot change later."
          : "Changes apply to runners that start after the change."
      }
      submitLabel={creating ? "Create profile" : "Save changes"}
      onSubmit={async () => {
        const found = profileErrors(values, vendor);
        if (Object.keys(found).length > 0) {
          setErrors(found);
          return;
        }
        if (creating) {
          await createProfile.mutateAsync({
            organizationId,
            profile: profileFromForm(values),
          });
        } else {
          const mask = profileUpdateMask(initial, values);
          if (mask.paths.length > 0) {
            await updateProfile.mutateAsync({
              organizationId,
              runnerProfileId: profile.runnerProfileId,
              profile: profileFromForm(values),
              updateMask: mask,
            });
          }
        }
        onClose();
      }}
    >
      <TextField
        label="Name"
        description="Lowercase kebab-case. A GitHub job profile gets the runs-on label tide-<name>."
        value={values.name}
        onValueChange={(v) => set("name", v)}
        error={errors.name}
        autoFocus
        maxLength={40}
        autoComplete="off"
      />
      <div className="field-row">
        <Select
          label="Workload kind"
          value={String(values.workloadKind)}
          onValueChange={(v) =>
            v && set("workloadKind", Number(v) as WorkloadKind)
          }
          disabled={!creating}
        >
          {KINDS.map((k) => (
            <SelectOption key={k} value={String(k)}>
              {labels.workloadKind[k]}
            </SelectOption>
          ))}
        </Select>
        <Select
          label="Guest OS"
          value={String(values.guestOs)}
          onValueChange={(v) => v && set("guestOs", Number(v) as GuestOs)}
          error={errors.guestOs}
        >
          {GUEST_OSES.map((g) => (
            <SelectOption key={g} value={String(g)}>
              {labels.guestOs[g]}
            </SelectOption>
          ))}
        </Select>
      </div>
      {values.workloadKind === WorkloadKind.AGENT_SESSION ? (
        <Select
          label="Agent integration"
          value={
            values.agentIntegrationId === "" ? null : values.agentIntegrationId
          }
          onValueChange={(v) => v && set("agentIntegrationId", v)}
          error={errors.agentIntegrationId}
          disabled={!creating}
        >
          {(integrations.data?.integrations ?? []).map((i) => (
            <SelectOption
              key={i.agentIntegrationId}
              value={i.agentIntegrationId}
            >
              {`${i.name} (${labels.vendor[i.vendor]})`}
            </SelectOption>
          ))}
        </Select>
      ) : null}
      {values.workloadKind === WorkloadKind.DEV_SESSION ? (
        <TextField
          label="Workspace path"
          description="Required for dev sessions: an absolute guest path. Sync writes into it, and commands start in it."
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
          label="Source"
          value={values.sourceKind}
          onValueChange={(v) => v && set("sourceKind", v as SourceKind)}
        >
          <SelectOption value={SourceKind.BaseImage}>
            OwnSand base image of the guest OS
          </SelectOption>
          <SelectOption value={SourceKind.Template}>
            OwnSand template
          </SelectOption>
        </Select>
        {values.sourceKind === SourceKind.Template ? (
          <TextField
            label="Template ID"
            value={values.templateId}
            onValueChange={(v) => set("templateId", v)}
            error={errors.templateId}
            autoComplete="off"
          />
        ) : null}
      </div>
      <div className="field-row">
        <NumberInput
          label="vCPUs"
          value={values.vcpuCount}
          onValueChange={(v) => set("vcpuCount", v)}
          min={1}
          error={errors.vcpuCount}
        />
        <NumberInput
          label="Memory (MiB)"
          value={values.memoryMib}
          onValueChange={(v) => set("memoryMib", v)}
          min={1}
          error={errors.memoryMib}
        />
        <NumberInput
          label="Disk (GiB)"
          value={values.diskGib}
          onValueChange={(v) => set("diskGib", v)}
          min={1}
          error={errors.diskGib}
        />
      </div>
      <div className="field-row">
        <NumberInput
          label="Idle TTL (minutes)"
          value={values.idleTtlMinutes}
          onValueChange={(v) => set("idleTtlMinutes", v)}
          min={1}
          error={errors.idleTtlMinutes}
        />
        <NumberInput
          label="Maximum TTL (hours)"
          value={values.maxTtlHours}
          onValueChange={(v) => set("maxTtlHours", v)}
          min={1}
          max={168}
          error={errors.maxTtlHours}
        />
        <NumberInput
          label="Maximum queue wait (minutes)"
          value={values.maxQueueWaitMinutes}
          onValueChange={(v) => set("maxQueueWaitMinutes", v)}
          min={1}
          error={errors.maxQueueWaitMinutes}
        />
      </div>
      <div className="field-row">
        <Select
          label="Priority"
          value={String(values.priority)}
          onValueChange={(v) => v && set("priority", Number(v) as Priority)}
        >
          {creating ? (
            // Only CreateRunnerProfile resolves UNSPECIFIED to the default
            // of the workload kind; UpdateRunnerProfile requires a priority.
            <SelectOption value={String(Priority.UNSPECIFIED)}>
              Default of the workload kind
            </SelectOption>
          ) : null}
          {PRIORITIES.map((p) => (
            <SelectOption key={p} value={String(p)}>
              {labels.priority[p]}
            </SelectOption>
          ))}
        </Select>
        <NumberInput
          label="Warm count"
          description="Running sandboxes that wait for a request."
          value={values.warmCount}
          onValueChange={(v) => set("warmCount", v)}
          min={0}
          max={10}
          error={errors.warmCount}
        />
      </div>
      <p className="muted">
        Network: {labels.networkMode[NetworkMode.OUTBOUND]}.
      </p>
    </FormDialog>
  );
}
