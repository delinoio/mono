// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { NumberInput } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { DeviceState } from "ownsand-sdk/gen/common_pb";
import { type Device, DeviceService } from "ownsand-sdk/gen/device_pb";
import { type FormEvent, useEffect, useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  DeviceStateBadge,
  Empty,
  ErrorNotice,
  Loading,
  PageHeader,
} from "../components/ui.tsx";
import {
  deviceEffectiveVcpuLimit,
  effectiveVcpuLimit,
  errorMessage,
  formatBytes,
  formatMib,
  formatTime,
  labels,
  OVERCOMMIT_PERCENTAGES,
} from "../format.ts";
import { deviceLimitsInput } from "../forms.ts";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

function vcpuSummary(d: Device): string {
  const l = d.limits;
  if (l === undefined) return "—";
  const ratio = l.vcpuOvercommitPercent / 100;
  return `${d.allocation?.vcpuCount ?? 0} of ${deviceEffectiveVcpuLimit(d)} (${l.vcpuCount} × ${ratio})`;
}

/** Route `devices`: devices with state, limits, allocation and agent version. */
export function DevicesPage() {
  const { organizationId, role } = useOrg();
  const list = useQuery(DeviceService.method.listDevices, {
    organizationId,
    pageSize: 200,
  });
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
      {list.isPending ? (
        <Loading label="Loading devices" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : list.data.devices.length === 0 ? (
        <Empty title="No devices yet">
          An admin creates an enrollment token and installs the agent on a
          device.
        </Empty>
      ) : (
        <div className="table-wrap">
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
              {list.data.devices.map((d) => (
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
        </div>
      )}
    </>
  );
}

/**
 * Route `device`: the per-device admin settings. Admins change the limits
 * (including vCPU overcommit) and remove the device; members read them.
 */
export function DevicePage({ deviceId }: { deviceId: string }) {
  const { organizationId, role } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const query = useQuery(DeviceService.method.getDevice, {
    organizationId,
    deviceId,
  });
  const update = useMutation(DeviceService.method.replaceDeviceLimits, {
    onSuccess: () => invalidateServices(queryClient, DeviceService),
  });
  const remove = useMutation(DeviceService.method.removeDevice, {
    onSuccess: () => invalidateServices(queryClient, DeviceService),
  });
  const [vcpu, setVcpu] = useState<number | null>(null);
  const [memory, setMemory] = useState<number | null>(null);
  const [disk, setDisk] = useState<number | null>(null);
  const [overcommit, setOvercommit] = useState("100");
  const [confirmRemove, setConfirmRemove] = useState(false);
  const device = query.data?.device;

  useEffect(() => {
    if (device?.limits === undefined) return;
    setVcpu(device.limits.vcpuCount);
    setMemory(Number(device.limits.memoryMib));
    setDisk(Number(device.limits.diskGib));
    setOvercommit(String(device.limits.vcpuOvercommitPercent || 100));
  }, [device?.limits]);

  if (query.isPending) return <Loading label="Loading the device" />;
  if (query.error)
    return (
      <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />
    );
  const d = device!;
  const admin =
    can(role, Permission.ManageDevices) && d.state !== DeviceState.REMOVED;
  const effective = effectiveVcpuLimit(vcpu ?? 0, Number(overcommit));

  const save = async (event: FormEvent) => {
    event.preventDefault();
    // ReplaceDeviceLimits needs every value. A blank field is not zero: the
    // form asks for the value instead of sending a silent zero.
    const input = deviceLimitsInput({
      vcpu,
      memoryMib: memory,
      diskGib: disk,
      overcommitPercent: Number(overcommit),
    });
    if ("error" in input) {
      toast.error(input.error);
      return;
    }
    try {
      await update.mutateAsync({
        organizationId,
        deviceId,
        limits: input.limits,
      });
      toast.success("The device limits are saved.");
    } catch (error) {
      toast.error(errorMessage(error));
    }
  };

  return (
    <>
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
          {`${d.allocation?.vcpuCount ?? 0} vCPU · ${formatMib(d.allocation?.memoryMib ?? 0n)} · ${d.allocation?.diskGib ?? 0n} GiB disk`}
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
              onValueChange={setVcpu}
              min={0}
              disabled={!admin}
            />
            <NumberInput
              label="Memory limit (MiB)"
              value={memory}
              onValueChange={setMemory}
              min={0}
              step={256}
              disabled={!admin}
            />
            <NumberInput
              label="Disk limit (GiB)"
              value={disk}
              onValueChange={setDisk}
              min={0}
              disabled={!admin}
            />
          </div>
          <Select
            label="vCPU overcommit"
            value={overcommit}
            onValueChange={(v) => v && setOvercommit(v)}
            disabled={!admin}
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
              <Button type="submit">Save limits</Button>
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
        onConfirm={async () => {
          await remove.mutateAsync({ organizationId, deviceId });
          navigate(routePath({ id: RouteId.Devices, organizationId }));
        }}
      />
    </>
  );
}
