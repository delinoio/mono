// SPDX-License-Identifier: Apache-2.0
import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  createInfiniteQueryOptions,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  hashKey,
  useInfiniteQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { NumberInput, type NumberInputDraft } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { Textarea } from "delino-ui/textarea";
import {
  BaseImage,
  NetworkMode,
  SandboxState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import {
  CreateSandboxRequestSchema,
  type Sandbox,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { useEffect, useMemo, useRef, useState } from "react";
import {
  invalidateServices,
  POLL_INTERVAL_MS,
  sandboxSettling,
} from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
  MorePagesNotice,
  PageHeader,
  QueryBoundary,
  SandboxStateBadge,
  ShortId,
  TableRegion,
  isTerminalError,
  shortIdText,
} from "../components/ui.tsx";
import { useDevices } from "../devices.ts";
import { deviceOptionLabel, formatMib, formatTime, labels } from "../format.ts";
import { parseLabels, resourceErrors, ttlErrors } from "../forms.ts";
import { lastRequestFailedOnLaterPage } from "../pagination.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { useSnapshots, useTemplates } from "../snapshot-lists.ts";
import { uuidV7 } from "../uuid.ts";

const ALL = "all";
const CREATE_RESERVATION_MS = 24 * 60 * 60 * 1000;
const SANDBOX_ID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

interface CreateAttempt {
  request: MessageInitShape<typeof CreateSandboxRequestSchema>;
  expiresAt: number;
  status: "pending" | "unconfirmed" | "expired";
  sourceLabel: string;
  deviceLabel: string | undefined;
}

/** Whether CreateSandbox rejected the request before reserving a sandbox. */
function isCreateRejection(error: unknown): boolean {
  return (
    error instanceof ConnectError &&
    (error.code === Code.InvalidArgument ||
      error.code === Code.FailedPrecondition ||
      error.code === Code.NotFound ||
      error.code === Code.PermissionDenied ||
      error.code === Code.Unauthenticated ||
      error.code === Code.ResourceExhausted)
  );
}

/** Route `sandboxes`: the sandbox list with filters and creation. */
export function SandboxesPage() {
  const { organizationId } = useOrg();
  const [state, setState] = useState<string>(ALL);
  const [deviceId, setDeviceId] = useState<string>(ALL);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [creating, setCreating] = useState(false);
  const deviceList = useDevices(organizationId);
  const { devices } = deviceList;
  const deviceNames = useMemo(
    () => new Map(devices.map((d) => [d.deviceId, d.name])),
    [devices],
  );
  const transport = useTransport();
  const listOptions = createInfiniteQueryOptions(
    SandboxService.method.listSandboxes,
    {
      organizationId,
      pageSize: 50,
      pageToken: "",
      state: state === ALL ? undefined : (Number(state) as SandboxState),
      deviceId: deviceId === ALL ? undefined : deviceId,
      includeEnded,
    },
    {
      transport,
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const list = useInfiniteQuery({
    ...listOptions,
    refetchInterval: (query) => {
      // A poll only reloads cached pages and would clear the continuation error.
      if (
        lastRequestFailedOnLaterPage(query) &&
        !isTerminalError(query.state.error)
      ) {
        return false;
      }
      return query.state.data?.pages.some((p) =>
        p.sandboxes.some(sandboxSettling),
      )
        ? POLL_INTERVAL_MS
        : false;
    },
  });
  const listKey = hashKey(listOptions.queryKey);
  const pendingNextPages = useRef(new Set<string>());

  async function requestNextPage() {
    const pending = pendingNextPages.current;
    if (pending.has(listKey)) return;

    // Supersede a background refresh. Guard the continuation before React
    // can disable the button, and hold the guard for this query until it settles.
    pending.add(listKey);
    try {
      await list.fetchNextPage({ cancelRefetch: true });
    } finally {
      pending.delete(listKey);
    }
  }
  const sandboxes = list.data?.pages.flatMap((p) => p.sandboxes) ?? [];
  // A failed "Show more" request is not a failed refresh of the list: the
  // loaded rows stay, and the error and its retry sit next to the button
  // (`QueryBoundary` would offer `refetch`, which reads the first page again
  // and never requests the failed continuation token). A terminal error
  // (`not_found`, `permission_denied`, `unauthenticated`) still takes the
  // boundary's full-error path, like every other list.
  const nextPageFailed =
    list.isFetchNextPageError && !isTerminalError(list.error);
  const boundaryQuery = {
    data: list.data,
    error: nextPageFailed ? null : list.error,
    isPending: list.isPending,
    isFetching: list.isFetching,
    refetch: list.refetch,
  };

  return (
    <>
      <PageHeader
        title="Sandboxes"
        description="Sandboxes run on your organization's devices. The console does not show command output or file contents."
        action={
          <Button onClick={() => setCreating(true)}>Create sandbox</Button>
        }
      />
      <div className="filters">
        <Select
          label="State"
          value={state}
          onValueChange={(v) => setState(v ?? ALL)}
        >
          <SelectOption value={ALL}>All states</SelectOption>
          {Object.values(SandboxState)
            .filter((v): v is SandboxState => typeof v === "number" && v !== 0)
            .map((v) => (
              <SelectOption key={v} value={String(v)}>
                {labels.sandboxState[v]}
              </SelectOption>
            ))}
        </Select>
        <Select
          label="Device"
          value={deviceId}
          onValueChange={(v) => setDeviceId(v ?? ALL)}
        >
          <SelectOption value={ALL}>All devices</SelectOption>
          {devices.map((d) => (
            <SelectOption key={d.deviceId} value={d.deviceId}>
              {d.name}
            </SelectOption>
          ))}
        </Select>
        <Checkbox
          label="Show ended sandboxes"
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      {creating ? null : <DeviceListNotice list={deviceList} />}
      <QueryBoundary query={boundaryQuery} loading="Loading sandboxes">
        {() => (
          <>
            {sandboxes.length === 0 ? (
              <Empty title="No sandboxes match">
                Create a sandbox, or change the filters.
              </Empty>
            ) : (
              <TableRegion label="Sandbox list">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Sandbox</th>
                      <th scope="col">State</th>
                      <th scope="col">Device</th>
                      <th scope="col">Resources</th>
                      <th scope="col">Idle deadline</th>
                      <th scope="col">Max deadline</th>
                      <th scope="col">Labels</th>
                    </tr>
                  </thead>
                  <tbody>
                    {sandboxes.map((s) => (
                      <SandboxRow
                        key={s.sandboxId}
                        sandbox={s}
                        organizationId={organizationId}
                        deviceName={deviceNames.get(s.deviceId)}
                      />
                    ))}
                  </tbody>
                </table>
              </TableRegion>
            )}
            {nextPageFailed && !list.isFetchingNextPage ? (
              <ErrorNotice error={list.error} onRetry={requestNextPage} />
            ) : null}
            {list.hasNextPage ? (
              <Button
                variant={ButtonVariant.Secondary}
                disabled={list.isFetchingNextPage}
                onClick={requestNextPage}
              >
                Show more
              </Button>
            ) : null}
          </>
        )}
      </QueryBoundary>
      <CreateSandboxDialog
        open={creating}
        onOpenChange={setCreating}
        deviceList={deviceList}
      />
    </>
  );
}

function DeviceListNotice({ list }: { list: ReturnType<typeof useDevices> }) {
  return (
    <QueryBoundary query={list.boundary} loading="Loading devices">
      {() => <MorePagesNotice noun="devices" pages={list.pages} />}
    </QueryBoundary>
  );
}

function SandboxRow({
  sandbox: s,
  organizationId,
  deviceName,
}: {
  sandbox: Sandbox;
  organizationId: string;
  deviceName: string | undefined;
}) {
  const ended =
    s.state === SandboxState.DELETED || s.state === SandboxState.FAILED;
  return (
    <tr>
      <td>
        <Link
          to={routePath({
            id: RouteId.Sandbox,
            organizationId,
            sandboxId: s.sandboxId,
          })}
        >
          <ShortId id={s.sandboxId} />
        </Link>
      </td>
      <td>
        <SandboxStateBadge state={s.state} lastKnownState={s.lastKnownState} />
      </td>
      <td>{deviceName ?? <ShortId id={s.deviceId} />}</td>
      <td>
        {s.resources
          ? `${s.resources.vcpuCount} vCPU · ${formatMib(s.resources.memoryMib)} · ${s.resources.diskGib} GiB disk`
          : "—"}
      </td>
      <td>{ended ? "—" : formatTime(s.idleDeadline)}</td>
      <td>{ended ? "—" : formatTime(s.maxDeadline)}</td>
      <td className="wrap">
        <LabelList labels={s.labels} />
      </td>
    </tr>
  );
}

export function LabelList({
  labels: values,
}: {
  labels: Record<string, string>;
}) {
  const entries = Object.entries(values);
  if (entries.length === 0) return <span className="muted">None</span>;
  return (
    <ul className="labels" aria-label="Labels">
      {entries.map(([k, v]) => (
        <li key={k}>
          <Badge>
            {k}={v}
          </Badge>
        </li>
      ))}
    </ul>
  );
}

/** The source of a new sandbox, as one select value. */
function sourceOf(value: string) {
  const [kind, id] = value.split(":", 2) as [string, string];
  if (kind === "template")
    return { source: { case: "templateId" as const, value: id } };
  if (kind === "snapshot")
    return { source: { case: "snapshotId" as const, value: id } };
  return {
    source: { case: "baseImage" as const, value: Number(id) as BaseImage },
  };
}

function CreateSandboxDialog({
  open,
  onOpenChange,
  deviceList,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  deviceList: ReturnType<typeof useDevices>;
}) {
  const { organizationId } = useOrg();
  const { devices } = deviceList;
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const templates = useTemplates(organizationId, "picker", open);
  const snapshots = useSnapshots(organizationId, "picker", open);
  const readySnapshots = useMemo(
    () => snapshots.rows.filter((s) => s.state === SnapshotState.READY),
    [snapshots.rows],
  );
  const create = useMutation(SandboxService.method.createSandbox);
  const [attempt, setAttempt] = useState<CreateAttempt>();
  const sending = useRef(false);
  const [source, setSource] = useState(`base:${BaseImage.LINUX}`);
  const vcpuDraft = useRef<NumberInputDraft>(null);
  const memoryDraft = useRef<NumberInputDraft>(null);
  const diskDraft = useRef<NumberInputDraft>(null);
  const [vcpu, setVcpu] = useState<number | null>(2);
  const [memory, setMemory] = useState<number | null>(4096);
  const [disk, setDisk] = useState<number | null>(20);
  const [idleMinutes, setIdleMinutes] = useState<number | null>(60);
  const [maxHours, setMaxHours] = useState<number | null>(24);
  const [network, setNetwork] = useState(String(NetworkMode.OUTBOUND));
  const [device, setDevice] = useState(ALL);
  const [labelText, setLabelText] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const sourceRequest = sourceOf(source);
  const sourceOptions = [
    ...[BaseImage.LINUX, BaseImage.MACOS, BaseImage.WINDOWS].map((image) => ({
      value: `base:${image}`,
      label: `${labels.baseImage[image]} base image`,
    })),
    ...templates.rows.map((template) => ({
      value: `template:${template.templateId}`,
      label: `Template: ${template.name}`,
    })),
    ...readySnapshots.map((snapshot) => ({
      value: `snapshot:${snapshot.snapshotId}`,
      label: `Snapshot ${shortIdText(snapshot.snapshotId)} · ${formatTime(snapshot.createTime)}`,
    })),
  ];
  const selectedSource = sourceOptions.find(
    (option) => option.value === source,
  );
  const selectedDevice = devices.find((item) => item.deviceId === device);
  const deviceUnavailable = device !== ALL && !selectedDevice;
  const retryDeadline =
    attempt?.status === "unconfirmed" ? attempt.expiresAt : undefined;
  const expired = attempt?.status === "expired";
  const recovering = retryDeadline !== undefined || expired;
  const fieldsLocked = attempt !== undefined;

  useEffect(() => {
    if (retryDeadline === undefined) return;
    const timer = window.setTimeout(
      () => {
        setAttempt((current) =>
          current?.status === "unconfirmed" &&
          current.expiresAt === retryDeadline
            ? { ...current, status: "expired" }
            : current,
        );
      },
      Math.max(0, retryDeadline - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [retryDeadline]);

  const reset = () => {
    sending.current = false;
    setAttempt(undefined);
    setErrors({});
    setLabelText("");
  };

  return (
    <FormDialog
      open={open}
      title="Create a sandbox"
      wide
      description="Choose the source, resources and time limits. There is no default TTL."
      submitLabel={recovering ? "Retry original request" : "Create"}
      submitDisabled={
        attempt?.status === "pending" ||
        expired ||
        (!fieldsLocked && (!selectedSource || deviceUnavailable))
      }
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
      onSubmit={async (opening) => {
        if (sending.current) return;
        let submission = attempt;
        if (submission) {
          if (submission.status !== "unconfirmed") return;
          if (Date.now() >= submission.expiresAt) {
            setAttempt({ ...submission, status: "expired" });
            return;
          }
        } else {
          if (!selectedSource || deviceUnavailable) return;
          const next: Record<string, string> = {};
          const ttl = ttlErrors({ idleMinutes, maxHours });
          if (ttl.idle) next.idle = ttl.idle;
          if (ttl.max) next.max = ttl.max;
          Object.assign(
            next,
            resourceErrors({
              vcpu: vcpuDraft.current?.hasFraction() ? Number.NaN : vcpu,
              memory: memoryDraft.current?.hasFraction() ? Number.NaN : memory,
              disk: diskDraft.current?.hasFraction() ? Number.NaN : disk,
            }),
          );
          const parsed = parseLabels(labelText);
          if ("error" in parsed) next.labels = parsed.error;
          setErrors(next);
          if (Object.keys(next).length > 0 || "error" in parsed) return;

          // Start the retry window before dispatch. A lost response can
          // hide a reservation made with this key.
          submission = {
            status: "pending",
            expiresAt: Date.now() + CREATE_RESERVATION_MS,
            sourceLabel: selectedSource.label,
            deviceLabel: selectedDevice
              ? deviceOptionLabel(selectedDevice.name, selectedDevice.state)
              : undefined,
            request: {
              organizationId,
              idempotencyKey: uuidV7(),
              source: sourceRequest,
              resources: {
                vcpuCount: vcpu!,
                memoryMib: BigInt(memory!),
                diskGib: BigInt(disk!),
              },
              idleTtl: { seconds: BigInt(Math.round(idleMinutes! * 60)) },
              maxTtl: { seconds: BigInt(Math.round(maxHours! * 3600)) },
              placement: selectedDevice
                ? { deviceId: selectedDevice.deviceId }
                : undefined,
              network: { mode: Number(network) as NetworkMode },
              labels: parsed.labels,
            },
          };
          setAttempt(submission);
        }
        sending.current = true;
        let res;
        try {
          res = await create.mutateAsync(submission.request);
        } catch (error) {
          if (!opening.isCurrent()) return;
          if (submission.status === "pending" && isCreateRejection(error)) {
            setAttempt(undefined);
            throw error;
          }
          // An error on a replay cannot rule out a reservation from the
          // first request. Unavailable is also ambiguous on the first
          // request, even when the cached device state is offline.
        } finally {
          if (opening.isCurrent()) sending.current = false;
        }
        if (res) {
          // Refresh allocation and sandbox data in the background so
          // navigation can use the reply immediately.
          void invalidateServices(queryClient, DeviceService);
          void invalidateServices(queryClient, SandboxService);
        }
        // A request of a dismissed opening neither closes the form of a
        // later opening nor moves the user away from the page they went to.
        if (!opening.isCurrent()) return;
        const sandboxId = res?.sandbox?.sandboxId;
        if (!sandboxId || !SANDBOX_ID.test(sandboxId)) {
          setAttempt({
            ...submission,
            status:
              Date.now() >= submission.expiresAt ? "expired" : "unconfirmed",
          });
          return;
        }
        onOpenChange(false);
        reset();
        navigate(
          routePath({
            id: RouteId.Sandbox,
            organizationId,
            sandboxId,
          }),
        );
      }}
    >
      {!fieldsLocked &&
      (templates.boundary.isPending || snapshots.boundary.isPending) ? (
        // Render the source picker once its options are known, so the
        // trigger shows the selected option's name from the start.
        <Loading label="Loading sources" />
      ) : (
        <Select
          label="Source"
          value={selectedSource || fieldsLocked ? source : null}
          placeholder="Choose a source"
          disabled={fieldsLocked}
          onValueChange={(v) => v && setSource(v)}
        >
          {sourceOptions.map(({ value, label }) => (
            <SelectOption key={value} value={value}>
              {label}
            </SelectOption>
          ))}
          {!selectedSource && attempt ? (
            <SelectOption value={source}>{attempt.sourceLabel}</SelectOption>
          ) : null}
        </Select>
      )}
      {templates.boundary.isPending ? null : (
        <QueryBoundary query={templates.boundary} loading="Loading templates">
          {() => <MorePagesNotice noun="templates" pages={templates.pages} />}
        </QueryBoundary>
      )}
      {snapshots.boundary.isPending ? null : (
        <QueryBoundary query={snapshots.boundary} loading="Loading snapshots">
          {() => <MorePagesNotice noun="snapshots" pages={snapshots.pages} />}
        </QueryBoundary>
      )}
      <div className="field-row">
        <NumberInput
          label="vCPUs"
          value={vcpu}
          disabled={fieldsLocked}
          onValueChange={setVcpu}
          draftRef={vcpuDraft}
          min={1}
          error={errors.vcpu}
        />
        <NumberInput
          label="Memory (MiB)"
          value={memory}
          disabled={fieldsLocked}
          onValueChange={setMemory}
          draftRef={memoryDraft}
          min={256}
          // `step` is also the grid that Enter and blur snap the typed value
          // to, so 256 here would turn 1000 into 1,024. The arrow keys and
          // buttons move by 1 and Shift moves by 256.
          step={1}
          largeStep={256}
          error={errors.memory}
        />
        <NumberInput
          label="Disk (GiB)"
          value={disk}
          disabled={fieldsLocked}
          onValueChange={setDisk}
          draftRef={diskDraft}
          min={1}
          error={errors.disk}
        />
      </div>
      <div className="field-row">
        <NumberInput
          label="Idle TTL (minutes)"
          description="Ends the sandbox after this long without activity."
          value={idleMinutes}
          disabled={fieldsLocked}
          onValueChange={setIdleMinutes}
          min={1}
          error={errors.idle}
        />
        <NumberInput
          label="Maximum TTL (hours)"
          description="At most 168 hours (7 days)."
          value={maxHours}
          disabled={fieldsLocked}
          onValueChange={setMaxHours}
          min={1}
          max={168}
          error={errors.max}
        />
      </div>
      <div className="field-row">
        <Select
          label="Network"
          value={network}
          disabled={fieldsLocked}
          onValueChange={(v) => v && setNetwork(v)}
        >
          <SelectOption value={String(NetworkMode.OUTBOUND)}>
            Outbound access
          </SelectOption>
          <SelectOption value={String(NetworkMode.NONE)}>
            No network
          </SelectOption>
        </Select>
        <Select
          label="Device"
          value={deviceUnavailable && !fieldsLocked ? null : device}
          placeholder="Choose a device"
          error={
            deviceUnavailable && !fieldsLocked
              ? "The selected device is unavailable. Choose a device or select Any device that fits."
              : undefined
          }
          disabled={fieldsLocked}
          onValueChange={(v) => v && setDevice(v)}
        >
          <SelectOption value={ALL}>Any device that fits</SelectOption>
          {devices.map((d) => (
            <SelectOption key={d.deviceId} value={d.deviceId}>
              {deviceOptionLabel(d.name, d.state)}
            </SelectOption>
          ))}
          {!selectedDevice && attempt?.deviceLabel ? (
            <SelectOption value={device}>{attempt.deviceLabel}</SelectOption>
          ) : null}
        </Select>
      </div>
      {open ? <DeviceListNotice list={deviceList} /> : null}
      <Textarea
        label="Labels"
        description="One key=value per line. Every member and API key can read labels, so never put secrets in them."
        value={labelText}
        disabled={fieldsLocked}
        onValueChange={setLabelText}
        rows={3}
        error={errors.labels}
      />
      {recovering ? (
        <div className="notice notice-error" role="alert">
          <p>
            The sandbox may have been created.{" "}
            {expired
              ? "The retry window has ended. Check the sandbox list before creating another sandbox."
              : "Retry the original request to recover the sandbox."}
          </p>
        </div>
      ) : null}
    </FormDialog>
  );
}
