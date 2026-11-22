// SPDX-License-Identifier: Apache-2.0
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { NumberInput } from "delino-ui/number-input";
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
  type Sandbox,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { useMemo, useState } from "react";
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
  PageHeader,
  SandboxStateBadge,
  ShortId,
} from "../components/ui.tsx";
import { formatMib, formatTime, labels } from "../format.ts";
import { parseLabels, resourceErrors, ttlErrors } from "../forms.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { uuidV7 } from "../uuid.ts";

const ALL = "all";

/** Route `sandboxes`: the sandbox list with filters and creation. */
export function SandboxesPage() {
  const { organizationId } = useOrg();
  const [state, setState] = useState<string>(ALL);
  const [deviceId, setDeviceId] = useState<string>(ALL);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [creating, setCreating] = useState(false);
  const devices = useQuery(DeviceService.method.listDevices, {
    organizationId,
    pageSize: 200,
  });
  const deviceNames = useMemo(
    () =>
      new Map((devices.data?.devices ?? []).map((d) => [d.deviceId, d.name])),
    [devices.data],
  );
  const list = useInfiniteQuery(
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
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
      refetchInterval: (q) =>
        q.state.data?.pages.some((p) => p.sandboxes.some(sandboxSettling))
          ? POLL_INTERVAL_MS
          : false,
    },
  );
  const sandboxes = list.data?.pages.flatMap((p) => p.sandboxes) ?? [];

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
          {(devices.data?.devices ?? []).map((d) => (
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
      {list.isPending ? (
        <Loading label="Loading sandboxes" />
      ) : list.error ? (
        <ErrorNotice error={list.error} onRetry={() => void list.refetch()} />
      ) : sandboxes.length === 0 ? (
        <Empty title="No sandboxes match">
          Create a sandbox, or change the filters.
        </Empty>
      ) : (
        <div className="table-wrap">
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
        </div>
      )}
      {list.hasNextPage ? (
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => void list.fetchNextPage()}
        >
          Show more
        </Button>
      ) : null}
      <CreateSandboxDialog open={creating} onOpenChange={setCreating} />
    </>
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
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { organizationId } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const templates = useQuery(
    SnapshotService.method.listTemplates,
    { organizationId, pageSize: 200 },
    { enabled: open },
  );
  const snapshots = useQuery(
    SnapshotService.method.listSnapshots,
    { organizationId, pageSize: 200 },
    { enabled: open },
  );
  const devices = useQuery(
    DeviceService.method.listDevices,
    { organizationId, pageSize: 200 },
    { enabled: open },
  );
  const create = useMutation(SandboxService.method.createSandbox);
  // One idempotency key per logical create: a retry after a lost response
  // reuses it, so the control plane returns the original sandbox.
  const [idempotencyKey, setIdempotencyKey] = useState(uuidV7);
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

  const reset = () => {
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
      submitLabel="Create"
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) reset();
      }}
      onSubmit={async () => {
        const next: Record<string, string> = {};
        const ttl = ttlErrors({ idleMinutes, maxHours });
        if (ttl.idle) next.idle = ttl.idle;
        if (ttl.max) next.max = ttl.max;
        Object.assign(next, resourceErrors({ vcpu, memory, disk }));
        const parsed = parseLabels(labelText);
        if ("error" in parsed) next.labels = parsed.error;
        setErrors(next);
        if (Object.keys(next).length > 0 || "error" in parsed) return;
        const res = await create.mutateAsync({
          organizationId,
          idempotencyKey,
          source: sourceOf(source),
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
        });
        await invalidateServices(queryClient, SandboxService);
        onOpenChange(false);
        reset();
        navigate(
          routePath({
            id: RouteId.Sandbox,
            organizationId,
            sandboxId: res.sandbox!.sandboxId,
          }),
        );
      }}
    >
      {templates.isPending || snapshots.isPending ? (
        // Render the source picker once its options are known, so the
        // trigger shows the selected option's name from the start.
        <Loading label="Loading sources" />
      ) : (
        <Select
          label="Source"
          value={source}
          onValueChange={(v) => v && setSource(v)}
        >
          {[BaseImage.LINUX, BaseImage.MACOS, BaseImage.WINDOWS].map((b) => (
            <SelectOption key={b} value={`base:${b}`}>
              {labels.baseImage[b]} base image
            </SelectOption>
          ))}
          {(templates.data?.templates ?? []).map((t) => (
            <SelectOption key={t.templateId} value={`template:${t.templateId}`}>
              Template: {t.name}
            </SelectOption>
          ))}
          {(snapshots.data?.snapshots ?? [])
            .filter((s) => s.state === SnapshotState.READY)
            .map((s) => (
              <SelectOption
                key={s.snapshotId}
                value={`snapshot:${s.snapshotId}`}
              >
                Snapshot {s.snapshotId.slice(0, 8)}
              </SelectOption>
            ))}
        </Select>
      )}
      <div className="field-row">
        <NumberInput
          label="vCPUs"
          value={vcpu}
          onValueChange={setVcpu}
          min={1}
          error={errors.vcpu}
        />
        <NumberInput
          label="Memory (MiB)"
          value={memory}
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
          onValueChange={setIdleMinutes}
          min={1}
          error={errors.idle}
        />
        <NumberInput
          label="Maximum TTL (hours)"
          description="At most 168 hours (7 days)."
          value={maxHours}
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
          onValueChange={(v) => setDevice(v ?? ALL)}
        >
          <SelectOption value={ALL}>Any device that fits</SelectOption>
          {(devices.data?.devices ?? []).map((d) => (
            <SelectOption key={d.deviceId} value={d.deviceId}>
              {d.name}
            </SelectOption>
          ))}
        </Select>
      </div>
      <Textarea
        label="Labels"
        description="One key=value per line. Every member and API key can read labels, so never put secrets in them."
        value={labelText}
        onValueChange={setLabelText}
        rows={3}
        error={errors.labels}
      />
    </FormDialog>
  );
}
