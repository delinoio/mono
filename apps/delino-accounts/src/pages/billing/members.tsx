// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { useState } from "react";
import { invalidateServices } from "../../api.ts";
import { useAuth } from "../../auth.tsx";
import { useActionFailure } from "../../components/action-failure.tsx";
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  RefetchNotice,
  FormDialog,
  formInvalid,
  formSubmitted,
  Loading,
  MorePages,
} from "../../components/ui.tsx";
import { checkEmail } from "../../forms.ts";
import { formatDate, labels } from "../../format.ts";
import { useI18n } from "../../i18n.ts";
import { useAllPages } from "../../paging.ts";
import {
  type BillingAccountMember,
  BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { can, canManageMember, Permission } from "../../roles.ts";
import { parseRoute, RouteId, routePath, useRouter } from "../../router.tsx";

function displayName(member: BillingAccountMember): string {
  return member.displayName !== "" ? member.displayName : member.userId;
}

/**
 * The members tab: members and roles, removal, leaving, ownership transfer
 * and email invitations.
 */
export function MembersSection() {
  const { billingAccountId, role } = useBillingAccount();
  const { userId } = useAuth();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const roleFailure = useActionFailure();
  const { lang, t } = useI18n();
  const refresh = () => invalidateServices(queryClient, BillingAccountService);
  const members = useAllPages(
    BillingAccountService.method.listMembers,
    { billingAccountId },
    (page) => ({ items: page.members, nextPageToken: page.nextPageToken }),
    (m) => m.userId,
  );
  const updateRole = useMutation(
    BillingAccountService.method.updateMemberRole,
    {
      onSuccess: refresh,
    },
  );
  const removeMember = useMutation(BillingAccountService.method.removeMember, {
    onSuccess: refresh,
  });
  // Leaving has no `onSuccess` refresh: the leaving user can no longer read
  // the account, so refetching its queries would render the not-found notice
  // before the page navigates away. A per-call `onSuccess` would not replace
  // the hook-level one, so leaving uses its own mutation.
  const leave = useMutation(BillingAccountService.method.removeMember);
  const transfer = useMutation(BillingAccountService.method.transferOwnership, {
    onSuccess: refresh,
  });
  const [removingId, setRemovingId] = useState<string>();
  const [transferringId, setTransferringId] = useState<string>();
  const [leaving, setLeaving] = useState(false);
  const canLeave = can(role, Permission.Leave);
  const canTransfer = can(role, Permission.TransferOwnership);
  const manageable = (member: BillingAccountMember) =>
    member.userId !== userId && canManageMember(role, member.role);
  const removing = members.items.find(
    (member) => member.userId === removingId && manageable(member),
  );
  const transferring = canTransfer
    ? members.items.find(
        (member) => member.userId === transferringId && manageable(member),
      )
    : undefined;

  // Clear invalid selections before the dialog can render again.
  if (leaving && !canLeave) {
    setLeaving(false);
  }
  if (removingId !== undefined && removing === undefined) {
    setRemovingId(undefined);
  }
  if (transferringId !== undefined && transferring === undefined) {
    setTransferringId(undefined);
  }

  return (
    <div className="stack">
      <section className="section" aria-labelledby="members-title">
        <div className="section-header">
          <h2 id="members-title">{t("members.title")}</h2>
          {canLeave ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setLeaving(true)}
            >
              {t("members.leave")}
            </Button>
          ) : null}
        </div>
        <RefetchNotice error={members.refreshError} onRetry={members.refetch} />
        {roleFailure.notice}
        {members.isPending ? (
          <Loading label={t("members.loading")} />
        ) : members.error ? (
          <ErrorNotice error={members.error} onRetry={members.refetch} />
        ) : (
          <>
            <ul className="list" aria-label={t("members.title")}>
              {members.items.map((m) => {
                const self = m.userId === userId;
                return (
                  <li key={m.userId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        {displayName(m)}
                        {self ? (
                          <span className="muted"> {t("members.you")}</span>
                        ) : null}
                      </span>
                      <span className="muted">
                        {t("members.joined", {
                          date: formatDate(m.createTime, lang),
                        })}
                      </span>
                    </div>
                    {manageable(m) ? (
                      <div className="list-actions">
                        <Select
                          label={t("members.roleOf", {
                            name: displayName(m),
                          })}
                          value={String(m.role)}
                          onValueChange={async (v) => {
                            if (v === null || Number(v) === m.role) return;
                            try {
                              await updateRole.mutateAsync({
                                billingAccountId,
                                userId: m.userId,
                                role: Number(v) as BillingAccountRole,
                              });
                              roleFailure.clear();
                              toast.success(t("members.roleChanged"));
                            } catch (error) {
                              roleFailure.report(error);
                            }
                          }}
                        >
                          <SelectOption
                            value={String(BillingAccountRole.ADMIN)}
                          >
                            {t(labels.role[BillingAccountRole.ADMIN])}
                          </SelectOption>
                          <SelectOption
                            value={String(BillingAccountRole.VIEWER)}
                          >
                            {t(labels.role[BillingAccountRole.VIEWER])}
                          </SelectOption>
                        </Select>
                        {canTransfer ? (
                          <Button
                            variant={ButtonVariant.Secondary}
                            size={ButtonSize.Small}
                            onClick={() => setTransferringId(m.userId)}
                          >
                            {t("members.makeOwner")}
                          </Button>
                        ) : null}
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          onClick={() => setRemovingId(m.userId)}
                        >
                          {t("members.remove")}
                        </Button>
                      </div>
                    ) : (
                      <span className="role">{t(labels.role[m.role])}</span>
                    )}
                  </li>
                );
              })}
            </ul>
            <MorePages pages={members} />
          </>
        )}
      </section>
      {can(role, Permission.ManageMembers) ? <Invitations /> : null}
      <ConfirmDialog
        open={removing !== undefined}
        onOpenChange={(open) => !open && setRemovingId(undefined)}
        title={t("members.removeDialog.title", {
          name: removing ? displayName(removing) : "",
        })}
        description={t("members.removeDialog.description")}
        confirmLabel={t("members.removeDialog.confirm")}
        onConfirm={async () => {
          if (removing === undefined) return;
          await removeMember.mutateAsync({
            billingAccountId,
            userId: removing.userId,
          });
        }}
      />
      <ConfirmDialog
        open={transferring !== undefined}
        onOpenChange={(open) => !open && setTransferringId(undefined)}
        title={t("members.transferDialog.title", {
          name: transferring ? displayName(transferring) : "",
        })}
        description={t("members.transferDialog.description")}
        confirmLabel={t("members.transferDialog.confirm")}
        onConfirm={async () => {
          if (transferring === undefined) return;
          await transfer.mutateAsync({
            billingAccountId,
            userId: transferring.userId,
          });
          toast.success(t("members.transferred"));
        }}
      />
      <ConfirmDialog
        open={leaving && canLeave}
        onOpenChange={setLeaving}
        title={t("members.leaveDialog.title")}
        description={t("members.leaveDialog.description")}
        confirmLabel={t("members.leave")}
        onConfirm={async () => {
          if (!canLeave) return;
          await leave.mutateAsync({
            billingAccountId,
            userId: userId ?? "",
          });
          // Read the live route after the RPC. Move off this account before
          // refreshing because the caller can no longer read it.
          const route = parseRoute(window.location.pathname);
          if (
            route.id === RouteId.BillingAccount &&
            route.billingAccountId === billingAccountId
          ) {
            navigate(routePath({ id: RouteId.Billing }));
          }
          await refresh();
        }}
      />
    </div>
  );
}

