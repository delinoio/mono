// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useMutation,
} from "@connectrpc/connect-query";
import { InvitationState } from "@delino/ownsand-sdk/gen/common_pb";
import {
  MemberService,
  OrganizationService,
  type Invitation,
  type Member,
} from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { Select, SelectOption } from "delino-ui/select";
import { useState } from "react";
import { errorMessage } from "../api/errors.ts";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import { useAuth } from "../auth/auth.tsx";
import {
  EmptyState,
  ErrorState,
  LoadMore,
  LoadingState,
  PageHeader,
} from "../components/common.tsx";
import { ConfirmDialog } from "../components/confirm-dialog.tsx";
import { SecretDialog } from "../components/secret-dialog.tsx";
import { formatEnum, formatTime } from "../format.ts";
import { buildInvitationLink } from "../invitation.ts";
import {
  ConsoleRole,
  Permission,
  can,
  canManageMember,
  fromProtoRole,
  grantableRoles,
  roleLabel,
  toProtoRole,
} from "../roles.ts";
import { useRouter } from "../router.tsx";
import type { OrganizationPageProps } from "./organization-layout.tsx";

function useInvalidate() {
  const queryClient = useQueryClient();
  return () =>
    queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({
        schema: MemberService,
        cardinality: undefined,
      }),
    });
}

function RoleSelectDialog({
  title,
  description,
  roles,
  initialRole,
  submitLabel,
  open,
  onClose,
  onSubmit,
}: {
  readonly title: string;
  readonly description: string;
  readonly roles: ConsoleRole[];
  readonly initialRole: ConsoleRole;
  readonly submitLabel: string;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onSubmit: (role: ConsoleRole) => Promise<void>;
}) {
  const [role, setRole] = useState<ConsoleRole>(initialRole);
  const [error, setError] = useState<string | undefined>(undefined);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          setError(undefined);
          onClose();
        }
      }}
    >
      <DialogContent>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>{description}</DialogDescription>
        <Select
          label="Role"
          value={role}
          onValueChange={(value) => {
            const next = roles.find((candidate) => candidate === value);
            if (next) {
              setRole(next);
            }
          }}
        >
          {roles.map((candidate) => (
            <SelectOption key={candidate} value={candidate}>
              {roleLabel(candidate)}
            </SelectOption>
          ))}
        </Select>
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        <DialogActions>
          <DialogClose>Cancel</DialogClose>
          <DialogClose
            variant={ButtonVariant.Primary}
            action={async () => {
              setError(undefined);
              try {
                await onSubmit(role);
              } catch (caught) {
                setError(errorMessage(caught));
                throw caught;
              }
            }}
          >
            {submitLabel}
          </DialogClose>
        </DialogActions>
      </DialogContent>
    </Dialog>
  );
}

/** Pending dialog of the members page. */
type MemberDialog =
  | { readonly kind: "role"; readonly member: Member }
  | { readonly kind: "remove"; readonly member: Member }
  | { readonly kind: "leave" }
  | { readonly kind: "invite" }
  | { readonly kind: "revoke-invitation"; readonly invitation: Invitation };

function memberName(member: Member): string {
  return member.displayName || member.userId;
}

