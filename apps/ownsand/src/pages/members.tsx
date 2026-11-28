// SPDX-License-Identifier: Apache-2.0
import {
  createInfiniteQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
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
import { useMemo, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  FormDialog,
  ListNoun,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  toastError,
  isTerminalError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { invitationLink } from "../invitation.ts";
import { membersBoundary, useMembers } from "../members.ts";
import {
  FOLLOW_ALL_PAGES_OPTIONS,
  boundaryOf,
  cancelUncachedList,
  uniqueRows,
  useFollowAllPages,
} from "../pagination.ts";
import { repeatedPrefixes } from "../revocation.ts";
import { can, canManageMember, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";
import { enumText } from "../text.ts";

function displayName(m: Member): string {
  return m.displayName !== "" ? m.displayName : m.userId;
}

/** Route `members`: members, roles, leaving and invitations. */
export function MembersPage() {
  const { organizationId, role } = useOrg();
  const { userId } = useAuth();
  const i18n = useI18n();
  const { t, lang } = i18n;
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
  const [removingUserId, setRemovingUserId] = useState<string>();
  const [leaving, setLeaving] = useState(false);
  const canLeave = !!userId && can(role, Permission.LeaveOrganization);

  if (!canLeave && leaving) {
    setLeaving(false);
  }

  const removing = members.members.find((m) => m.userId === removingUserId);
  const canRemove =
    removing !== undefined && canManageMember(role, userId, removing);
  const terminalMembersError = isTerminalError(members.error);
  const hasCompleteRoster =
    members.isLoaded &&
    !members.isFetching &&
    !members.isIncomplete &&
    members.error === null;
  if (
    removingUserId !== undefined &&
    (terminalMembersError ||
      (removing !== undefined ? !canRemove : hasCompleteRoster))
  ) {
    setRemovingUserId(undefined);
  }

  return (
    <>
      <PageHeader
        title={t("members.title")}
        description={t("members.description")}
        action={
          canLeave ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setLeaving(true)}
            >
              {t("members.leave")}
            </Button>
          ) : undefined
        }
      />
      <QueryBoundary
        query={membersBoundary(members)}
        loading={t("members.loading")}
      >
        {(loaded) => (
          <>
            <ul className="list" aria-label={t("members.list")}>
              {loaded.map((m) => {
                const manageable = canManageMember(role, userId, m);
                return (
                  <li key={m.userId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        {displayName(m)}
                        {m.userId === userId ? (
                          <span className="muted">
                            {` ${t("members.you")}`}
                          </span>
                        ) : null}
                      </span>
                      <span className="muted">
                        {t("members.joined", {
                          time: formatTime(m.joinTime, lang),
                        })}
                      </span>
                    </div>
                    {manageable ? (
                      <div className="list-actions">
                        <Select
                          label={t("members.roleOf", { name: displayName(m) })}
                          value={String(m.role)}
                          onValueChange={async (v) => {
                            if (v === null || Number(v) === m.role) return;
                            try {
                              await updateRole.mutateAsync({
                                organizationId,
                                userId: m.userId,
                                role: Number(v) as Role,
                              });
                              toast.success(t("members.roleChanged"));
                            } catch (error) {
                              toastError(error, i18n);
                            }
                          }}
                        >
                          <SelectOption value={String(Role.ADMIN)}>
                            {enumText(i18n, labels.role, Role.ADMIN)}
                          </SelectOption>
                          <SelectOption value={String(Role.MEMBER)}>
                            {enumText(i18n, labels.role, Role.MEMBER)}
                          </SelectOption>
                        </Select>
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          aria-label={t("members.removeNamed", {
                            name: displayName(m),
                          })}
                          onClick={() => setRemovingUserId(m.userId)}
                        >
                          {t("members.remove")}
                        </Button>
                      </div>
                    ) : (
                      <span className="role">
                        {enumText(i18n, labels.role, m.role)}
                      </span>
                    )}
                  </li>
                );
              })}
            </ul>
            <MorePagesNotice noun={ListNoun.Members} pages={members} />
          </>
        )}
      </QueryBoundary>
      {can(role, Permission.ManageInvitations) ? <Invitations /> : null}
      <ConfirmDialog
        open={canRemove && !terminalMembersError}
        onOpenChange={(open) => !open && setRemovingUserId(undefined)}
        title={t("members.removeDialog.title", {
          name: removing ? displayName(removing) : "",
        })}
        description={t("members.removeDialog.description")}
        confirmLabel={t("members.removeDialog.confirm")}
        onConfirm={async () => {
          if (removingUserId === undefined) return false;
          const current = members.getCurrentMember(removingUserId);
          if (
            current === undefined ||
            !canManageMember(role, userId, current)
          ) {
            return false;
          }
          await removeMember.mutateAsync({
            organizationId,
            userId: current.userId,
          });
        }}
      />
      <ConfirmDialog
        open={canLeave && leaving}
        onOpenChange={setLeaving}
        title={t("members.leaveDialog.title")}
        description={t("members.leaveDialog.description")}
        confirmLabel={t("members.leave")}
        onConfirm={async (opening) => {
          if (!canLeave) return;
          await leave.mutateAsync({
            organizationId,
            userId,
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
  const i18n = useI18n();
  const { t, lang } = i18n;
  const queryClient = useQueryClient();
  const transport = useTransport();
  const refresh = () => invalidateServices(queryClient, MemberService);
  const listOptions = (listOrganizationId: string) =>
    createInfiniteQueryOptions(
      MemberService.method.listInvitations,
      { organizationId: listOrganizationId, pageSize: 200, pageToken: "" },
      {
        transport,
        pageParamKey: FOLLOW_ALL_PAGES_OPTIONS.pageParamKey,
        getNextPageParam: (last) => last.nextPageToken || undefined,
      },
    );
  const list = useInfiniteQuery({
    ...listOptions(organizationId),
    ...FOLLOW_ALL_PAGES_OPTIONS,
  });
  const pages = useFollowAllPages(list);
  const pending = useMemo(
    () =>
      uniqueRows(
        list.data?.pages.map((page) => page.invitations) ?? [],
        (invitation) => invitation.invitationId,
      ).filter((invitation) => invitation.state === InvitationState.PENDING),
    [list.data],
  );
  const collisions = useMemo(
    () =>
      repeatedPrefixes(pending.map((invitation) => invitation.secretPrefix)),
    [pending],
  );
  const create = useMutation(MemberService.method.createInvitation, {
    onSuccess: (_, { organizationId: submittedOrganizationId }) => {
      if (submittedOrganizationId === undefined) {
        void refresh();
        return;
      }
      void cancelUncachedList(
        queryClient,
        listOptions(submittedOrganizationId).queryKey,
      ).then(refresh);
    },
  });
  const revoke = useMutation(MemberService.method.revokeInvitation, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [inviteRole, setInviteRole] = useState(String(Role.MEMBER));
  const [link, setLink] = useState<string>();
  const [revoking, setRevoking] = useState<Invitation>();
  const revokingId =
    revoking && collisions.has(revoking.secretPrefix)
      ? ` (${revoking.invitationId})`
      : "";
  if (isTerminalError(pages.error) && revoking !== undefined) {
    setRevoking(undefined);
  }

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
      <QueryBoundary
        query={boundaryOf(list, pages)}
        loading={t("invitations.loading")}
      >
        {() =>
          pending.length === 0 && !pages.isIncomplete ? (
            <Empty title={t("invitations.empty")} />
          ) : (
            <>
              {pending.length > 0 ? (
                <ul className="list" aria-label={t("invitations.list")}>
                  {pending.map((i) => {
                    const showId = collisions.has(i.secretPrefix);
                    const role = enumText(i18n, labels.role, i.role);
                    return (
                      <li key={i.invitationId} className="list-row">
                        <div className="list-main">
                          <span className="list-title">
                            {i18n.rich("invitations.rowTitle", {
                              role,
                              prefix: <code>{i.secretPrefix}…</code>,
                            })}
                          </span>
                          <span className="muted">
                            {t("invitations.expires", {
                              time: formatTime(i.expireTime, lang),
                            })}
                          </span>
                          {showId ? (
                            <span className="muted list-record-id">
                              {i18n.rich("invitations.recordId", {
                                id: <code>{i.invitationId}</code>,
                              })}
                            </span>
                          ) : null}
                        </div>
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          aria-label={
                            showId
                              ? t("invitations.revokeNamedWithId", {
                                  role,
                                  prefix: i.secretPrefix,
                                  id: i.invitationId,
                                })
                              : t("invitations.revokeNamed", {
                                  role,
                                  prefix: i.secretPrefix,
                                })
                          }
                          onClick={() => setRevoking(i)}
                        >
                          {t("invitations.revoke")}
                        </Button>
                      </li>
                    );
                  })}
                </ul>
              ) : null}
              <MorePagesNotice
                noun={ListNoun.PendingInvitations}
                pages={pages}
              />
            </>
          )
        }
      </QueryBoundary>
      <ConfirmDialog
        open={revoking !== undefined}
        onOpenChange={(open) => !open && setRevoking(undefined)}
        title={t("invitations.revokeDialog.title", {
          invitation: revoking ? `${revoking.secretPrefix}…${revokingId}` : "",
        })}
        description={t("invitations.revokeDialog.description")}
        confirmLabel={t("invitations.revokeDialog.confirm")}
        onConfirm={async () => {
          if (!revoking) return;
          await revoke.mutateAsync({
            organizationId,
            invitationId: revoking.invitationId,
          });
          toast.success(t("invitations.revoked"));
        }}
      />
      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title={t("invitations.invite")}
        description={t("invitations.createDialog.description")}
        submitLabel={t("invitations.createDialog.submit")}
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
          label={t("invitations.createDialog.role")}
          value={inviteRole}
          onValueChange={(v) => v && setInviteRole(v)}
        >
          <SelectOption value={String(Role.MEMBER)}>
            {enumText(i18n, labels.role, Role.MEMBER)}
          </SelectOption>
          <SelectOption value={String(Role.ADMIN)}>
            {enumText(i18n, labels.role, Role.ADMIN)}
          </SelectOption>
        </Select>
      </FormDialog>
      <SecretDialog
        title={t("invitations.secret.title")}
        label={t("invitations.secret.label")}
        secret={link}
        onClose={() => setLink(undefined)}
      />
    </section>
  );
}
