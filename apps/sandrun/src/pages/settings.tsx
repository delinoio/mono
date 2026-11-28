// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { FieldMaskSchema } from "@bufbuild/protobuf/wkt";
import {
  createConnectQueryKey,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "delino-ui/toast";
import { Button, ButtonVariant } from "delino-ui/button";
import { NumberInput, type NumberInputDraft } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { Role } from "@delino/sandrun-protos/v1/common_pb";
import { MemberService } from "@delino/sandrun-protos/v1/member_pb";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import { type FormEvent, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { MemberListStatus } from "../components/member-list-status.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ErrorNotice,
  PageHeader,
  showsFullError,
} from "../components/ui.tsx";
import { DEV_SESSION_LIMIT_MAX, devSessionLimitError } from "../forms.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { displayName, useMembers } from "../members.ts";
import { can, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

/**
 * Route `settings`: the per-user dev session limit (`admin`), ownership
 * transfer and organization deletion (`owner`).
 */
export function SettingsPage() {
  const { t } = useI18n();
  const { role } = useOrg();
  return (
    <>
      <PageHeader title={t("settings.title")} />
      <DevSessionLimit />
      {can(role, Permission.TransferOwnership) ? <OwnerActions /> : null}
    </>
  );
}

function DevSessionLimit() {
  const { t } = useI18n();
  const { organizationId, organization } = useOrg();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const stored = organization.settings?.maxDevSessionsPerUser ?? null;
  const inputDraft = useRef<NumberInputDraft>(null);
  const [draft, setDraft] = useState<{
    value: number | null;
    invalid: boolean;
  }>();
  const value = draft ? draft.value : stored;
  const hasChanges = draft !== undefined && (draft.invalid || value !== stored);
  const [error, setError] = useState<MessageKey>();
  const update = useMutation(
    OrganizationService.method.updateOrganizationSettings,
    {
      onSuccess: (saved) => {
        const confirmed = saved.organization;
        if (confirmed) {
          const queryKey = createConnectQueryKey({
            schema: OrganizationService.method.getOrganization,
            input: { organizationId },
            transport,
            cardinality: "finite",
          });
          // Keep the confirmed settings available if the follow-up read fails.
          queryClient.setQueryData(queryKey, (current) =>
            current?.organization
              ? {
                  ...current,
                  organization: {
                    ...current.organization,
                    settings: confirmed.settings,
                  },
                }
              : current,
          );
        }
        return invalidateServices(queryClient, OrganizationService);
      },
    },
  );
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!hasChanges || update.isPending) return;
    const found = devSessionLimitError(
      inputDraft.current?.isInvalid() ? Number.NaN : value,
    );
    if (found !== undefined) {
      setError(found);
      return;
    }
    try {
      await update.mutateAsync({
        organizationId,
        settings: value === null ? {} : { maxDevSessionsPerUser: value },
        updateMask: create(FieldMaskSchema, {
          paths: ["max_dev_sessions_per_user"],
        }),
      });
      // Each edit creates a new draft, even if it returns to the submitted value.
      setDraft((current) => (current === draft ? undefined : current));
      toast.success(
        value === null
          ? t("settings.devSessionLimit.removed")
          : t("settings.devSessionLimit.saved"),
      );
    } catch {
      // The notice below shows the error.
    }
  };
  return (
    <section className="section" aria-labelledby="limit-title">
      <h2 id="limit-title">{t("settings.devSessionLimit.title")}</h2>
      <p className="muted">{t("settings.devSessionLimit.description")}</p>
      <form className="inline-form" onSubmit={submit} noValidate>
        <NumberInput
          label={t("settings.devSessionLimit.label")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
          value={value}
          onValueChange={(v) => {
            setDraft({ value: v, invalid: false });
            setError(undefined);
          }}
          onDraftChange={(invalid) => {
            // The SDK emits no numeric change for text outside the bounds.
            setDraft((current) =>
              invalid || current?.invalid
                ? { value: current ? current.value : stored, invalid }
                : current,
            );
            setError(undefined);
          }}
          min={1}
          max={DEV_SESSION_LIMIT_MAX}
          draftRef={inputDraft}
          validateDraftBounds
          error={error && t(error)}
          autoFocus
        />
        <Button type="submit" disabled={!hasChanges || update.isPending}>
          {t("settings.devSessionLimit.save")}
        </Button>
      </form>
      {update.error ? <ErrorNotice error={update.error} /> : null}
    </section>
  );
}

function OwnerActions() {
  const { t } = useI18n();
  const { organizationId, name } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const roster = useMembers(organizationId);
  const transfer = useMutation(OrganizationService.method.transferOwnership, {
    onSuccess: () =>
      invalidateServices(queryClient, OrganizationService, MemberService),
  });
  const remove = useMutation(OrganizationService.method.deleteOrganization);
  const [target, setTarget] = useState<string | null>(null);
  const [transferTarget, setTransferTarget] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const hasRoster = roster.hasData && !showsFullError(roster.query);
  const candidates = hasRoster
    ? roster.members.filter((m) => m.role !== Role.OWNER)
    : [];
  const candidate = candidates.find((m) => m.userId === target);
  const transferCandidate =
    candidate?.userId === transferTarget ? candidate : undefined;
  if (transferTarget !== null && transferCandidate === undefined) {
    setTransferTarget(null);
  }

  return (
    <>
      <section className="section" aria-labelledby="transfer-title">
        <h2 id="transfer-title">{t("settings.transfer.title")}</h2>
        <p className="muted">{t("settings.transfer.description")}</p>
        <MemberListStatus roster={roster} />
        <div className="inline-form">
          <Select
            disabled={!hasRoster}
            label={t("settings.transfer.newOwner")}
            placeholder={t("settings.transfer.placeholder")}
            value={candidate?.userId ?? null}
            onValueChange={setTarget}
          >
            {candidates.map((m) => (
              <SelectOption key={m.userId} value={m.userId}>
                {displayName(m)}
              </SelectOption>
            ))}
          </Select>
          <Button
            variant={ButtonVariant.Secondary}
            disabled={candidate === undefined}
            onClick={() => setTransferTarget(candidate?.userId ?? null)}
          >
            {t("settings.transfer.action")}
          </Button>
        </div>
      </section>
      <section className="section danger-zone" aria-labelledby="delete-title">
        <h2 id="delete-title">{t("settings.delete.title")}</h2>
        <p className="muted">{t("settings.delete.description")}</p>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirmDelete(true)}
        >
          {t("settings.delete.title")}
        </Button>
      </section>
      <ConfirmDialog
        open={transferCandidate !== undefined}
        onOpenChange={(open) => {
          if (!open) setTransferTarget(null);
        }}
        title={t("settings.transferDialog.title", {
          name: candidate ? displayName(candidate) : "",
        })}
        description={t("settings.transferDialog.description")}
        confirmLabel={t("settings.transfer.title")}
        onConfirm={async () => {
          if (transferCandidate === undefined) return;
          await transfer.mutateAsync({
            organizationId,
            userId: transferCandidate.userId,
          });
        }}
      />
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t("settings.deleteDialog.title", { name })}
        description={t("settings.deleteDialog.description")}
        confirmLabel={t("settings.delete.title")}
        onConfirm={async (opening) => {
          await remove.mutateAsync({ organizationId });
          // Refreshing the old route can unmount the confirmation.
          if (opening.isCurrent()) navigate("/");
          await invalidateServices(queryClient, OrganizationService);
        }}
      />
    </>
  );
}
