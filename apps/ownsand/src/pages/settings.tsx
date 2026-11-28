// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import {
  createConnectQueryKey,
  useMutation,
  useQuery,
  useTransport,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "delino-ui/segmented-control";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { BillingService } from "@delino/ownsand-protos/v1/billing_pb";
import {
  AgentVersionPolicyMode,
  Role,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  DeviceService,
  GetAgentVersionPolicyResponseSchema,
} from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { type FormEvent, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  ListNoun,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  toastError,
  isTerminalError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { useMembers } from "../members.ts";
import { can, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";
import { enumText } from "../text.ts";
import { BillingSection } from "./billing-settings.tsx";

const MODES = [
  AgentVersionPolicyMode.FOLLOW_DEFAULT,
  AgentVersionPolicyMode.PIN,
  AgentVersionPolicyMode.HOLD,
] as const;

/**
 * Route `settings`: the agent version policy (read for members, changed by
 * admins), billing (owner and admins; linking by the owner), ownership
 * transfer and organization deletion (owner).
 */
export function SettingsPage() {
  const { organizationId, name, role } = useOrg();
  const i18n = useI18n();
  const { t } = i18n;
  return (
    <>
      <PageHeader title={t("settings.title")} />
      <dl className="details">
        <Detail term={t("settings.organization")}>{name}</Detail>
        <Detail term={t("settings.organizationId")}>
          <code>{organizationId}</code>
        </Detail>
        <Detail term={t("settings.yourRole")}>
          {enumText(i18n, labels.role, role)}
        </Detail>
      </dl>
      <AgentPolicy />
      {can(role, Permission.ViewBilling) ? <BillingSection /> : null}
      {can(role, Permission.TransferOwnership) ? <OwnerActions /> : null}
    </>
  );
}

function AgentPolicy() {
  const { organizationId, role } = useOrg();
  const i18n = useI18n();
  const { t, lang } = i18n;
  const queryClient = useQueryClient();
  const transport = useTransport();
  const policy = useQuery(DeviceService.method.getAgentVersionPolicy, {
    organizationId,
  });
  const update = useMutation(DeviceService.method.updateAgentVersionPolicy, {
    onSuccess: ({ policy }, { organizationId }) => {
      if (policy !== undefined) {
        // Use the confirmed policy if the readback fails.
        queryClient.setQueryData(
          createConnectQueryKey({
            schema: DeviceService.method.getAgentVersionPolicy,
            input: { organizationId },
            transport,
            cardinality: "finite",
          }),
          create(GetAgentVersionPolicyResponseSchema, { policy }),
        );
      }
      return invalidateServices(queryClient, DeviceService);
    },
  });
  const [draft, setDraft] = useState<{
    mode?: AgentVersionPolicyMode;
    version?: string;
  }>({});
  const [versionError, setVersionError] = useState<string>();
  const editable = can(role, Permission.ChangeAgentVersionPolicy);
  const current = policy.data?.policy;
  const mode =
    draft.mode ?? (current?.mode || AgentVersionPolicyMode.FOLLOW_DEFAULT);
  const version = draft.version ?? current?.pinnedVersion ?? "";
  const pinnedVersion = version.trim();
  const isPin = mode === AgentVersionPolicyMode.PIN;
  // The mutation stays pending through the readback. The fields stay disabled
  // until the submitted draft is cleared. The ref guards a second submission
  // in the same tick, before React disables the button.
  const saving = useRef(false);
  const pending = update.isPending;

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saving.current) return;
    if (isPin && pinnedVersion === "") {
      setVersionError(t("agentPolicy.versionRequired", { example: "1.4.2" }));
      return;
    }
    saving.current = true;
    try {
      await update.mutateAsync({
        organizationId,
        mode,
        pinnedVersion: isPin ? pinnedVersion : "",
      });
      setDraft({});
      setVersionError(undefined);
      toast.success(t("agentPolicy.saved"));
    } catch (error) {
      // The draft stays as the user left it and the form is usable again.
      toastError(error, i18n);
    } finally {
      saving.current = false;
    }
  };

  return (
    <section className="section" aria-labelledby="policy-title">
      <h2 id="policy-title">{t("agentPolicy.title")}</h2>
      <p className="muted">{t("agentPolicy.description")}</p>
      <QueryBoundary query={policy} loading={t("agentPolicy.loading")}>
        {() => (
          <form className="form" onSubmit={save}>
            <SegmentedControl
              aria-label={t("agentPolicy.mode")}
              value={String(mode)}
              onValueChange={(value) => {
                const selectedMode = MODES.find(
                  (candidate) => String(candidate) === value,
                );
                if (selectedMode !== undefined) {
                  setDraft((draft) => ({ ...draft, mode: selectedMode }));
                }
              }}
              disabled={!editable || pending}
            >
              {MODES.map((m) => (
                <SegmentedControlItem key={m} value={String(m)}>
                  {enumText(i18n, labels.policyMode, m)}
                </SegmentedControlItem>
              ))}
            </SegmentedControl>
            {isPin ? (
              <TextField
                label={t("agentPolicy.pinnedVersion")}
                value={version}
                onValueChange={(v) => {
                  setDraft((draft) => ({ ...draft, version: v }));
                  setVersionError(undefined);
                }}
                error={pinnedVersion === "" ? versionError : undefined}
                disabled={!editable || pending}
              />
            ) : null}
            {current?.updateTime ? (
              <p className="muted">
                {t("agentPolicy.lastChanged", {
                  time: formatTime(current.updateTime, lang),
                })}
              </p>
            ) : null}
            {editable ? (
              <div className="actions">
                <Button type="submit" disabled={pending}>
                  {t("agentPolicy.save")}
                </Button>
              </div>
            ) : (
              <p className="muted">{t("agentPolicy.adminsOnly")}</p>
            )}
          </form>
        )}
      </QueryBoundary>
    </section>
  );
}

