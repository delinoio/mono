// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useMutation,
  useQuery,
} from "@connectrpc/connect-query";
import {
  AgentVersionPolicyMode,
  Role,
} from "@delino/ownsand-sdk/gen/common_pb";
import {
  DeviceService,
  MemberService,
  OrganizationService,
  type AgentVersionPolicy,
} from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Card, CardActions, CardDescription, CardTitle } from "delino-ui/card";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/errors.ts";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import { useAuth } from "../auth/auth.tsx";
import {
  ErrorState,
  LoadingState,
  PageHeader,
  formText,
} from "../components/common.tsx";
import { ConfirmDialog } from "../components/confirm-dialog.tsx";
import { Permission, can } from "../roles.ts";
import { useRouter } from "../router.tsx";
import type { OrganizationPageProps } from "./organization-layout.tsx";

/** Policy modes that a caller can select, by their stable identifiers. */
export enum PolicyMode {
  FollowDefault = "follow-default",
  Pin = "pin",
  Hold = "hold",
}

const MODE_TO_PROTO: Readonly<Record<PolicyMode, AgentVersionPolicyMode>> = {
  [PolicyMode.FollowDefault]: AgentVersionPolicyMode.FOLLOW_DEFAULT,
  [PolicyMode.Pin]: AgentVersionPolicyMode.PIN,
  [PolicyMode.Hold]: AgentVersionPolicyMode.HOLD,
};

const MODE_LABELS: Readonly<Record<PolicyMode, string>> = {
  [PolicyMode.FollowDefault]: "Follow the default version",
  [PolicyMode.Pin]: "Pin a version",
  [PolicyMode.Hold]: "Hold updates",
};

function fromProtoMode(mode: AgentVersionPolicyMode): PolicyMode {
  switch (mode) {
    case AgentVersionPolicyMode.PIN:
      return PolicyMode.Pin;
    case AgentVersionPolicyMode.HOLD:
      return PolicyMode.Hold;
    default:
      return PolicyMode.FollowDefault;
  }
}

function PolicyCard({
  organizationId,
  policy,
  editable,
}: {
  readonly organizationId: string;
  readonly policy: AgentVersionPolicy;
  readonly editable: boolean;
}) {
  const queryClient = useQueryClient();
  const update = useMutation(DeviceService.method.updateAgentVersionPolicy);
  const [mode, setMode] = useState(fromProtoMode(policy.mode));
  const [error, setError] = useState<string | undefined>(undefined);
  const [saved, setSaved] = useState(false);
  const defaults = Object.entries(policy.defaultVersions);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const pinnedVersion =
      mode === PolicyMode.Pin ? formText(event.currentTarget, "pinned") : "";
    if (mode === PolicyMode.Pin && pinnedVersion === "") {
      setError("Enter the version to pin.");
      return;
    }
    setError(undefined);
    setSaved(false);
    try {
      await update.mutateAsync({
        organizationId,
        mode: MODE_TO_PROTO[mode],
        pinnedVersion,
      });
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: DeviceService.method.getAgentVersionPolicy,
          cardinality: undefined,
        }),
      });
      setSaved(true);
    } catch (caught) {
      setError(errorMessage(caught));
    }
  };

  return (
    <Card>
      <CardTitle>Agent version policy</CardTitle>
      <CardDescription>
        Which device agent version the devices of this organization install.
      </CardDescription>
      <dl className="details">
        <div className="details-row">
          <dt>Current policy</dt>
          <dd>{MODE_LABELS[fromProtoMode(policy.mode)]}</dd>
        </div>
        {policy.mode === AgentVersionPolicyMode.PIN ? (
          <div className="details-row">
            <dt>Pinned version</dt>
            <dd>{policy.pinnedVersion}</dd>
          </div>
        ) : null}
        <div className="details-row">
          <dt>Default versions</dt>
          <dd>
            {defaults.length === 0
              ? "—"
              : defaults
                  .map(([platform, version]) => `${platform}: ${version}`)
                  .join(", ")}
          </dd>
        </div>
      </dl>
      {editable ? (
        <form
          onSubmit={(event) => void submit(event)}
          noValidate
          className="policy-form"
        >
          <Select
            label="Policy"
            value={mode}
            onValueChange={(value) => {
              const next = Object.values(PolicyMode).find(
                (candidate) => candidate === value,
              );
              if (next) {
                setMode(next);
                setSaved(false);
              }
            }}
          >
            {Object.values(PolicyMode).map((candidate) => (
              <SelectOption key={candidate} value={candidate}>
                {MODE_LABELS[candidate]}
              </SelectOption>
            ))}
          </Select>
          {mode === PolicyMode.Pin ? (
            <TextField
              label="Version to pin"
              name="pinned"
              defaultValue={policy.pinnedVersion}
              autoComplete="off"
            />
          ) : null}
          {error ? (
            <p className="form-error" role="alert">
              {error}
            </p>
          ) : null}
          {saved ? (
            <p className="muted" role="status">
              Policy saved.
            </p>
          ) : null}
          <CardActions>
            <Button type="submit" disabled={update.isPending}>
              {update.isPending ? "Saving…" : "Save policy"}
            </Button>
          </CardActions>
        </form>
      ) : null}
    </Card>
  );
}

