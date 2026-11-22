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
import { useBillingAccount } from "../../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
  MorePages,
} from "../../components/ui.tsx";
import { checkEmail } from "../../forms.ts";
import { errorMessage, formatDate, labels } from "../../format.ts";
import { useAllPages } from "../../paging.ts";
import {
  type BillingAccountMember,
  BillingAccountRole,
  BillingAccountService,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { can, canManageMember, Permission } from "../../roles.ts";
import { RouteId, routePath, useRouter } from "../../router.tsx";

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
  const [removing, setRemoving] = useState<BillingAccountMember>();
  const [transferring, setTransferring] = useState<BillingAccountMember>();
  const [leaving, setLeaving] = useState(false);

  return (
    <div className="stack">
      <section className="section" aria-labelledby="members-title">
        <div className="section-header">
          <h2 id="members-title">Members</h2>
          {can(role, Permission.Leave) ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setLeaving(true)}
            >
              Leave billing account
            </Button>
          ) : null}
        </div>
        {members.isPending ? (
          <Loading label="Loading members" />
        ) : members.error ? (
          <ErrorNotice error={members.error} onRetry={members.refetch} />
        ) : (
          <>
            <ul className="list" aria-label="Members">
              {members.items.map((m) => {
                const self = m.userId === userId;
                const manageable = !self && canManageMember(role, m.role);
                return (
                  <li key={m.userId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        {displayName(m)}
                        {self ? <span className="muted"> (you)</span> : null}
                      </span>
                      <span className="muted">
                        Joined {formatDate(m.createTime)}
                      </span>
                    </div>
                    {manageable ? (
                      <div className="list-actions">
                        <Select
                          label={`Role of ${displayName(m)}`}
                          value={String(m.role)}
                          onValueChange={async (v) => {
                            if (v === null || Number(v) === m.role) return;
                            try {
                              await updateRole.mutateAsync({
                                billingAccountId,
                                userId: m.userId,
                                role: Number(v) as BillingAccountRole,
                              });
                              toast.success("The role is changed.");
                            } catch (error) {
                              toast.error(errorMessage(error));
                            }
                          }}
                        >
                          <SelectOption
                            value={String(BillingAccountRole.ADMIN)}
                          >
                            Admin
                          </SelectOption>
                          <SelectOption
                            value={String(BillingAccountRole.VIEWER)}
                          >
                            Viewer
                          </SelectOption>
                        </Select>
                        {can(role, Permission.TransferOwnership) ? (
                          <Button
                            variant={ButtonVariant.Secondary}
                            size={ButtonSize.Small}
                            onClick={() => setTransferring(m)}
                          >
                            Make owner
                          </Button>
                        ) : null}
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          onClick={() => setRemoving(m)}
                        >
                          Remove
                        </Button>
                      </div>
                    ) : (
                      <span className="role">{labels.role[m.role]}</span>
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
        onOpenChange={(open) => !open && setRemoving(undefined)}
        title={`Remove ${removing ? displayName(removing) : ""}?`}
        description="They lose access to this billing account. Linked organizations stay linked."
        confirmLabel="Remove member"
        onConfirm={() =>
          removeMember.mutateAsync({
            billingAccountId,
            userId: removing!.userId,
          })
        }
      />
      <ConfirmDialog
        open={transferring !== undefined}
        onOpenChange={(open) => !open && setTransferring(undefined)}
        title={`Make ${transferring ? displayName(transferring) : ""} the owner?`}
        description="There is only one owner. You become an admin and can no longer delete the billing account or transfer ownership."
        confirmLabel="Transfer ownership"
        onConfirm={async () => {
          await transfer.mutateAsync({
            billingAccountId,
            userId: transferring!.userId,
          });
          toast.success("Ownership is transferred.");
        }}
      />
      <ConfirmDialog
        open={leaving}
        onOpenChange={setLeaving}
        title="Leave this billing account?"
        description="You lose access to its balance, usage and settings. An owner or admin can invite you again."
        confirmLabel="Leave billing account"
        onConfirm={async () => {
          await leave.mutateAsync({
            billingAccountId,
            userId: userId ?? "",
          });
          // Leave the account first, so that the invalidation does not
          // refetch its queries (they are now `not_found` for this user).
          navigate(routePath({ id: RouteId.Billing }));
          await refresh();
        }}
      />
    </div>
  );
}

function Invitations() {
  const { billingAccountId } = useBillingAccount();
  const queryClient = useQueryClient();
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
        <h2 id="invitations-title">Invitations</h2>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setCreating(true)}
        >
          Invite by email
        </Button>
      </div>
      {list.isPending ? (
        <Loading label="Loading invitations" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={list.refetch} />
      ) : list.items.length === 0 ? (
        <Empty title="No pending invitations" />
      ) : (
        <>
          <ul className="list" aria-label="Pending invitations">
            {list.items.map((i) => (
              <li key={i.invitationId} className="list-row">
                <div className="list-main">
                  <span className="list-title">{i.email}</span>
                  <span className="muted">
                    {labels.role[i.role]} · Expires {formatDate(i.expireTime)}
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
                      toast.success("The invitation is revoked.");
                    } catch (error) {
                      toast.error(errorMessage(error));
                    }
                  }}
                >
                  Revoke
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
        title="Invite by email"
        description="We email a link that works once for 7 days. The person accepts it after signing in with this email address."
        submitLabel="Send invitation"
        onSubmit={async () => {
          const checked = checkEmail(email);
          if (!checked.ok) {
            setEmailError(checked.error);
            return;
          }
          setEmailError(undefined);
          await create.mutateAsync({
            billingAccountId,
            email: checked.value,
            role: Number(inviteRole) as BillingAccountRole,
          });
          openInvite(false);
          toast.success(`An invitation is sent to ${checked.value}.`);
        }}
      >
        <TextField
          label="Email"
          type="email"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          error={emailError}
          autoFocus
          autoComplete="off"
        />
        <Select
          label="Role"
          value={inviteRole}
          onValueChange={(v) => v && setInviteRole(v)}
        >
          <SelectOption value={String(BillingAccountRole.VIEWER)}>
            Viewer: reads balance, usage and settings
          </SelectOption>
          <SelectOption value={String(BillingAccountRole.ADMIN)}>
            Admin: manages payments and members
          </SelectOption>
        </Select>
      </FormDialog>
    </section>
  );
}