function OwnerActions() {
  const { organizationId, name } = useOrg();
  const { t } = useI18n();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  // Read every page: the new owner can be a member that sorts after the
  // first 200.
  const members = useMembers(organizationId);
  const transfer = useMutation(OrganizationService.method.transferOwnership, {
    onSuccess: () =>
      invalidateServices(
        queryClient,
        OrganizationService,
        MemberService,
        BillingService,
      ),
  });
  // No `onSuccess` refetch: a refetch of the deleted organization's queries
  // fails with `not_found` and would render that error before the
  // navigation. The confirmation refreshes the cache after it navigates.
  const remove = useMutation(OrganizationService.method.deleteOrganization);
  const [target, setTarget] = useState<string | null>(null);
  const [confirmTransfer, setConfirmTransfer] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const terminalMembersError = isTerminalError(members.error);
  if (terminalMembersError && (target !== null || confirmTransfer)) {
    // Discard the selection and its confirmation when the list loses access.
    setTarget(null);
    setConfirmTransfer(false);
  }

  const candidates = members.members.filter((m) => m.role !== Role.OWNER);
  const selectedMember = candidates.find((m) => m.userId === target);
  const targetName =
    selectedMember?.displayName || selectedMember?.userId || "";

  return (
    <>
      <section className="section" aria-labelledby="transfer-title">
        <h2 id="transfer-title">{t("ownership.title")}</h2>
        <p className="muted">{t("ownership.description")}</p>
        {terminalMembersError ? (
          <ErrorNotice
            error={members.error}
            onRetry={() => void members.refetch()}
          />
        ) : (
          <>
            <div className="inline-form">
              <Select
                label={t("ownership.newOwner")}
                placeholder={t("ownership.chooseMember")}
                value={target}
                onValueChange={setTarget}
              >
                {candidates.map((m) => (
                  <SelectOption key={m.userId} value={m.userId}>
                    {m.displayName || m.userId}
                  </SelectOption>
                ))}
              </Select>
              <Button
                variant={ButtonVariant.Secondary}
                disabled={target === null}
                onClick={() => setConfirmTransfer(true)}
              >
                {t("ownership.transfer")}
              </Button>
            </div>
            <MorePagesNotice noun={ListNoun.Members} pages={members} />
          </>
        )}
      </section>
      <section className="section danger-zone" aria-labelledby="delete-title">
        <h2 id="delete-title">{t("deleteOrganization.title")}</h2>
        <p className="muted">{t("deleteOrganization.description")}</p>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirmDelete(true)}
        >
          {t("deleteOrganization.title")}
        </Button>
      </section>
      <ConfirmDialog
        open={confirmTransfer}
        onOpenChange={setConfirmTransfer}
        title={t("ownership.dialog.title", { name: targetName })}
        description={t("ownership.dialog.description")}
        confirmLabel={t("ownership.title")}
        onConfirm={async () => {
          if (terminalMembersError || target === null) return;
          await transfer.mutateAsync({
            organizationId,
            userId: target,
          });
        }}
      />
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t("deleteOrganization.dialog.title", { name })}
        description={t("deleteOrganization.dialog.description")}
        confirmLabel={t("deleteOrganization.title")}
        onConfirm={async (opening) => {
          await remove.mutateAsync({ organizationId });
          // Only the opening that asked for the deletion redirects; after a
          // dismissal the user stays on the page they went to. The refresh
          // runs either way, after the navigation decision, so a redirected
          // user does not refetch the deleted organization's queries.
          if (opening.isCurrent()) navigate("/");
          void invalidateServices(queryClient, OrganizationService);
        }}
      />
    </>
  );
}