function TransferOwnershipCard({
  organizationId,
  organizationName,
}: {
  readonly organizationId: string;
  readonly organizationName: string;
}) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const transfer = useMutation(OrganizationService.method.transferOwnership);
  const members = useInfiniteQuery(
    MemberService.method.listMembers,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const candidates = (
    members.data?.pages.flatMap((page) => page.members) ?? []
  ).filter(
    (member) => member.userId !== auth.userId && member.role !== Role.OWNER,
  );
  const [newOwner, setNewOwner] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const newOwnerMember = candidates.find((m) => m.userId === newOwner);
  const newOwnerName = newOwnerMember
    ? newOwnerMember.displayName || newOwnerMember.userId
    : "";

  return (
    <Card className="danger-zone">
      <CardTitle>Transfer ownership</CardTitle>
      <CardDescription>
        Make another member the owner. You become an admin.
      </CardDescription>
      {members.isError ? <ErrorState error={members.error} /> : null}
      {candidates.length === 0 ? (
        <p className="muted">
          {members.isPending
            ? "Loading members…"
            : "Invite another member first."}
        </p>
      ) : (
        <Select
          label="New owner"
          placeholder="Choose a member"
          value={newOwner}
          onValueChange={(value) => setNewOwner(value)}
        >
          {candidates.map((member) => (
            <SelectOption key={member.userId} value={member.userId}>
              {member.displayName || member.userId}
            </SelectOption>
          ))}
        </Select>
      )}
      <CardActions>
        <Button
          variant={ButtonVariant.Danger}
          disabled={newOwnerMember === undefined}
          onClick={() => setConfirming(true)}
        >
          Transfer ownership
        </Button>
      </CardActions>
      <ConfirmDialog
        open={confirming}
        title={`Make ${newOwnerName} the owner of ${organizationName}?`}
        description="You become an admin and can no longer delete the organization or transfer ownership."
        confirmLabel="Transfer ownership"
        onClose={() => setConfirming(false)}
        onConfirm={async () => {
          if (newOwnerMember === undefined) {
            return;
          }
          await transfer.mutateAsync({
            organizationId,
            newOwnerUserId: newOwnerMember.userId,
          });
          await queryClient.invalidateQueries();
        }}
      />
    </Card>
  );
}

function DeleteOrganizationCard({
  organizationId,
  organizationName,
}: {
  readonly organizationId: string;
  readonly organizationName: string;
}) {
  const queryClient = useQueryClient();
  const { navigate } = useRouter();
  const remove = useMutation(OrganizationService.method.deleteOrganization);
  const [confirming, setConfirming] = useState(false);
  return (
    <Card className="danger-zone">
      <CardTitle>Delete organization</CardTitle>
      <CardDescription>
        All sandboxes end, every device is removed and every API key stops
        working.
      </CardDescription>
      <CardActions>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirming(true)}
        >
          Delete organization
        </Button>
      </CardActions>
      <ConfirmDialog
        open={confirming}
        title={`Delete ${organizationName}?`}
        description="All sandboxes end, every device is removed and every API key stops working. This cannot be undone."
        confirmLabel="Delete organization"
        onClose={() => setConfirming(false)}
        onConfirm={async () => {
          await remove.mutateAsync({ organizationId });
          queryClient.removeQueries({
            queryKey: createConnectQueryKey({
              schema: OrganizationService.method.getOrganization,
              cardinality: undefined,
            }),
          });
          await queryClient.invalidateQueries();
          navigate("/");
        }}
      />
    </Card>
  );
}

export function SettingsPage({
  organizationId,
  organizationName,
  role,
}: OrganizationPageProps) {
  const query = useQuery(DeviceService.method.getAgentVersionPolicy, {
    organizationId,
  });
  const policy = query.data?.policy;
  return (
    <main className="page">
      <PageHeader title="Settings" />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError || policy === undefined ? (
        <ErrorState error={query.error} />
      ) : (
        <PolicyCard
          organizationId={organizationId}
          policy={policy}
          editable={can(role, Permission.ChangeAgentVersionPolicy)}
        />
      )}
      {can(role, Permission.TransferOwnership) ? (
        <TransferOwnershipCard
          organizationId={organizationId}
          organizationName={organizationName}
        />
      ) : null}
      {can(role, Permission.DeleteOrganization) ? (
        <DeleteOrganizationCard
          organizationId={organizationId}
          organizationName={organizationName}
        />
      ) : null}
    </main>
  );
}