export function MembersPage({
  organizationId,
  organizationName,
  role,
}: OrganizationPageProps) {
  const auth = useAuth();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const invalidate = useInvalidate();
  const [dialog, setDialog] = useState<MemberDialog | undefined>(undefined);
  const [newInvitationLink, setNewInvitationLink] = useState<
    string | undefined
  >(undefined);

  const members = useInfiniteQuery(
    MemberService.method.listMembers,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const canInvite = can(role, Permission.ManageInvitations);
  const invitations = useInfiniteQuery(
    MemberService.method.listInvitations,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    { ...pageOptions, enabled: canInvite },
  );
  const updateRole = useMutation(MemberService.method.updateMemberRole);
  const removeMember = useMutation(MemberService.method.removeMember);
  const createInvitation = useMutation(MemberService.method.createInvitation);
  const revokeInvitation = useMutation(MemberService.method.revokeInvitation);

  const memberList = members.data?.pages.flatMap((page) => page.members) ?? [];
  const pendingInvitations = (
    invitations.data?.pages.flatMap((page) => page.invitations) ?? []
  ).filter((invitation) => invitation.state === InvitationState.PENDING);
  const close = () => setDialog(undefined);

  return (
    <main className="page">
      <PageHeader
        title="Members"
        description="People in this organization and their roles."
        actions={
          <>
            {canInvite ? (
              <Button onClick={() => setDialog({ kind: "invite" })}>
                Invite member
              </Button>
            ) : null}
            {can(role, Permission.LeaveOrganization) ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() => setDialog({ kind: "leave" })}
              >
                Leave organization
              </Button>
            ) : null}
          </>
        }
      />
      {members.isPending ? (
        <LoadingState />
      ) : members.isError ? (
        <ErrorState error={members.error} />
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Role</th>
                <th scope="col">Joined</th>
                <th scope="col">
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {memberList.map((member) => {
                const memberRole = fromProtoRole(member.role);
                const manageable = canManageMember(role, auth.userId, {
                  userId: member.userId,
                  role: memberRole,
                });
                return (
                  <tr key={member.userId}>
                    <td>
                      {memberName(member)}
                      {member.userId === auth.userId ? (
                        <span className="muted"> (you)</span>
                      ) : null}
                    </td>
                    <td>{roleLabel(memberRole)}</td>
                    <td>{formatTime(member.joinTime)}</td>
                    <td className="row-actions">
                      {manageable ? (
                        <>
                          <Button
                            size={ButtonSize.Small}
                            variant={ButtonVariant.Secondary}
                            aria-label={`Change role of ${memberName(member)}`}
                            onClick={() => setDialog({ kind: "role", member })}
                          >
                            Change role
                          </Button>
                          <Button
                            size={ButtonSize.Small}
                            variant={ButtonVariant.Danger}
                            aria-label={`Remove ${memberName(member)}`}
                            onClick={() =>
                              setDialog({ kind: "remove", member })
                            }
                          >
                            Remove
                          </Button>
                        </>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <LoadMore
        hasMore={members.hasNextPage}
        loading={members.isFetchingNextPage}
        onLoadMore={() => void members.fetchNextPage()}
      />

      {canInvite ? (
        <section aria-labelledby="pending-invitations">
          <h2 id="pending-invitations">Pending invitations</h2>
          {invitations.isPending ? (
            <LoadingState />
          ) : invitations.isError ? (
            <ErrorState error={invitations.error} />
          ) : pendingInvitations.length === 0 ? (
            <EmptyState>No pending invitations.</EmptyState>
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Starts with</th>
                    <th scope="col">Role</th>
                    <th scope="col">State</th>
                    <th scope="col">Expires</th>
                    <th scope="col">
                      <span className="visually-hidden">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {pendingInvitations.map((invitation) => (
                    <tr key={invitation.invitationId}>
                      <td className="mono">{invitation.secretPrefix}…</td>
                      <td>{roleLabel(fromProtoRole(invitation.role))}</td>
                      <td>{formatEnum(InvitationState, invitation.state)}</td>
                      <td>{formatTime(invitation.expireTime)}</td>
                      <td className="row-actions">
                        <Button
                          size={ButtonSize.Small}
                          variant={ButtonVariant.Danger}
                          onClick={() =>
                            setDialog({ kind: "revoke-invitation", invitation })
                          }
                        >
                          Revoke
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ) : null}

      <RoleSelectDialog
        key={dialog?.kind === "invite" ? "invite-open" : "invite"}
        open={dialog?.kind === "invite"}
        title="Invite a member"
        description="Create an invitation link and share it with the person you want to invite. The link works once and expires in 7 days."
        roles={grantableRoles(role)}
        initialRole={ConsoleRole.Member}
        submitLabel="Create invitation link"
        onClose={close}
        onSubmit={async (invitedRole) => {
          const response = await createInvitation.mutateAsync({
            organizationId,
            role: toProtoRole(invitedRole),
          });
          await invalidate();
          setNewInvitationLink(
            buildInvitationLink(
              window.location.origin,
              response.invitationSecret,
            ),
          );
        }}
      />

      {dialog?.kind === "role" ? (
        <RoleSelectDialog
          key={dialog.member.userId}
          open
          title={`Change the role of ${memberName(dialog.member)}`}
          description="Admins manage members, devices, enrollment tokens and API keys. Members use devices and sandboxes."
          roles={grantableRoles(role)}
          initialRole={fromProtoRole(dialog.member.role) ?? ConsoleRole.Member}
          submitLabel="Save role"
          onClose={close}
          onSubmit={async (newRole) => {
            await updateRole.mutateAsync({
              organizationId,
              userId: dialog.member.userId,
              role: toProtoRole(newRole),
            });
            await invalidate();
          }}
        />
      ) : null}

      <ConfirmDialog
        open={dialog?.kind === "remove"}
        title={
          dialog?.kind === "remove"
            ? `Remove ${memberName(dialog.member)}?`
            : "Remove member?"
        }
        description="They lose access to this organization at once. You can invite them again later."
        confirmLabel="Remove member"
        onClose={close}
        onConfirm={async () => {
          if (dialog?.kind !== "remove") {
            return;
          }
          await removeMember.mutateAsync({
            organizationId,
            userId: dialog.member.userId,
          });
          await invalidate();
        }}
      />

      <ConfirmDialog
        open={dialog?.kind === "leave"}
        title={`Leave ${organizationName}?`}
        description="You lose access to this organization at once. An admin must invite you again to come back."
        confirmLabel="Leave organization"
        onClose={close}
        onConfirm={async () => {
          if (auth.userId === undefined) {
            throw new Error("missing user ID");
          }
          await removeMember.mutateAsync({
            organizationId,
            userId: auth.userId,
          });
          await queryClient.invalidateQueries({
            queryKey: createConnectQueryKey({
              schema: OrganizationService,
              cardinality: undefined,
            }),
          });
          navigate("/");
        }}
      />

      <ConfirmDialog
        open={dialog?.kind === "revoke-invitation"}
        title="Revoke this invitation?"
        description="The invitation link stops working. Nobody can join with it."
        confirmLabel="Revoke invitation"
        onClose={close}
        onConfirm={async () => {
          if (dialog?.kind !== "revoke-invitation") {
            return;
          }
          await revokeInvitation.mutateAsync({
            organizationId,
            invitationId: dialog.invitation.invitationId,
          });
          await invalidate();
        }}
      />

      <SecretDialog
        secret={newInvitationLink}
        title="Invitation link"
        description="Share this link with the person you invite. It works once and expires in 7 days."
        secretLabel="Invitation link"
        onClose={() => setNewInvitationLink(undefined)}
      />
    </main>
  );
}
