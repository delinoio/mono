// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { SandboxState } from "ownsand-sdk/gen/common_pb";
import { DeviceService } from "ownsand-sdk/gen/device_pb";
import { type Sandbox, SandboxService } from "ownsand-sdk/gen/sandbox_pb";
import { SnapshotService } from "ownsand-sdk/gen/snapshot_pb";
import { toast } from "delino-ui/toast";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  Loading,
  PageHeader,
  SandboxStateBadge,
  ShortId,
} from "../components/ui.tsx";
import {
  errorMessage,
  formatDuration,
  formatMib,
  formatTime,
  labels,
} from "../format.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import { LabelList } from "./sandboxes.tsx";

function sourceText(s: Sandbox): string {
  const src = s.source?.source;
  switch (src?.case) {
    case "baseImage":
      return `${labels.baseImage[src.value]} base image`;
    case "snapshotId":
      return `Snapshot ${src.value}`;
    case "templateId":
      return `Template ${src.value}`;
    default:
      return "—";
  }
}

/** Route `sandbox`: one sandbox with pause, resume, snapshot and delete. */
export function SandboxPage({ sandboxId }: { sandboxId: string }) {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const query = useQuery(SandboxService.method.getSandbox, {
    organizationId,
    sandboxId,
  });
  const devices = useQuery(DeviceService.method.listDevices, {
    organizationId,
    pageSize: 200,
  });
  const refresh = () =>
    invalidateServices(queryClient, SandboxService, SnapshotService);
  const pause = useMutation(SandboxService.method.pauseSandbox, {
    onSuccess: refresh,
  });
  const resume = useMutation(SandboxService.method.resumeSandbox, {
    onSuccess: refresh,
  });
  const remove = useMutation(SandboxService.method.deleteSandbox, {
    onSuccess: refresh,
  });
  const snapshot = useMutation(SnapshotService.method.createSnapshot, {
    onSuccess: refresh,
  });
  const [confirmDelete, setConfirmDelete] = useState(false);

  if (query.isPending) return <Loading label="Loading the sandbox" />;
  if (query.error)
    return (
      <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />
    );
  const s = query.data.sandbox!;
  const device = devices.data?.devices.find((d) => d.deviceId === s.deviceId);
  const ended =
    s.state === SandboxState.DELETED || s.state === SandboxState.FAILED;
  const act = async (run: () => Promise<unknown>, done: string) => {
    try {
      await run();
      toast.success(done);
    } catch (error) {
      toast.error(errorMessage(error));
    }
  };
  const ids = { organizationId, sandboxId };

  return (
    <>
      <PageHeader
        title="Sandbox"
        description={<ShortId id={s.sandboxId} />}
        action={
          <div className="actions">
            {s.state === SandboxState.RUNNING ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() =>
                  act(() => pause.mutateAsync(ids), "The sandbox is paused.")
                }
              >
                Pause
              </Button>
            ) : null}
            {s.state === SandboxState.PAUSED ? (
              <Button
                onClick={() =>
                  act(() => resume.mutateAsync(ids), "The sandbox is running.")
                }
              >
                Resume
              </Button>
            ) : null}
            {s.state === SandboxState.RUNNING ||
            s.state === SandboxState.PAUSED ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() =>
                  act(
                    () => snapshot.mutateAsync(ids),
                    "The snapshot is being created.",
                  )
                }
              >
                Take snapshot
              </Button>
            ) : null}
            {!ended ? (
              <Button
                variant={ButtonVariant.Danger}
                onClick={() => setConfirmDelete(true)}
              >
                Delete
              </Button>
            ) : null}
          </div>
        }
      />
      <dl className="details">
        <Detail term="State">
          <SandboxStateBadge
            state={s.state}
            lastKnownState={s.lastKnownState}
          />
        </Detail>
        {s.endReason !== undefined ? (
          <Detail term="End reason">{labels.endReason[s.endReason]}</Detail>
        ) : null}
        <Detail term="Device">
          <Link
            to={routePath({
              id: RouteId.Device,
              organizationId,
              deviceId: s.deviceId,
            })}
          >
            {device?.name ?? s.deviceId}
          </Link>
        </Detail>
        <Detail term="Source">{sourceText(s)}</Detail>
        <Detail term="Resources">
          {s.resources
            ? `${s.resources.vcpuCount} vCPU · ${formatMib(s.resources.memoryMib)} memory · ${s.resources.diskGib} GiB disk`
            : "—"}
        </Detail>
        <Detail term="Guest">
          {labels.guestOs[s.guestOs]} on {labels.hostOs[s.hostOs]}{" "}
          {labels.arch[s.arch]}
        </Detail>
        <Detail term="Network">{labels.network[s.network?.mode ?? 0]}</Detail>
        <Detail term="Idle TTL">{formatDuration(s.idleTtl)}</Detail>
        <Detail term="Maximum TTL">{formatDuration(s.maxTtl)}</Detail>
        <Detail term="Idle deadline">
          {ended ? "—" : formatTime(s.idleDeadline)}
        </Detail>
        <Detail term="Maximum deadline">
          {ended ? "—" : formatTime(s.maxDeadline)}
        </Detail>
        <Detail term="Created">{formatTime(s.createTime)}</Detail>
        <Detail term="Labels">
          <LabelList labels={s.labels} />
        </Detail>
      </dl>
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title="Delete this sandbox?"
        description="The sandbox and its disk are deleted on the device. Snapshots of it stay."
        confirmLabel="Delete sandbox"
        onConfirm={() => remove.mutateAsync(ids)}
      />
    </>
  );
}
