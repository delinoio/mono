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
import { NumberInput } from "delino-ui/number-input";
import { Select, SelectOption } from "delino-ui/select";
import { Textarea } from "delino-ui/textarea";
import {
  BaseImage,
  DeviceState,
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
import {
  deviceOptionLabel,
  formatMib,
  formatTime,
  labels,
  offlineDeviceMessage,
} from "../format.ts";
import { parseLabels, resourceErrors, ttlErrors } from "../forms.ts";
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
    refetchInterval: (q) =>
      q.state.data?.pages.some((p) => p.sandboxes.some(sandboxSettling))
        ? POLL_INTERVAL_MS
        : false,
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
        {() =>
          sandboxes.length === 0 ? (
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
          )
        }
      </QueryBoundary>
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
  // One idempotency key per logical create: a retry after a lost response
  // reuses it, so the control plane returns the original sandbox.
  const [idempotencyKey, setIdempotencyKey] = useState(uuidV7);
  const firstSubmittedAt = useRef<number>(undefined);
  const [attempt, setAttempt] = useState<CreateAttempt>();
  const [source, setSource] = useState(`base:${BaseImage.LINUX}`);
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
    firstSubmittedAt.current = undefined;
    setAttempt(undefined);
    setIdempotencyKey(uuidV7());
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
        (attempt === undefined && !selectedSource)
      }
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
      onSubmit={async (opening) => {
        let submission = attempt;
        if (submission) {
          if (submission.status !== "unconfirmed") return;
          if (Date.now() >= submission.expiresAt) {
            setAttempt({ ...submission, status: "expired" });
            return;
          }
        } else {
          if (!selectedSource) return;
          const next: Record<string, string> = {};
          const ttl = ttlErrors({ idleMinutes, maxHours });
          if (ttl.idle) next.idle = ttl.idle;
          if (ttl.max) next.max = ttl.max;
          Object.assign(next, resourceErrors({ vcpu, memory, disk }));
          const parsed = parseLabels(labelText);
          if ("error" in parsed) next.labels = parsed.error;
          setErrors(next);
          if (Object.keys(next).length > 0 || "error" in parsed) return;

          // A response can be lost after the server reserves the key. Start
          // the retry window with the first request that used this key.
          firstSubmittedAt.current ??= Date.now();
          submission = {
            status: "pending",
            expiresAt: firstSubmittedAt.current + CREATE_RESERVATION_MS,
            sourceLabel: selectedSource.label,
            deviceLabel: selectedDevice
              ? deviceOptionLabel(selectedDevice.name, selectedDevice.state)
              : undefined,
            request: {
              organizationId,
              idempotencyKey,
              source: sourceRequest,
              resources: {
                vcpuCount: vcpu!,
                memoryMib: BigInt(memory!),
                diskGib: BigInt(disk!),
              },
              idleTtl: { seconds: BigInt(Math.round(idleMinutes! * 60)) },
              maxTtl: { seconds: BigInt(Math.round(maxHours! * 3600)) },
              placement: device === ALL ? undefined : { deviceId: device },
              network: { mode: Number(network) as NetworkMode },
              labels: parsed.labels,
            },
          };
          setAttempt(submission);
        }
        let res;
        try {
          res = await create.mutateAsync(submission.request);
        } catch (error) {
          if (submission.status === "pending" && opening.isCurrent()) {
            setAttempt(undefined);
          }
          // The control plane answers `Unavailable` without a reason. When
          // the selected device is offline in the loaded device list, name
          // it, so the notice does not read as an outage of OwnSand.
          if (
            error instanceof ConnectError &&
            error.code === Code.Unavailable &&
            selectedDevice?.state === DeviceState.OFFLINE
          ) {
            throw new ConnectError(
              offlineDeviceMessage(selectedDevice.name),
              Code.FailedPrecondition,
              undefined,
              undefined,
              error,
            );
          }
          throw error;
        }
        // A new sandbox changes the allocation of its device. The device
        // and sandbox queries are marked stale and refetched in the
        // background. Navigation can use the response without waiting for
        // either list to refetch.
        void invalidateServices(queryClient, DeviceService);
        void invalidateServices(queryClient, SandboxService);
        // A request of a dismissed opening neither closes the form of a
        // later opening nor moves the user away from the page they went to.
        if (!opening.isCurrent()) return;
        const sandboxId = res.sandbox?.sandboxId;
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
          min={1}
          error={errors.vcpu}
        />
        <NumberInput
          label="Memory (MiB)"
          value={memory}
          disabled={fieldsLocked}
          onValueChange={setMemory}
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
          value={device}
          disabled={fieldsLocked}
          onValueChange={(v) => setDevice(v ?? ALL)}
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
