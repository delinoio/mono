// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonVariant } from "delino-ui/button";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import {
  type Sandbox,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { toast } from "delino-ui/toast";
import { useEffect, useRef, useState } from "react";
import {
  invalidateServices,
  POLL_INTERVAL_MS,
  sandboxSettling,
} from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Detail,
  ErrorNotice,
  Loading,
  PageHeader,
  RefreshFailedNotice,
  SandboxStateBadge,
  showsFullError,
  ShortId,
  toastError,
} from "../components/ui.tsx";
import { formatDuration, formatMib, formatTime, labels } from "../format.ts";
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
  const query = useQuery(
    SandboxService.method.getSandbox,
    { organizationId, sandboxId },
    {
      refetchInterval: (q) => {
        const s = q.state.data?.sandbox;
        return s !== undefined && sandboxSettling(s) ? POLL_INTERVAL_MS : false;
      },
    },
  );
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
  // Deleting a sandbox frees its allocation on the device. DeleteSandbox
  // can return while the sandbox is still `DELETING`, so the effect below
  // invalidates the device queries again once the polled state settles.
  const remove = useMutation(SandboxService.method.deleteSandbox, {
    onSuccess: () => {
      void invalidateServices(queryClient, DeviceService);
      return refresh();
    },
  });
  const snapshot = useMutation(SnapshotService.method.createSnapshot, {
    onSuccess: refresh,
  });
  const [confirmDelete, setConfirmDelete] = useState(false);

  // A sandbox that leaves `CREATING` or `DELETING` changes the allocation of
  // its device, so the device queries are stale from that moment.
  const polledState = query.data?.sandbox?.state;
  const previousState = useRef(polledState);
  useEffect(() => {
    const before = previousState.current;
    previousState.current = polledState;
    if (
      before !== undefined &&
      polledState !== undefined &&
      sandboxSettling({ state: before }) &&
      !sandboxSettling({ state: polledState })
    ) {
      void invalidateServices(queryClient, DeviceService);
    }
  }, [polledState, queryClient]);

  if (query.isPending) return <Loading label="Loading the sandbox" />;
  if (query.data === undefined || showsFullError(query))
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
      toastError(error);
    }
  };
  const ids = { organizationId, sandboxId };

  return (
    <>
      {query.error ? <RefreshFailedNotice query={query} /> : null}
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
                // The Promise that `onClick` returns keeps the button pending
                // until the request and the refresh settle, so repeated
                // activations send one request (see the "Take snapshot while
                // a request is pending" test). Do not drop the return value.
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
