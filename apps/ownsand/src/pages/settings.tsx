// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
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
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { type FormEvent, useEffect, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  PageHeader,
  QueryBoundary,
  toastError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";
import { can, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";
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
  return (
    <>
      <PageHeader title="Settings" />
      <dl className="details">
        <Detail term="Organization">{name}</Detail>
        <Detail term="Organization ID">
          <code>{organizationId}</code>
        </Detail>
        <Detail term="Your role">{labels.role[role]}</Detail>
      </dl>
      <AgentPolicy />
      {can(role, Permission.ViewBilling) ? <BillingSection /> : null}
      {can(role, Permission.TransferOwnership) ? <OwnerActions /> : null}
    </>
  );
}

function AgentPolicy() {
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const policy = useQuery(DeviceService.method.getAgentVersionPolicy, {
    organizationId,
  });
  const update = useMutation(DeviceService.method.updateAgentVersionPolicy, {
    onSuccess: () => invalidateServices(queryClient, DeviceService),
  });
  const [mode, setMode] = useState(
    String(AgentVersionPolicyMode.FOLLOW_DEFAULT),
  );
  const [version, setVersion] = useState("");
  const [versionError, setVersionError] = useState<string>();
  const editable = can(role, Permission.ChangeAgentVersionPolicy);
  const current = policy.data?.policy;

  useEffect(() => {
    if (current === undefined) return;
    setMode(String(current.mode || AgentVersionPolicyMode.FOLLOW_DEFAULT));
    setVersion(current.pinnedVersion);
  }, [current]);

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (Number(mode) === AgentVersionPolicyMode.PIN && version.trim() === "") {
      setVersionError("Enter the agent version to pin, for example 1.4.2.");
      return;
    }
    try {
      await update.mutateAsync({
        organizationId,
        mode: Number(mode) as AgentVersionPolicyMode,
        pinnedVersion:
          Number(mode) === AgentVersionPolicyMode.PIN ? version.trim() : "",
      });
      toast.success("The agent version policy is saved.");
    } catch (error) {
      toastError(error);
    }
  };

  return (
    <section className="section" aria-labelledby="policy-title">
      <h2 id="policy-title">Agent version policy</h2>
      <p className="muted">
        Decides which agent version the devices of this organization install.
      </p>
      <QueryBoundary query={policy} loading="Loading the agent version policy">
        {() => (
          <form className="form" onSubmit={save}>
            <SegmentedControl
              aria-label="Update mode"
              value={mode}
              onValueChange={(v: unknown) =>
                typeof v === "string" && setMode(v)
              }
              disabled={!editable}
            >
              {MODES.map((m) => (
                <SegmentedControlItem key={m} value={String(m)}>
                  {labels.policyMode[m]}
                </SegmentedControlItem>
              ))}
            </SegmentedControl>
            {Number(mode) === AgentVersionPolicyMode.PIN ? (
              <TextField
                label="Pinned version"
                value={version}
                onValueChange={(v) => {
                  setVersion(v);
                  setVersionError(undefined);
                }}
                error={versionError}
                disabled={!editable}
              />
            ) : null}
            {current?.updateTime ? (
              <p className="muted">
                Last changed {formatTime(current.updateTime)}
              </p>
            ) : null}
            {editable ? (
              <div className="actions">
                <Button type="submit">Save policy</Button>
              </div>
            ) : (
              <p className="muted">Only admins can change the policy.</p>
            )}
          </form>
        )}
      </QueryBoundary>
    </section>
  );
}

function OwnerActions() {
  const { organizationId, name } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const members = useQuery(MemberService.method.listMembers, {
    organizationId,
    pageSize: 200,
  });
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
  const candidates =
    members.data?.members.filter((m) => m.role !== Role.OWNER) ?? [];
  const targetName = (() => {
    const m = candidates.find((c) => c.userId === target);
    return m ? m.displayName || m.userId : "";
  })();

  return (
    <>
      <section className="section" aria-labelledby="transfer-title">
        <h2 id="transfer-title">Transfer ownership</h2>
        <p className="muted">
          The new owner gets every permission. You become an admin.
        </p>
        <div className="inline-form">
          <Select
            label="New owner"
            placeholder="Choose a member"
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
            Transfer
          </Button>
        </div>
      </section>
      <section className="section danger-zone" aria-labelledby="delete-title">
        <h2 id="delete-title">Delete organization</h2>
        <p className="muted">
          Ends every sandbox and removes every device, API key and member.
        </p>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirmDelete(true)}
        >
          Delete organization
        </Button>
      </section>
      <ConfirmDialog
        open={confirmTransfer}
        onOpenChange={setConfirmTransfer}
        title={`Make ${targetName} the owner?`}
        description="You become an admin. Only the new owner can give ownership back. The transfer removes the billing link, so the organization is blocked until the new owner links a billing account."
        confirmLabel="Transfer ownership"
        onConfirm={() =>
          transfer.mutateAsync({ organizationId, userId: target! })
        }
      />
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={`Delete ${name}?`}
        description="Every sandbox ends and every device is removed. This cannot be undone."
        confirmLabel="Delete organization"
        onConfirm={async () => {
          await remove.mutateAsync({ organizationId });
          navigate("/");
          void invalidateServices(queryClient, OrganizationService);
        }}
      />
    </>
  );
}
