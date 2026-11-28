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
  ListNoun,
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
  formatPercent,
  formatPlainNumber,
  formatTime,
  labels,
  OVERCOMMIT_PERCENTAGES,
} from "../format.ts";
import { DeviceLimitField, deviceLimitsInput } from "../forms.ts";
import { useI18n } from "../i18n.ts";
import { can, Permission } from "../roles.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { enumText, type Text } from "../text.ts";

function vcpuSummary(d: Device, text: Text): string {
  const l = d.limits;
  if (l === undefined) return "—";
  return text.t("devices.vcpuSummary", {
    allocated: formatPlainNumber(deviceAllocatedVcpu(d), text.lang),
    effective: formatPlainNumber(d.effectiveVcpuLimitCount, text.lang),
    limit: formatPlainNumber(l.vcpuCount, text.lang),
    ratio: formatPlainNumber(l.vcpuOvercommitPercent / 100, text.lang),
  });
}

/** The host OS and architecture of a device, for example `Linux x86-64`. */
function platformText(d: Device, text: Text): string {
  return text.t("devices.platformValue", {
    os: enumText(text, labels.hostOs, d.hostOs),
    arch: enumText(text, labels.arch, d.arch),
  });
}

/** The agent version and its update state. */
function agentText(d: Device, text: Text): string {
  return text.t("devices.agentValue", {
    version: d.agentVersion,
    state: enumText(text, labels.updateState, d.updateState),
  });
}