function Invitations() {
  const { billingAccountId } = useBillingAccount();
  const queryClient = useQueryClient();
  const revokeFailure = useActionFailure();
  const i18n = useI18n();
  const { lang, t } = i18n;
  const refresh = () => invalidateServices(queryClient, BillingAccountService);
  const list = useAllPages(
    BillingAccountService.method.listInvitations,
    { billingAccountId },
    (page) => ({ items: page.invitations, nextPageToken: page.nextPageToken }),
    (i) => i.invitationId,
  );
  const create = useMutation(BillingAccountService.method.createInvitation, {
    onSuccess: refresh,
  });
  const revoke = useMutation(BillingAccountService.method.revokeInvitation, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [email, setEmail] = useState("");
  const [emailError, setEmailError] = useState<string>();
  const [inviteRole, setInviteRole] = useState(
    String(BillingAccountRole.VIEWER),
  );
  const openInvite = (open: boolean) => {
    if (!open) {
      setEmail("");
      setEmailError(undefined);
      setInviteRole(String(BillingAccountRole.VIEWER));
    }
    setCreating(open);
  };

  return (
    <section className="section" aria-labelledby="invitations-title">
      <div className="section-header">
        <h2 id="invitations-title">{t("invitations.title")}</h2>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setCreating(true)}
        >
          {t("invitations.invite")}
        </Button>
      </div>
      <RefetchNotice error={list.refreshError} onRetry={list.refetch} />
      {revokeFailure.notice}
      {list.isPending ? (
        <Loading label={t("invitations.loading")} />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={list.refetch} />
      ) : list.items.length === 0 ? (
        <Empty title={t("invitations.empty")} />
      ) : (
        <>
          <ul className="list" aria-label={t("invitations.list")}>
            {list.items.map((i) => (
              <li key={i.invitationId} className="list-row">
                <div className="list-main">
                  <span className="list-title">{i.email}</span>
                  <span className="muted">
                    {t("invitations.details", {
                      role: t(labels.role[i.role]),
                      date: formatDate(i.expireTime, lang),
                    })}
                  </span>
                </div>
                <Button
                  variant={ButtonVariant.Secondary}
                  size={ButtonSize.Small}
                  onClick={async () => {
                    try {
                      await revoke.mutateAsync({
                        billingAccountId,
                        invitationId: i.invitationId,
                      });
                      revokeFailure.clear();
                      toast.success(t("invitations.revoked"));
                    } catch (error) {
                      revokeFailure.report(error);
                    }
                  }}
                >
                  {t("invitations.revoke")}
                </Button>
              </li>
            ))}
          </ul>
          <MorePages pages={list} />
        </>
      )}
      <FormDialog
        open={creating}
        onOpenChange={openInvite}
        title={t("invitations.invite")}
        description={t("invitations.dialog.description")}
        submitLabel={t("invitations.dialog.submit")}
        onSubmit={async () => {
          const checked = checkEmail(email, i18n);
          if (!checked.ok) {
            setEmailError(checked.error);
            return formInvalid();
          }
          setEmailError(undefined);
          await create.mutateAsync({
            billingAccountId,
            email: checked.value,
            role: Number(inviteRole) as BillingAccountRole,
          });
          // The list refreshes in the mutation's `onSuccess` for every sent
          // invitation; the close, reset and toast belong to this opening.
          return formSubmitted(() => {
            openInvite(false);
            toast.success(t("invitations.sent", { email: checked.value }));
          });
        }}
      >
        <TextField
          label={t("invitations.dialog.email")}
          type="email"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          error={emailError}
          autoFocus
          autoComplete="off"
        />
        <Select
          label={t("invitations.dialog.role")}
          value={inviteRole}
          onValueChange={(v) => v && setInviteRole(v)}
        >
          <SelectOption value={String(BillingAccountRole.VIEWER)}>
            {t("invitations.dialog.roleViewer")}
          </SelectOption>
          <SelectOption value={String(BillingAccountRole.ADMIN)}>
            {t("invitations.dialog.roleAdmin")}
          </SelectOption>
        </Select>
      </FormDialog>
    </section>
  );
}
