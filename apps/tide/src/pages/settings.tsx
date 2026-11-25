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
import { NumberInput } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { Role } from "@delino/tide-protos/v1/common_pb";
import { MemberService } from "@delino/tide-protos/v1/member_pb";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { type FormEvent, useState } from "react";
import { invalidateServices } from "../api.ts";
import { MemberListStatus } from "../components/member-list-status.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ErrorNotice,
  PageHeader,
  showsFullError,
} from "../components/ui.tsx";
import { devSessionLimitError } from "../forms.ts";
import { displayName, useMembers } from "../members.ts";
import { can, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

/**
 * Route `settings`: the per-user dev session limit (`admin`), ownership
 * transfer and organization deletion (`owner`).
 */
export function SettingsPage() {
  const { role } = useOrg();
  return (
    <>
      <PageHeader title="Settings" />
      <DevSessionLimit />
      {can(role, Permission.TransferOwnership) ? <OwnerActions /> : null}
    </>
  );
}

function DevSessionLimit() {
  const { organizationId, organization } = useOrg();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const stored = organization.settings?.maxDevSessionsPerUser ?? null;
  const [draft, setDraft] = useState<{ value: number | null }>();
  const value = draft ? draft.value : stored;
  const hasChanges = draft !== undefined && draft.value !== stored;
  const [error, setError] = useState<string>();
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
    const found = devSessionLimitError(value);
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
          ? "Dev sessions have no per-user limit."
          : "The dev session limit is saved.",
      );
    } catch {
      // The notice below shows the error.
    }
  };
  return (
    <section className="section" aria-labelledby="limit-title">
      <h2 id="limit-title">Dev sessions per user</h2>
      <p className="muted">
        The most dev sessions that one user can have queued, starting or running
        at the same time. Leave it empty for no limit. Lowering it ends no
        existing dev session.
      </p>
      <form className="inline-form" onSubmit={submit} noValidate>
        <NumberInput
          label="Limit per user"
          value={value}
          onValueChange={(v) => {
            setDraft({ value: v });
            setError(undefined);
          }}
          min={1}
          error={error}
          autoFocus
        />
        <Button type="submit" disabled={!hasChanges || update.isPending}>
          Save limit
        </Button>
      </form>
      {update.error ? <ErrorNotice error={update.error} /> : null}
    </section>
  );
}

function OwnerActions() {
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
  const [confirmTransfer, setConfirmTransfer] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const hasRoster = roster.hasData && !showsFullError(roster.query);
  const candidates = hasRoster
    ? roster.members.filter((m) => m.role !== Role.OWNER)
    : [];
  const candidate = candidates.find((m) => m.userId === target);

  return (
    <>
      <section className="section" aria-labelledby="transfer-title">
        <h2 id="transfer-title">Transfer ownership</h2>
        <p className="muted">
          The new owner gets every permission. You become an admin.
        </p>
        <MemberListStatus roster={roster} />
        <div className="inline-form">
          <Select
            disabled={!hasRoster}
            label="New owner"
            placeholder="Choose a member"
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
            onClick={() => setConfirmTransfer(true)}
          >
            Transfer
          </Button>
        </div>
      </section>
      <section className="section danger-zone" aria-labelledby="delete-title">
        <h2 id="delete-title">Delete organization</h2>
        <p className="muted">
          Cancels every queued request, deletes every runner and removes the
          OwnSand link, the GitHub installations, the agent integrations and
          every member. The tide GitHub App stays installed in GitHub: uninstall
          it in the settings of each GitHub organization, because tide cannot
          uninstall it.
        </p>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirmDelete(true)}
        >
          Delete organization
        </Button>
      </section>
      <ConfirmDialog
        open={confirmTransfer && candidate !== undefined}
        onOpenChange={setConfirmTransfer}
        title={`Make ${candidate ? displayName(candidate) : ""} the owner?`}
        description="You become an admin. Only the new owner can give ownership back."
        confirmLabel="Transfer ownership"
        onConfirm={() =>
          transfer.mutateAsync({ organizationId, userId: candidate!.userId })
        }
      />
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={`Delete ${name}?`}
        description="Every runner is deleted and every queued request is canceled. Uninstall the tide GitHub App in GitHub yourself, because tide cannot uninstall it. This cannot be undone."
        confirmLabel="Delete organization"
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
