// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useMutation,
  useQuery,
} from "@connectrpc/connect-query";
import {
  AgentUpdateState,
  Arch,
  BaseImage,
  DeviceState,
  GuestOs,
  HostOs,
} from "@delino/ownsand-sdk/gen/common_pb";
import {
  DeviceService,
  type Device,
} from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { Card, CardActions, CardDescription, CardTitle } from "delino-ui/card";
import { TextField } from "delino-ui/text-field";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/errors.ts";
import {
  ErrorState,
  LoadingState,
  PageHeader,
  formText,
} from "../components/common.tsx";
import { ConfirmDialog } from "../components/confirm-dialog.tsx";
import { formatEnum, formatResources, formatTime } from "../format.ts";
import { Permission, can } from "../roles.ts";
import { Link, useRouter } from "../router.tsx";
import { RouteId, organizationPath } from "../routes.ts";
import type { OrganizationPageProps } from "./organization-layout.tsx";

/** Parses a non-negative whole number; `undefined` for anything else. */
export function parseLimit(value: string): bigint | undefined {
  if (!/^\d{1,12}$/.test(value)) {
    return undefined;
  }
  return BigInt(value);
}

function DeviceSummary({ device }: { readonly device: Device }) {
  const capabilities = device.capabilities;
  const rows: [string, string][] = [
    ["State", formatEnum(DeviceState, device.state)],
    ["Device ID", device.deviceId],
    [
      "Platform",
      `${formatEnum(HostOs, capabilities?.hostOs ?? 0)} · ${formatEnum(Arch, capabilities?.arch ?? 0)}`,
    ],
    [
      "Capacity",
      capabilities
        ? `${capabilities.logicalCpuCount} CPU · ${capabilities.memoryMib} MiB · ${capabilities.freeDiskBytes} free disk bytes`
        : "—",
    ],
    [
      "Guest systems",
      capabilities && capabilities.guestOses.length > 0
        ? capabilities.guestOses.map((os) => formatEnum(GuestOs, os)).join(", ")
        : "—",
    ],
    [
      "Built base images",
      capabilities && capabilities.builtBaseImages.length > 0
        ? capabilities.builtBaseImages
            .map((image) => formatEnum(BaseImage, image))
            .join(", ")
        : "—",
    ],
    ["Limits", formatResources(device.limits)],
    ["Allocated", formatResources(device.allocated)],
    [
      "Agent version",
      `${device.agentVersion || "—"} (${formatEnum(AgentUpdateState, device.updateState)})`,
    ],
    ["Enrolled", formatTime(device.enrollTime)],
    ["Last seen", formatTime(device.lastSeenTime)],
  ];
  return (
    <dl className="details">
      {rows.map(([term, value]) => (
        <div key={term} className="details-row">
          <dt>{term}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

function DeviceLimitsForm({
  organizationId,
  device,
}: {
  readonly organizationId: string;
  readonly device: Device;
}) {
  const queryClient = useQueryClient();
  const update = useMutation(DeviceService.method.updateDeviceLimits);
  const [error, setError] = useState<string | undefined>(undefined);
  const [saved, setSaved] = useState(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const vcpuCount = parseLimit(formText(form, "vcpuCount"));
    const memoryMib = parseLimit(formText(form, "memoryMib"));
    const diskGib = parseLimit(formText(form, "diskGib"));
    if (
      vcpuCount === undefined ||
      memoryMib === undefined ||
      diskGib === undefined
    ) {
      setError("Enter whole numbers of 0 or more.");
      return;
    }
    setError(undefined);
    setSaved(false);
    try {
      await update.mutateAsync({
        organizationId,
        deviceId: device.deviceId,
        limits: { vcpuCount, memoryMib, diskGib },
      });
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: DeviceService,
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
      <CardTitle>Limits</CardTitle>
      <CardDescription>
        The vCPUs, memory and disk that sandboxes on this device may use
        together. A device with zero limits gets no new sandboxes. Lower limits
        do not stop running sandboxes.
      </CardDescription>
      <form
        onSubmit={(event) => void submit(event)}
        noValidate
        className="limits-form"
      >
        <TextField
          label="vCPUs"
          name="vcpuCount"
          inputMode="numeric"
          defaultValue={String(device.limits?.vcpuCount ?? 0n)}
        />
        <TextField
          label="Memory (MiB)"
          name="memoryMib"
          inputMode="numeric"
          defaultValue={String(device.limits?.memoryMib ?? 0n)}
        />
        <TextField
          label="Disk (GiB)"
          name="diskGib"
          inputMode="numeric"
          defaultValue={String(device.limits?.diskGib ?? 0n)}
        />
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        {saved ? (
          <p className="muted" role="status">
            Limits saved.
          </p>
        ) : null}
        <CardActions>
          <Button type="submit" disabled={update.isPending}>
            {update.isPending ? "Saving…" : "Save limits"}
          </Button>
        </CardActions>
      </form>
    </Card>
  );
}

function RemoveDevice({
  organizationId,
  device,
}: {
  readonly organizationId: string;
  readonly device: Device;
}) {
  const queryClient = useQueryClient();
  const { navigate } = useRouter();
  const remove = useMutation(DeviceService.method.removeDevice);
  const [confirming, setConfirming] = useState(false);
  return (
    <Card className="danger-zone">
      <CardTitle>Remove device</CardTitle>
      <CardDescription>
        The device leaves the organization. Its agent deletes the sandboxes,
        snapshots and images on the device. To use the device again, enroll it
        with a new enrollment token.
      </CardDescription>
      <CardActions>
        <Button
          variant={ButtonVariant.Danger}
          onClick={() => setConfirming(true)}
        >
          Remove device
        </Button>
      </CardActions>
      <ConfirmDialog
        open={confirming}
        title={`Remove ${device.name || "this device"}?`}
        description="Its sandboxes end, and its agent deletes the sandboxes, snapshots and images on the device. This cannot be undone."
        confirmLabel="Remove device"
        onClose={() => setConfirming(false)}
        onConfirm={async () => {
          await remove.mutateAsync({
            organizationId,
            deviceId: device.deviceId,
          });
          await queryClient.invalidateQueries({
            queryKey: createConnectQueryKey({
              schema: DeviceService,
              cardinality: undefined,
            }),
          });
          navigate(organizationPath(organizationId, RouteId.Devices));
        }}
      />
    </Card>
  );
}

export function DevicePage({
  organizationId,
  role,
  deviceId,
}: OrganizationPageProps & { readonly deviceId: string }) {
  const query = useQuery(DeviceService.method.getDevice, {
    organizationId,
    deviceId,
  });
  const device = query.data?.device;
  return (
    <main className="page">
      <Link
        className="text-link muted"
        to={organizationPath(organizationId, RouteId.Devices)}
      >
        ← Devices
      </Link>
      {query.isPending ? (
        <LoadingState />
      ) : query.isError || device === undefined ? (
        <ErrorState error={query.error} />
      ) : (
        <>
          <PageHeader title={device.name || device.deviceId} />
          <DeviceSummary device={device} />
          {can(role, Permission.ManageDevices) &&
          device.state !== DeviceState.REMOVED ? (
            <section aria-labelledby="device-admin-settings">
              <h2 id="device-admin-settings">Admin settings</h2>
              <DeviceLimitsForm
                key={device.deviceId}
                organizationId={organizationId}
                device={device}
              />
              <RemoveDevice organizationId={organizationId} device={device} />
            </section>
          ) : null}
        </>
      )}
    </main>
  );
}
