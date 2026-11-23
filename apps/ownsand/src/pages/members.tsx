// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { InvitationState, Role } from "@delino/ownsand-protos/v1/common_pb";
import {
  type Invitation,
  type Member,
  MemberService,
} from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  FormDialog,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  toastError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { invitationLink } from "../invitation.ts";
import { membersBoundary, useMembers } from "../members.ts";
import { can, canManageMember, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

function displayName(m: Member): string {
  return m.displayName !== "" ? m.displayName : m.userId;
}

/** Route `members`: members, roles, leaving and invitations. */
export function MembersPage() {
  const { organizationId, role } = useOrg();
  const { userId } = useAuth();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const refresh = () =>
    invalidateServices(queryClient, MemberService, OrganizationService);
  // Read every page: a member that sorts after the first 200 would otherwise
  // be missing from the list and could not get a role change or a removal.
  const members = useMembers(organizationId);
  const updateRole = useMutation(MemberService.method.updateMemberRole, {
    onSuccess: refresh,
  });
  const removeMember = useMutation(MemberService.method.removeMember, {
    onSuccess: refresh,
  });
  // Leaving has no awaited refetch: the caller no longer has access to the
  // organization, so a refetch of its member and organization queries would
  // fail with `not_found` and render that error before the navigation.
  const leave = useMutation(MemberService.method.removeMember);
  const [removing, setRemoving] = useState<Member>();
  const [leaving, setLeaving] = useState(false);

  return (
    <>
      <PageHeader
        title="Members"
        description="People who can use this organization. Profiles come from your Delino account."
        action={
          can(role, Permission.LeaveOrganization) ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setLeaving(true)}
            >
              Leave organization
            </Button>
          ) : undefined
        }
      />
      <QueryBoundary query={membersBoundary(members)} loading="Loading members">
        {(loaded) => (
          <>
            <ul className="list" aria-label="Members">
              {loaded.map((m) => {
                const manageable = canManageMember(role, userId, m);
                return (
                  <li key={m.userId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        {displayName(m)}
                        {m.userId === userId ? (
                          <span className="muted"> (you)</span>
                        ) : null}
                      </span>
                      <span className="muted">
                        Joined {formatTime(m.joinTime)}
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
                                organizationId,
                                userId: m.userId,
                                role: Number(v) as Role,
                              });
                              toast.success("The role is changed.");
                            } catch (error) {
                              toastError(error);
                            }
                          }}
                        >
                          <SelectOption value={String(Role.ADMIN)}>
                            Admin
                          </SelectOption>
                          <SelectOption value={String(Role.MEMBER)}>
                            Member
                          </SelectOption>
                        </Select>
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          aria-label={`Remove ${displayName(m)}`}
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
            <MorePagesNotice noun="members" pages={members} />
          </>
        )}
      </QueryBoundary>
      {can(role, Permission.ManageInvitations) ? <Invitations /> : null}
      <ConfirmDialog
        open={removing !== undefined}
        onOpenChange={(open) => !open && setRemoving(undefined)}
        title={`Remove ${removing ? displayName(removing) : ""}?`}
        description="They lose access to this organization. Its sandboxes keep running."
        confirmLabel="Remove member"
        onConfirm={() =>
          removeMember.mutateAsync({ organizationId, userId: removing!.userId })
        }
      />
      <ConfirmDialog
        open={leaving}
        onOpenChange={setLeaving}
        title="Leave this organization?"
        description="You lose access to its devices and sandboxes. An admin can invite you again."
        confirmLabel="Leave organization"
        onConfirm={async (opening) => {
          await leave.mutateAsync({
            organizationId,
            userId: userId ?? "",
          });
          // Only the opening that asked to leave redirects; after a
          // dismissal the user stays on the page they went to.
          if (opening.isCurrent()) navigate("/");
          // After the navigation decision, so a redirected user refetches
          // only the organization list at `/`. It is not awaited and runs
          // for a dismissed opening too.
          void refresh();
        }}
      />
    </>
  );
}

function Invitations() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, MemberService);
  const list = useQuery(MemberService.method.listInvitations, {
    organizationId,
    pageSize: 200,
  });
  const create = useMutation(MemberService.method.createInvitation, {
    onSuccess: refresh,
  });
  const revoke = useMutation(MemberService.method.revokeInvitation, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [inviteRole, setInviteRole] = useState(String(Role.MEMBER));
  const [link, setLink] = useState<string>();
  const [revoking, setRevoking] = useState<Invitation>();
  const pending =
    list.data?.invitations.filter((i) => i.state === InvitationState.PENDING) ??
    [];

  return (
    <section className="section" aria-labelledby="invitations-title">
      <div className="section-header">
        <h2 id="invitations-title">Invitations</h2>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setCreating(true)}
        >
          Invite someone
        </Button>
      </div>
      <QueryBoundary query={list} loading="Loading invitations">
        {() =>
          pending.length === 0 ? (
            <Empty title="No pending invitations" />
          ) : (
            <ul className="list" aria-label="Pending invitations">
              {pending.map((i) => (
                <li key={i.invitationId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {labels.role[i.role]} invitation{" "}
                      <code>{i.secretPrefix}…</code>
                    </span>
                    <span className="muted">
                      Expires {formatTime(i.expireTime)}
                    </span>
                  </div>
                  <Button
                    variant={ButtonVariant.Secondary}
                    size={ButtonSize.Small}
                    aria-label={`Revoke ${labels.role[i.role]} invitation ${i.secretPrefix}`}
                    onClick={() => setRevoking(i)}
                  >
                    Revoke
                  </Button>
                </li>
              ))}
            </ul>
          )
        }
      </QueryBoundary>
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={`Revoke invitation ${revoking ? `${revoking.secretPrefix}…` : ""}?`}
        description="The invitation link stops working at once. It cannot be restored; create a new link instead."
        confirmLabel="Revoke invitation"
        onConfirm={async () => {
          await revoke.mutateAsync({
            organizationId,
            invitationId: revoking!.invitationId,
          });
          toast.success("The invitation is revoked.");
        }}
      />
      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title="Invite someone"
        description="You get a one-time link that works for 7 days. Send it to the person you invite."
        submitLabel="Create link"
        onSubmit={async (opening) => {
          const res = await create.mutateAsync({
            organizationId,
            role: Number(inviteRole) as Role,
          });
          // The list refreshes either way (`onSuccess`). A request of a
          // dismissed opening neither closes the form of a later opening nor
          // shows its link; the invitation stays listed and can be revoked.
          if (!opening.isCurrent()) return;
          setCreating(false);
          setLink(invitationLink(window.location.origin, res.invitationSecret));
        }}
      >
        <Select
          label="Role"
          value={inviteRole}
          onValueChange={(v) => v && setInviteRole(v)}
        >
          <SelectOption value={String(Role.MEMBER)}>Member</SelectOption>
          <SelectOption value={String(Role.ADMIN)}>Admin</SelectOption>
        </Select>
      </FormDialog>
      <SecretDialog
        title="Your invitation link"
        label="Invitation link"
        secret={link}
        onClose={() => setLink(undefined)}
      />
    </section>
  );
}
