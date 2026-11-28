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
import { InvitationState, Role } from "@delino/sandrun-protos/v1/common_pb";
import {
  type Invitation,
  ListInvitationsResponseSchema,
  type Member,
  MemberService,
} from "@delino/sandrun-protos/v1/member_pb";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import { useState } from "react";
import { invalidateServices, MAX_PAGE_SIZE } from "../api.ts";
import { useAuth } from "../auth.tsx";
import { MemberListStatus } from "../components/member-list-status.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  useActionSignInNotice,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { invitationLink } from "../invitation.ts";
import { displayName, useMembers } from "../members.ts";
import { can, canManageMember, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

/** Route `members`: members, roles, leaving and invitations. */
export function MembersPage() {
  const { lang, t } = useI18n();
  const { organizationId, role } = useOrg();
  const { userId } = useAuth();
  const { navigate } = useRouter();
  const { signInError, reportActionError } = useActionSignInNotice();
  const queryClient = useQueryClient();
  const refresh = () =>
    invalidateServices(queryClient, MemberService, OrganizationService);
  const roster = useMembers(organizationId);
  const updateRole = useMutation(MemberService.method.updateMemberRole, {
    onSuccess: refresh,
  });
  const removeMember = useMutation(MemberService.method.removeMember);
  const [removing, setRemoving] = useState<Member>();
  const [leaving, setLeaving] = useState(false);

  return (
    <>
      <PageHeader
        title={t("members.title")}
        description={t("members.description")}
        action={
          can(role, Permission.LeaveOrganization) ? (
            <Button
              variant={ButtonVariant.Secondary}
              onClick={() => setLeaving(true)}
            >
              {t("members.leave")}
            </Button>
          ) : undefined
        }
      />
      {signInError ? <ErrorNotice error={signInError} /> : null}
      <MemberListStatus roster={roster}>
        <ul className="list" aria-label={t("members.title")}>
          {roster.members.map((m) => {
            const manageable = canManageMember(role, userId, m);
            return (
              <li key={m.userId} className="list-row">
                <div className="list-main">
                  <span className="list-title">
                    {displayName(m)}
                    {m.userId === userId ? (
                      <span className="muted"> {t("common.you")}</span>
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
                          reportActionError(error);
                        }
                      }}
                    >
                      <SelectOption value={String(Role.ADMIN)}>
                        {t(labels.role[Role.ADMIN])}
                      </SelectOption>
                      <SelectOption value={String(Role.MEMBER)}>
                        {t(labels.role[Role.MEMBER])}
                      </SelectOption>
                    </Select>
                    <Button
                      variant={ButtonVariant.Danger}
                      size={ButtonSize.Small}
                      onClick={() => setRemoving(m)}
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
      </MemberListStatus>
      {can(role, Permission.ManageInvitations) ? (
        <Invitations reportActionError={reportActionError} />
      ) : null}
      <ConfirmDialog
        open={removing !== undefined}
        onOpenChange={(open) => !open && setRemoving(undefined)}
        title={t("members.removeDialog.title", {
          name: removing ? displayName(removing) : "",
        })}
        description={t("members.removeDialog.description")}
        confirmLabel={t("members.removeDialog.confirm")}
        onConfirm={async () => {
          await removeMember.mutateAsync({
            organizationId,
            userId: removing!.userId,
          });
          await refresh();
        }}
      />
      <ConfirmDialog
        open={leaving}
        onOpenChange={setLeaving}
        title={t("members.leaveDialog.title")}
        description={t("members.leaveDialog.description")}
        confirmLabel={t("members.leave")}
        onConfirm={async (opening) => {
          await removeMember.mutateAsync({
            organizationId,
            userId: userId ?? "",
          });
          // Refreshing the old route can unmount the confirmation.
          if (opening.isCurrent()) navigate("/");
          await refresh();
        }}
      />
    </>
  );
}

function Invitations({
  reportActionError,
}: {
  reportActionError: (error: unknown) => void;
}) {
  const { lang, t, rich } = useI18n();
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
    onSuccess: () => {
      void refresh();
    },
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
        <h2 id="invitations-title">{t("members.invitations.title")}</h2>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setCreating(true)}
        >
          {t("members.invitations.invite")}
        </Button>
      </div>
      <QueryBoundary query={list} loading={t("members.invitations.loading")}>
        {(data) => {
          const pending = data.invitations.filter(
            (i) => i.state === InvitationState.PENDING,
          );
          return pending.length === 0 ? (
            <Empty title={t("members.invitations.empty")} />
          ) : (
            <ul
              className="list"
              aria-label={t("members.invitations.pendingLabel")}
            >
              {pending.map((i) => (
                <li key={i.invitationId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {rich("members.invitations.item", {
                        role: t(labels.role[i.role]),
                        prefix: <code>{i.secretPrefix}…</code>,
                      })}
                    </span>
                    <span className="muted">
                      {t("members.invitations.expires", {
                        time: formatTime(i.expireTime, lang),
                      })}
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
                        toast.success(t("members.invitations.revoked"));
                      } catch (error) {
                        reportActionError(error);
                      }
                    }}
                  >
                    {t("members.invitations.revoke")}
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
        title={t("members.invitations.invite")}
        description={t("members.invitations.createDialog.description")}
        submitLabel={t("members.invitations.createDialog.submit")}
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
          label={t("members.invitations.createDialog.role")}
          value={inviteRole}
          onValueChange={(v) => v && setInviteRole(v)}
        >
          <SelectOption value={String(Role.MEMBER)}>
            {t(labels.role[Role.MEMBER])}
          </SelectOption>
          <SelectOption value={String(Role.ADMIN)}>
            {t(labels.role[Role.ADMIN])}
          </SelectOption>
        </Select>
      </FormDialog>
      <SecretDialog
        title={t("members.invitations.secretDialog.title")}
        label={t("members.invitations.secretDialog.label")}
        secret={link}
        onClose={() => setLink(undefined)}
      />
    </section>
  );
}
