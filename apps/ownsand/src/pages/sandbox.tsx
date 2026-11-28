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
import {
  SnapshotService,
  type Template,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { toast } from "delino-ui/toast";
import { type ReactNode, useEffect, useRef, useState } from "react";
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
import { formatDuration, formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import { enumText } from "../text.ts";
import { LabelList, resourcesText } from "./sandboxes.tsx";

/**
 * The source of a sandbox. A template shows its name when `ListTemplates`
 * (the first page) holds it. While that request loads or fails, and when the
 * template is deleted or beyond the first page, the row shows the short ID,
 * so the row never waits for or reports the lookup.
 */
function sourceText(
  s: Sandbox,
  templates: readonly Template[],
  i18n: ReturnType<typeof useI18n>,
): ReactNode {
  const src = s.source?.source;
  switch (src?.case) {
    case "baseImage":
      return i18n.t("sandboxes.source.baseImage", {
        image: enumText(i18n, labels.baseImage, src.value),
      });
    case "snapshotId":
      return i18n.rich("sandbox.source.snapshot", {
        id: <ShortId id={src.value} />,
      });
    case "templateId": {
      const id = src.value;
      const template = templates.find((t) => t.templateId === id);
      return i18n.rich("sandbox.source.template", {
        template:
          template === undefined ? (
            <ShortId id={id} />
          ) : (
            <span title={id}>{template.name}</span>
          ),
      });
    }
    default:
      return "—";
  }
}

/** Route `sandbox`: one sandbox with pause, resume, snapshot and delete. */
export function SandboxPage({ sandboxId }: { sandboxId: string }) {
  const { organizationId } = useOrg();
  const i18n = useI18n();
  const { t, lang } = i18n;
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
  // Only a template source needs a name; the other sources send no request.
  const templates = useQuery(
    SnapshotService.method.listTemplates,
    { organizationId, pageSize: 200 },
    { enabled: query.data?.sandbox?.source?.source.case === "templateId" },
  );
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

  if (query.isPending) return <Loading label={t("sandbox.loading")} />;
  if (query.data === undefined || showsFullError(query))
    return (
      <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />
    );
  const s = query.data.sandbox!;
  const device = devices.data?.devices.find((d) => d.deviceId === s.deviceId);
  const ended =
    s.state === SandboxState.DELETED || s.state === SandboxState.FAILED;
  const act = async <T,>(
    run: () => Promise<T>,
    notify: (response: T) => void,
  ) => {
    try {
      notify(await run());
    } catch (error) {
      toastError(error, i18n);
    }
  };
  const ids = { organizationId, sandboxId };

  return (
    <>
      {query.error ? <RefreshFailedNotice query={query} /> : null}
      <PageHeader
        title={t("sandbox.title")}
        description={<ShortId id={s.sandboxId} />}
        action={
          <div className="actions">
            {s.state === SandboxState.RUNNING ? (
              <Button
                variant={ButtonVariant.Secondary}
                onClick={() =>
                  act(
                    () => pause.mutateAsync(ids),
                    ({ sandbox }) => {
                      if (sandbox?.state === SandboxState.PAUSED) {
                        toast.success(t("sandbox.paused"));
                      } else {
                        toast.info(t("sandbox.pauseRequested"));
                      }
                    },
                  )
                }
              >
                {t("sandbox.pause")}
              </Button>
            ) : null}
            {s.state === SandboxState.PAUSED ? (
              <Button
                onClick={() =>
                  act(
                    () => resume.mutateAsync(ids),
                    ({ sandbox }) => {
                      if (sandbox?.state === SandboxState.RUNNING) {
                        toast.success(t("sandbox.running"));
                      } else {
                        toast.info(t("sandbox.resumeRequested"));
                      }
                    },
                  )
                }
              >
                {t("sandbox.resume")}
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
                    () => toast.success(t("sandbox.snapshotStarted")),
                  )
                }
              >
                {t("sandbox.takeSnapshot")}
              </Button>
            ) : null}
            {!ended ? (
              <Button
                variant={ButtonVariant.Danger}
                onClick={() => setConfirmDelete(true)}
              >
                {t("common.delete")}
              </Button>
            ) : null}
          </div>
        }
      />
      <dl className="details">
        <Detail term={t("sandbox.details.state")}>
          <SandboxStateBadge
            state={s.state}
            lastKnownState={s.lastKnownState}
          />
        </Detail>
        {s.endReason !== undefined ? (
          <Detail term={t("sandbox.details.endReason")}>
            {enumText(i18n, labels.endReason, s.endReason)}
          </Detail>
        ) : null}
        <Detail term={t("sandbox.details.device")}>
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
        <Detail term={t("sandbox.details.source")}>
          {sourceText(s, templates.data?.templates ?? [], i18n)}
        </Detail>
        <Detail term={t("sandbox.details.resources")}>
          {resourcesText(s.resources, i18n, "sandbox.resourcesValue")}
        </Detail>
        <Detail term={t("sandbox.details.guest")}>
          {t("sandbox.guestValue", {
            guest: enumText(i18n, labels.guestOs, s.guestOs),
            host: enumText(i18n, labels.hostOs, s.hostOs),
            arch: enumText(i18n, labels.arch, s.arch),
          })}
        </Detail>
        <Detail term={t("sandbox.details.network")}>
          {enumText(i18n, labels.network, s.network?.mode ?? 0)}
        </Detail>
        <Detail term={t("sandbox.details.idleTtl")}>
          {formatDuration(s.idleTtl, i18n)}
        </Detail>
        <Detail term={t("sandbox.details.maxTtl")}>
          {formatDuration(s.maxTtl, i18n)}
        </Detail>
        <Detail term={t("sandbox.details.idleDeadline")}>
          {ended ? "—" : formatTime(s.idleDeadline, lang)}
        </Detail>
        <Detail term={t("sandbox.details.maxDeadline")}>
          {ended ? "—" : formatTime(s.maxDeadline, lang)}
        </Detail>
        <Detail term={t("sandbox.details.created")}>
          {formatTime(s.createTime, lang)}
        </Detail>
        <Detail term={t("sandbox.details.labels")}>
          <LabelList labels={s.labels} />
        </Detail>
      </dl>
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={t("sandbox.deleteDialog.title")}
        description={t("sandbox.deleteDialog.description")}
        confirmLabel={t("sandbox.deleteDialog.confirm")}
        onConfirm={() => remove.mutateAsync(ids)}
      />
    </>
  );
}
