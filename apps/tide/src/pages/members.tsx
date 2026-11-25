// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import {
  callUnaryMethod,
  createQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import {
  useQuery as useReactQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { InvitationState, Role } from "@delino/tide-protos/v1/common_pb";
import {
  type Invitation,
  ListInvitationsResponseSchema,
  type Member,
  MemberService,
} from "@delino/tide-protos/v1/member_pb";
import { OrganizationService } from "@delino/tide-protos/v1/organization_pb";
import { useState } from "react";
import { invalidateServices, MAX_PAGE_SIZE } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { MemberListStatus } from "../components/member-list-status.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  FormDialog,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  toastError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { invitationLink } from "../invitation.ts";
import { displayName, useMembers } from "../members.ts";
import { can, canManageMember, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

/** Route `members`: members, roles, leaving and invitations. */
export function MembersPage() {
  const { organizationId, role } = useOrg();
  const { userId } = useAuth();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const refresh = () =>
    invalidateServices(queryClient, MemberService, OrganizationService);
  const roster = useMembers(organizationId);
  const updateRole = useMutation(MemberService.method.updateMemberRole, {
    onSuccess: refresh,
  });
  const removeMember = useMutation(MemberService.method.removeMember, {
    onSuccess: refresh,
  });
  const [removing, setRemoving] = useState<Member>();
  const [leaving, setLeaving] = useState(false);

  return (
    <>
      <PageHeader
        title="Members"
        description="People who can use this tide organization. tide memberships are separate from OwnSand memberships. Profiles come from your Delino account."
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
      <MemberListStatus roster={roster}>
        <ul className="list" aria-label="Members">
          {roster.members.map((m) => {
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
                  <span className="muted">Joined {formatTime(m.joinTime)}</span>
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
      </MemberListStatus>
      {can(role, Permission.ManageInvitations) ? <Invitations /> : null}
      <ConfirmDialog
        open={removing !== undefined}
        onOpenChange={(open) => !open && setRemoving(undefined)}
        title={`Remove ${removing ? displayName(removing) : ""}?`}
        description="They lose access to this organization. tide revokes their personal API keys and ends their dev sessions."
        confirmLabel="Remove member"
        onConfirm={() =>
          removeMember.mutateAsync({ organizationId, userId: removing!.userId })
        }
      />
      <ConfirmDialog
        open={leaving}
        onOpenChange={setLeaving}
        title="Leave this organization?"
        description="You lose access to this organization. tide revokes your personal API keys and ends your dev sessions. An admin can invite you again."
        confirmLabel="Leave organization"
        onConfirm={async (opening) => {
          await removeMember.mutateAsync({
            organizationId,
            userId: userId ?? "",
          });
          if (opening.isCurrent()) navigate("/");
        }}
      />
    </>
  );
}

function Invitations() {
  const { organizationId } = useOrg();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const method = MemberService.method.listInvitations;
  const input = { organizationId, pageSize: MAX_PAGE_SIZE };
  const query = createQueryOptions(method, input, { transport });
  const list = useReactQuery({
    ...query,
    queryFn: async ({ signal }) => {
      const invitations: Invitation[] = [];
      let pageToken = "";
      do {
        const page = await callUnaryMethod(
          transport,
          method,
          { ...input, pageToken },
          { signal },
        );
        invitations.push(...page.invitations);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return create(ListInvitationsResponseSchema, { invitations });
    },
  });
  const refresh = async () => {
    // A write can finish while the first load is still paging.
    await queryClient.cancelQueries({ queryKey: query.queryKey, exact: true });
    await invalidateServices(queryClient, MemberService);
  };
  const createInvitation = useMutation(MemberService.method.createInvitation, {
    onSuccess: refresh,
  });
  const revoke = useMutation(MemberService.method.revokeInvitation, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [inviteRole, setInviteRole] = useState(String(Role.MEMBER));
  const [link, setLink] = useState<string>();

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
        {(data) => {
          const pending = data.invitations.filter(
            (i) => i.state === InvitationState.PENDING,
          );
          return pending.length === 0 ? (
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
                    onClick={async () => {
                      try {
                        await revoke.mutateAsync({
                          organizationId,
                          invitationId: i.invitationId,
                        });
                        toast.success("The invitation is revoked.");
                      } catch (error) {
                        toastError(error);
                      }
                    }}
                  >
                    Revoke
                  </Button>
                </li>
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title="Invite someone"
        description="You get a one-time link that works for 7 days. Send it to the person you invite."
        submitLabel="Create link"
        onSubmit={async (opening) => {
          const res = await createInvitation.mutateAsync({
            organizationId,
            role: Number(inviteRole) as Role,
          });
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
