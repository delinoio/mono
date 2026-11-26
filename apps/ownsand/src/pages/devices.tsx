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
import { NumberInput, type NumberInputDraft } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { DeviceState } from "@delino/ownsand-protos/v1/common_pb";
import {
  type Device,
  DeviceService,
  GetDeviceResponseSchema,
} from "@delino/ownsand-protos/v1/device_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { type FormEvent, useRef, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  DeviceStateBadge,
  Empty,
  ErrorNotice,
  Loading,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  RefreshFailedNotice,
  TableRegion,
  showsFullError,
  toastError,
} from "../components/ui.tsx";
import { useDevices } from "../devices.ts";
import {
  deviceAllocatedVcpu,
  effectiveVcpuLimit,
  formatBytes,
  formatMib,
  formatTime,
  labels,
  OVERCOMMIT_PERCENTAGES,
} from "../format.ts";
import { DeviceLimitField, deviceLimitsInput } from "../forms.ts";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

function vcpuSummary(d: Device): string {
  const l = d.limits;
  if (l === undefined) return "—";
  const ratio = l.vcpuOvercommitPercent / 100;
  return `${deviceAllocatedVcpu(d)} of ${d.effectiveVcpuLimitCount} (${l.vcpuCount} × ${ratio})`;
}

/** Route `devices`: devices with state, limits, allocation and agent version. */
export function DevicesPage() {
  const { organizationId, role } = useOrg();
  const { devices, boundary, pages } = useDevices(organizationId);
  return (
    <>
      <PageHeader
        title="Devices"
        description="Your own machines that run sandboxes. A new device runs nothing until an admin sets its limits."
        action={
          can(role, Permission.ManageEnrollmentTokens) ? (
            <Link
              className="button-link"
              to={routePath({ id: RouteId.EnrollmentTokens, organizationId })}
            >
              Add a device
            </Link>
          ) : undefined
        }
      />
      {showsFullError(boundary) ? null : (
        <MorePagesNotice noun="devices" pages={pages} />
      )}
      <QueryBoundary query={boundary} loading="Loading devices">
        {() =>
          devices.length === 0 ? (
            <Empty title="No devices yet">
              An admin creates an enrollment token and installs the agent on a
              device.
            </Empty>
          ) : (
            <TableRegion label="Device list">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Device</th>
                    <th scope="col">State</th>
                    <th scope="col">Platform</th>
                    <th scope="col">vCPUs allocated</th>
                    <th scope="col">Memory allocated</th>
                    <th scope="col">Disk allocated</th>
                    <th scope="col">Agent</th>
                  </tr>
                </thead>
                <tbody>
                  {devices.map((d) => (
                    <tr key={d.deviceId}>
                      <td>
                        <Link
                          to={routePath({
                            id: RouteId.Device,
                            organizationId,
                            deviceId: d.deviceId,
                          })}
                        >
                          {d.name || d.deviceId}
                        </Link>
                      </td>
                      <td>
                        <DeviceStateBadge state={d.state} />
                      </td>
                      <td>
                        {labels.hostOs[d.hostOs]} {labels.arch[d.arch]}
                      </td>
                      <td>{vcpuSummary(d)}</td>
                      <td>
                        {formatMib(d.allocation?.memoryMib ?? 0n)} of{" "}
                        {formatMib(d.limits?.memoryMib ?? 0n)}
                      </td>
                      <td>
                        {String(d.allocation?.diskGib ?? 0n)} of{" "}
                        {String(d.limits?.diskGib ?? 0n)} GiB
                      </td>
                      <td>
                        {d.agentVersion} · {labels.updateState[d.updateState]}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </TableRegion>
          )
        }
      </QueryBoundary>
    </>
  );
}

/**
 * The unsaved edits of the device limits form. A field that is `undefined`
 * is untouched; `vcpu` is `null` when the user blanked the input.
 */
interface DeviceLimitsDraft {
  vcpu?: number | null;
  memory?: string;
  disk?: string;
  overcommit?: string;
}

/**
 * Route `device`: the per-device admin settings. Admins change the limits
 * (including vCPU overcommit) and remove the device; members read them.
 */
export function DevicePage({ deviceId }: { deviceId: string }) {
  const { organizationId, role } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const transport = useTransport();
  const query = useQuery(DeviceService.method.getDevice, {
    organizationId,
    deviceId,
  });
  const update = useMutation(DeviceService.method.replaceDeviceLimits, {
    onSuccess: ({ device }, { organizationId, deviceId }) => {
      if (device !== undefined) {
        // Use the confirmed device if the readback fails.
        queryClient.setQueryData(
          createConnectQueryKey({
            schema: DeviceService.method.getDevice,
            input: { organizationId, deviceId },
            transport,
            cardinality: "finite",
          }),
          create(GetDeviceResponseSchema, { device }),
        );
      }
      return invalidateServices(queryClient, DeviceService);
    },
  });
  const remove = useMutation(DeviceService.method.removeDevice, {
    onSuccess: () => {
      // Removing a device ends its sandboxes and deletes its local snapshots
      // and templates. Refresh all three services in the background so the
      // confirmation can navigate even while GetDevice is still pending.
      void invalidateServices(
        queryClient,
        DeviceService,
        SandboxService,
        SnapshotService,
      );
    },
  });
  // The fields the user has edited and not saved yet. A field that is absent
  // (`undefined`) is untouched and shows the cached limit, so a refresh
  // that changes the limits updates it and never replaces an edit.
  const vcpuDraft = useRef<NumberInputDraft>(null);
  const [draft, setDraft] = useState<DeviceLimitsDraft>({});
  const [fieldError, setFieldError] = useState<{
    field: DeviceLimitField;
    message: string;
  } | null>(null);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const device = query.data?.device;
  // `update.isPending` lasts until the refetch that follows the save has
  // settled (the mutation awaits `onSuccess`), so it also covers the moment
  // when the saved limits are read back. The fields are disabled for that
  // whole window, so no edit can be made between the submit and the clearing
  // of the saved draft. `saving` is the synchronous guard for a second
  // submit in the same tick, before React renders the disabled button.
  const saving = useRef(false);
  const pending = update.isPending;

  if (query.isPending) return <Loading label="Loading the device" />;
  if (query.data === undefined || showsFullError(query))
    return (
      <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />
    );
  const d = device!;
  const admin =
    can(role, Permission.ManageDevices) && d.state !== DeviceState.REMOVED;
  // The shown value of each field is its edit, else the cached limit.
  // Memory and disk are the digits of the stored int64 limits: a number
  // cannot hold a value above 2^53 exactly, so saving another limit would
  // otherwise round them.
  const vcpu =
    draft.vcpu !== undefined ? draft.vcpu : (d.limits?.vcpuCount ?? null);
  const memory = draft.memory ?? (d.limits ? String(d.limits.memoryMib) : "");
  const disk = draft.disk ?? (d.limits ? String(d.limits.diskGib) : "");
  const overcommit =
    draft.overcommit ??
    (d.limits ? String(d.limits.vcpuOvercommitPercent || 100) : "100");
  const effective = effectiveVcpuLimit(vcpu ?? 0, Number(overcommit));

  const errorOf = (field: DeviceLimitField) =>
    fieldError?.field === field ? fieldError.message : undefined;

  // Editing a field clears its error: the message describes the old value.
  const edit =
    <K extends keyof DeviceLimitsDraft>(field: DeviceLimitField, key: K) =>
    (value: NonNullable<DeviceLimitsDraft[K]> | null) => {
      setDraft((current) => ({ ...current, [key]: value }));
      setFieldError((current) => (current?.field === field ? null : current));
    };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saving.current) return;
    // ReplaceDeviceLimits needs every value. A blank field is not zero: the
    // form asks for the value instead of sending a silent zero.
    const input = deviceLimitsInput({
      vcpu: vcpuDraft.current?.hasFraction() ? Number.NaN : vcpu,
      memoryMib: memory,
      diskGib: disk,
      overcommitPercent: Number(overcommit),
    });
    if ("error" in input) {
      setFieldError({ field: input.field, message: input.error });
      toast.error(input.error);
      return;
    }
    setFieldError(null);
    saving.current = true;
    try {
      await update.mutateAsync({
        organizationId,
        deviceId,
        limits: input.limits,
      });
      // The cache holds the confirmed or refreshed limits. Drop the submitted
      // edits. An edit that differs from what was submitted stays.
      setDraft((current) => ({
        vcpu:
          current.vcpu === input.limits.vcpuCount ? undefined : current.vcpu,
        memory: current.memory === memory ? undefined : current.memory,
        disk: current.disk === disk ? undefined : current.disk,
        overcommit:
          current.overcommit === overcommit ? undefined : current.overcommit,
      }));
      toast.success("The device limits are saved.");
    } catch (error) {
      // The draft stays as the user left it and the form is usable again.
      toastError(error);
    } finally {
      saving.current = false;
    }
  };

  return (
    <>
      {query.error ? <RefreshFailedNotice query={query} /> : null}
      <PageHeader
        title={d.name || "Device"}
        description={<DeviceStateBadge state={d.state} />}
      />
      <dl className="details">
        <Detail term="Platform">
          {labels.hostOs[d.hostOs]} {labels.arch[d.arch]}
        </Detail>
        <Detail term="Agent">
          {d.agentVersion} · {labels.updateState[d.updateState]}
        </Detail>
        <Detail term="Last seen">{formatTime(d.lastSeenTime)}</Detail>
        <Detail term="Host capacity">
          {d.capabilities
            ? `${d.capabilities.logicalCpuCount} CPUs · ${formatMib(d.capabilities.memoryMib)} memory · ${formatBytes(d.capabilities.freeDiskBytes)} free disk`
            : "—"}
        </Detail>
        <Detail term="Guests">
          {d.capabilities?.guestOses.map((g) => labels.guestOs[g]).join(", ") ||
            "—"}
        </Detail>
        <Detail term="Allocated">
          {`${deviceAllocatedVcpu(d)} vCPU · ${formatMib(d.allocation?.memoryMib ?? 0n)} · ${d.allocation?.diskGib ?? 0n} GiB disk`}
        </Detail>
      </dl>
      <section className="section" aria-labelledby="limits-title">
        <h2 id="limits-title">Limits</h2>
        <p className="muted">
          The sandboxes on this device share these limits. vCPU overcommit lets
          the scheduler allocate more vCPUs than the limit; memory and disk are
          never overcommitted.
        </p>
        <form className="form" onSubmit={save}>
          <div className="field-row">
            <NumberInput
              label="vCPU limit"
              value={vcpu}
              onValueChange={edit(DeviceLimitField.Vcpu, "vcpu")}
              draftRef={vcpuDraft}
              min={0}
              error={errorOf(DeviceLimitField.Vcpu)}
              disabled={!admin || pending}
            />
            <TextField
              label="Memory limit (MiB)"
              value={memory}
              onValueChange={edit(DeviceLimitField.Memory, "memory")}
              inputMode="numeric"
              error={errorOf(DeviceLimitField.Memory)}
              disabled={!admin || pending}
            />
            <TextField
              label="Disk limit (GiB)"
              value={disk}
              onValueChange={edit(DeviceLimitField.Disk, "disk")}
              inputMode="numeric"
              error={errorOf(DeviceLimitField.Disk)}
              disabled={!admin || pending}
            />
          </div>
          <Select
            label="vCPU overcommit"
            value={overcommit}
            onValueChange={(v) =>
              v && setDraft((c) => ({ ...c, overcommit: v }))
            }
            disabled={!admin || pending}
          >
            {OVERCOMMIT_PERCENTAGES.map((p) => (
              <SelectOption key={p} value={String(p)}>
                {p === 100 ? "None (100%)" : `${p}% (${p / 100}×)`}
              </SelectOption>
            ))}
          </Select>
          <p
            className="callout"
            role="status"
            aria-label="Effective vCPU limit"
          >
            Effective vCPU limit: <strong>{effective}</strong>
          </p>
          {admin ? (
            <div className="actions">
              <Button type="submit" disabled={pending}>
                Save limits
              </Button>
            </div>
          ) : null}
        </form>
      </section>
      {admin ? (
        <section className="section danger-zone" aria-labelledby="remove-title">
          <h2 id="remove-title">Remove device</h2>
          <p className="muted">
            Revokes the device credentials and ends its sandboxes. The agent
            deletes its local data when it reconnects.
          </p>
          <Button
            variant={ButtonVariant.Danger}
            onClick={() => setConfirmRemove(true)}
          >
            Remove device
          </Button>
        </section>
      ) : null}
      <ConfirmDialog
        open={confirmRemove}
        onOpenChange={setConfirmRemove}
        title={`Remove ${d.name || "this device"}?`}
        description="Its sandboxes end and it can no longer connect. To use the machine again, enroll it as a new device."
        confirmLabel="Remove device"
        onConfirm={async (opening) => {
          await remove.mutateAsync({ organizationId, deviceId });
          // The redirect belongs to the opening that asked for the removal.
          // After a dismissal the user is on another page, which stays. The
          // mutation's own invalidation has already run.
          if (opening.isCurrent()) {
            navigate(routePath({ id: RouteId.Devices, organizationId }));
          }
        }}
      />
    </>
  );
}