/** Route `devices`: devices with state, limits, allocation and agent version. */
export function DevicesPage() {
  const { organizationId, role } = useOrg();
  const { devices, boundary, pages } = useDevices(organizationId);
  const i18n = useI18n();
  const { t, lang } = i18n;
  return (
    <>
      <PageHeader
        title={t("devices.title")}
        description={t("devices.description")}
        action={
          can(role, Permission.ManageEnrollmentTokens) ? (
            <Link
              className="button-link"
              to={routePath({ id: RouteId.EnrollmentTokens, organizationId })}
            >
              {t("devices.add")}
            </Link>
          ) : undefined
        }
      />
      {showsFullError(boundary) ? null : (
        <MorePagesNotice noun={ListNoun.Devices} pages={pages} />
      )}
      <QueryBoundary query={boundary} loading={t("devices.loading")}>
        {() =>
          devices.length === 0 ? (
            <Empty title={t("devices.empty.title")}>
              {t("devices.empty.description")}
            </Empty>
          ) : (
            <TableRegion label={t("devices.table.label")}>
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">{t("devices.table.device")}</th>
                    <th scope="col">{t("devices.table.state")}</th>
                    <th scope="col">{t("devices.table.platform")}</th>
                    <th scope="col">{t("devices.table.vcpus")}</th>
                    <th scope="col">{t("devices.table.memory")}</th>
                    <th scope="col">{t("devices.table.disk")}</th>
                    <th scope="col">{t("devices.table.agent")}</th>
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
                      <td>{platformText(d, i18n)}</td>
                      <td>{vcpuSummary(d, i18n)}</td>
                      <td>
                        {t("devices.memoryAllocated", {
                          allocated: formatMib(
                            d.allocation?.memoryMib ?? 0n,
                            lang,
                          ),
                          limit: formatMib(d.limits?.memoryMib ?? 0n, lang),
                        })}
                      </td>
                      <td>
                        {t("devices.diskAllocated", {
                          allocated: formatPlainNumber(
                            d.allocation?.diskGib ?? 0n,
                            lang,
                          ),
                          limit: formatPlainNumber(
                            d.limits?.diskGib ?? 0n,
                            lang,
                          ),
                        })}
                      </td>
                      <td>{agentText(d, i18n)}</td>
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
  const i18n = useI18n();
  const { t, lang, rich } = i18n;
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

  if (query.isPending) return <Loading label={t("device.loading")} />;
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
  const clearError = (field: DeviceLimitField) =>
    setFieldError((current) => (current?.field === field ? null : current));

  const edit =
    <K extends keyof DeviceLimitsDraft>(field: DeviceLimitField, key: K) =>
    (value: NonNullable<DeviceLimitsDraft[K]> | null) => {
      setDraft((current) => ({ ...current, [key]: value }));
      clearError(field);
    };

  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (saving.current) return;
    // ReplaceDeviceLimits needs every value. A blank field is not zero: the
    // form asks for the value instead of sending a silent zero.
    const input = deviceLimitsInput(
      {
        vcpu: vcpuDraft.current?.hasFraction()
          ? Number.NaN
          : (vcpuDraft.current?.valueBelowMin() ?? vcpu),
        memoryMib: memory,
        diskGib: disk,
        overcommitPercent: Number(overcommit),
      },
      i18n,
    );
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
      toast.success(t("device.limitsSaved"));
    } catch (error) {
      // The draft stays as the user left it and the form is usable again.
      toastError(error, i18n);
    } finally {
      saving.current = false;
    }
  };

  return (
    <>
      {query.error ? <RefreshFailedNotice query={query} /> : null}
      <PageHeader
        title={d.name || t("device.title")}
        description={<DeviceStateBadge state={d.state} />}
      />
      <dl className="details">
        <Detail term={t("device.details.platform")}>
          {platformText(d, i18n)}
        </Detail>
        <Detail term={t("device.details.agent")}>{agentText(d, i18n)}</Detail>
        <Detail term={t("device.details.lastSeen")}>
          {formatTime(d.lastSeenTime, lang)}
        </Detail>
        <Detail term={t("device.details.capacity")}>
          {d.capabilities
            ? t("device.capacityValue", {
                cpus: formatPlainNumber(d.capabilities.logicalCpuCount, lang),
                memory: formatMib(d.capabilities.memoryMib, lang),
                disk: formatBytes(d.capabilities.freeDiskBytes, lang),
              })
            : "—"}
        </Detail>
        <Detail term={t("device.details.guests")}>
          {new Intl.ListFormat(lang, { type: "unit", style: "short" }).format(
            d.capabilities?.guestOses.map((g) =>
              enumText(i18n, labels.guestOs, g),
            ) ?? [],
          ) || "—"}
        </Detail>
        <Detail term={t("device.details.allocated")}>
          {t("sandboxes.resourcesValue", {
            vcpu: formatPlainNumber(deviceAllocatedVcpu(d), lang),
            memory: formatMib(d.allocation?.memoryMib ?? 0n, lang),
            disk: formatPlainNumber(d.allocation?.diskGib ?? 0n, lang),
          })}
        </Detail>
      </dl>
      <section className="section" aria-labelledby="limits-title">
        <h2 id="limits-title">{t("device.limits.title")}</h2>
        <p className="muted">{t("device.limits.description")}</p>
        <form className="form" onSubmit={save}>
          <div className="field-row">
            <NumberInput
              label={t("device.limits.vcpu")}
              decrementLabel={t("common.decrease")}
              incrementLabel={t("common.increase")}
              value={vcpu}
              onValueChange={edit(DeviceLimitField.Vcpu, "vcpu")}
              onInput={() => clearError(DeviceLimitField.Vcpu)}
              draftRef={vcpuDraft}
              preserveBelowMin
              min={0}
              error={errorOf(DeviceLimitField.Vcpu)}
              disabled={!admin || pending}
            />
            <TextField
              label={t("device.limits.memory")}
              value={memory}
              onValueChange={edit(DeviceLimitField.Memory, "memory")}
              inputMode="numeric"
              error={errorOf(DeviceLimitField.Memory)}
              disabled={!admin || pending}
            />
            <TextField
              label={t("device.limits.disk")}
              value={disk}
              onValueChange={edit(DeviceLimitField.Disk, "disk")}
              inputMode="numeric"
              error={errorOf(DeviceLimitField.Disk)}
              disabled={!admin || pending}
            />
          </div>
          <Select
            label={t("device.limits.overcommit")}
            value={overcommit}
            onValueChange={(v) =>
              v && setDraft((c) => ({ ...c, overcommit: v }))
            }
            disabled={!admin || pending}
          >
            {OVERCOMMIT_PERCENTAGES.map((p) => (
              <SelectOption key={p} value={String(p)}>
                {p === 100
                  ? t("device.limits.overcommitNone", {
                      percent: formatPercent(p, lang),
                    })
                  : t("device.limits.overcommitValue", {
                      percent: formatPercent(p, lang),
                      ratio: formatPlainNumber(p / 100, lang),
                    })}
              </SelectOption>
            ))}
          </Select>
          <p
            className="callout"
            role="status"
            aria-label={t("device.limits.effective")}
          >
            {rich("device.limits.effectiveValue", {
              value: <strong>{formatPlainNumber(effective, lang)}</strong>,
            })}
          </p>
          {admin ? (
            <div className="actions">
              <Button type="submit" disabled={pending}>
                {t("device.limits.save")}
              </Button>
            </div>
          ) : null}
        </form>
      </section>
      {admin ? (
        <section className="section danger-zone" aria-labelledby="remove-title">
          <h2 id="remove-title">{t("device.remove.title")}</h2>
          <p className="muted">{t("device.remove.description")}</p>
          <Button
            variant={ButtonVariant.Danger}
            onClick={() => setConfirmRemove(true)}
          >
            {t("device.remove.button")}
          </Button>
        </section>
      ) : null}
      <ConfirmDialog
        open={confirmRemove}
        onOpenChange={setConfirmRemove}
        title={
          d.name
            ? t("device.remove.dialogTitle", { name: d.name })
            : t("device.remove.dialogTitleUnnamed")
        }
        description={t("device.remove.dialogDescription")}
        confirmLabel={t("device.remove.button")}
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
