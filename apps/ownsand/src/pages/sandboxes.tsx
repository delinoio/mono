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
  ListNoun,
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
  formatPlainNumber,
  formatTime,
  labels,
} from "../format.ts";
import { parseLabels, resourceErrors, ttlErrors } from "../forms.ts";
import { useI18n } from "../i18n.ts";
import { lastRequestFailedOnLaterPage } from "../pagination.ts";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";
import { useSnapshots, useTemplates } from "../snapshot-lists.ts";
import { enumText, type Text } from "../text.ts";
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
  const i18n = useI18n();
  const { t } = i18n;
  const [state, setState] = useState<string>(ALL);
  const [deviceFilter, setDeviceFilter] = useState<{
    id: string;
    name: string | undefined;
  } | null>(null);
  const [includeEnded, setIncludeEnded] = useState(false);
  const [creating, setCreating] = useState(false);
  const deviceList = useDevices(organizationId);
  const { devices } = deviceList;
  const deviceNames = useMemo(
    () => new Map(devices.map((d) => [d.deviceId, d.name])),
    [devices],
  );
  const deviceId = deviceFilter?.id;
  const deviceName =
    deviceId === undefined ? undefined : deviceNames.get(deviceId);
  // Keep the last name shown for the selected device across inventory refreshes.
  if (deviceFilter && deviceName && deviceName !== deviceFilter.name) {
    setDeviceFilter({ id: deviceFilter.id, name: deviceName });
  }
  const missingDeviceLabel =
    deviceFilter && deviceList.isComplete && !deviceNames.has(deviceFilter.id)
      ? deviceFilter.name || deviceFilter.id
      : undefined;
  const transport = useTransport();
  const listOptions = createInfiniteQueryOptions(
    SandboxService.method.listSandboxes,
    {
      organizationId,
      pageSize: 50,
      pageToken: "",
      state: state === ALL ? undefined : (Number(state) as SandboxState),
      deviceId,
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
        title={t("sandboxes.title")}
        description={t("sandboxes.description")}
        action={
          <Button onClick={() => setCreating(true)}>
            {t("sandboxes.create")}
          </Button>
        }
      />
      <div className="filters">
        <Select
          label={t("sandboxes.filters.state")}
          value={state}
          onValueChange={(v) => setState(v ?? ALL)}
        >
          <SelectOption value={ALL}>
            {t("sandboxes.filters.allStates")}
          </SelectOption>
          {Object.values(SandboxState)
            .filter((v): v is SandboxState => typeof v === "number" && v !== 0)
            .map((v) => (
              <SelectOption key={v} value={String(v)}>
                {enumText(i18n, labels.sandboxState, v)}
              </SelectOption>
            ))}
        </Select>
        <Select
          label={t("sandboxes.filters.device")}
          value={deviceId ?? ALL}
          onValueChange={(id) =>
            setDeviceFilter(
              id === null || id === ALL
                ? null
                : { id, name: deviceNames.get(id) },
            )
          }
        >
          <SelectOption value={ALL}>
            {t("sandboxes.filters.allDevices")}
          </SelectOption>
          {devices.map((d) => (
            <SelectOption key={d.deviceId} value={d.deviceId}>
              {d.name}
            </SelectOption>
          ))}
        </Select>
        <Checkbox
          label={t("sandboxes.filters.showEnded")}
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      <div className="muted device-filter-status" role="status">
        {missingDeviceLabel
          ? t("sandboxes.filters.deviceMissing", {
              device: missingDeviceLabel,
              allDevices: t("sandboxes.filters.allDevices"),
            })
          : null}
      </div>
      {creating ? null : <DeviceListNotice list={deviceList} />}
      <QueryBoundary query={boundaryQuery} loading={t("sandboxes.loading")}>
        {() => (
          <>
            {sandboxes.length === 0 ? (
              <Empty title={t("sandboxes.empty.title")}>
                {t("sandboxes.empty.description")}
              </Empty>
            ) : (
              <TableRegion label={t("sandboxes.table.label")}>
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">{t("sandboxes.table.sandbox")}</th>
                      <th scope="col">{t("sandboxes.table.state")}</th>
                      <th scope="col">{t("sandboxes.table.device")}</th>
                      <th scope="col">{t("sandboxes.table.resources")}</th>
                      <th scope="col">{t("sandboxes.table.idleDeadline")}</th>
                      <th scope="col">{t("sandboxes.table.maxDeadline")}</th>
                      <th scope="col">{t("sandboxes.table.labels")}</th>
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
                {t("common.showMore")}
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
  const { t } = useI18n();
  return (
    <QueryBoundary query={list.boundary} loading={t("devices.loading")}>
      {() => <MorePagesNotice noun={ListNoun.Devices} pages={list.pages} />}
    </QueryBoundary>
  );
}

/**
 * The resources of a sandbox in one line, for example
 * `2 vCPU · 4 GiB · 20 GiB disk`. `key` selects the wording; its
 * placeholders are `vcpu`, `memory` and `disk`.
 */
export function resourcesText(
  resources: Sandbox["resources"],
  text: Text,
  key:
    | "sandboxes.resourcesValue"
    | "sandbox.resourcesValue" = "sandboxes.resourcesValue",
): string {
  if (resources === undefined) return "—";
  return text.t(key, {
    vcpu: formatPlainNumber(resources.vcpuCount, text.lang),
    memory: formatMib(resources.memoryMib, text.lang),
    disk: formatPlainNumber(resources.diskGib, text.lang),
  });
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
  const i18n = useI18n();
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
      <td>{resourcesText(s.resources, i18n)}</td>
      <td>{ended ? "—" : formatTime(s.idleDeadline, i18n.lang)}</td>
      <td>{ended ? "—" : formatTime(s.maxDeadline, i18n.lang)}</td>
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
  const { t } = useI18n();
  const entries = Object.entries(values);
  if (entries.length === 0)
    return <span className="muted">{t("sandboxes.labels.none")}</span>;
  return (
    <ul className="labels" aria-label={t("sandboxes.labels.list")}>
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
  const i18n = useI18n();
  const { t } = i18n;
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
      label: t("sandboxes.source.baseImage", {
        image: enumText(i18n, labels.baseImage, image),
      }),
    })),
    ...templates.rows.map((template) => ({
      value: `template:${template.templateId}`,
      label: t("sandboxes.source.template", { name: template.name }),
    })),
    ...readySnapshots.map((snapshot) => ({
      value: `snapshot:${snapshot.snapshotId}`,
      label: t("sandboxes.source.snapshot", {
        id: shortIdText(snapshot.snapshotId),
        time: formatTime(snapshot.createTime, i18n.lang),
      }),
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
      title={t("sandboxes.createDialog.title")}
      wide
      description={t("sandboxes.createDialog.description")}
      submitLabel={
        recovering ? t("sandboxes.createDialog.retry") : t("common.create")
      }
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
          const ttl = ttlErrors({ idleMinutes, maxHours }, i18n);
          if (ttl.idle) next.idle = ttl.idle;
          if (ttl.max) next.max = ttl.max;
          Object.assign(
            next,
            resourceErrors(
              {
                vcpu: vcpuDraft.current?.hasFraction()
                  ? Number.NaN
                  : (vcpuDraft.current?.valueBelowMin() ?? vcpu),
                memory: memoryDraft.current?.hasFraction()
                  ? Number.NaN
                  : memory,
                disk: diskDraft.current?.hasFraction()
                  ? Number.NaN
                  : (diskDraft.current?.valueBelowMin() ?? disk),
              },
              i18n,
            ),
          );
          const parsed = parseLabels(labelText, i18n);
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
              ? deviceOptionLabel(
                  selectedDevice.name,
                  selectedDevice.state,
                  i18n,
                )
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
        <Loading label={t("sandboxes.createDialog.loadingSources")} />
      ) : (
        <Select
          label={t("sandboxes.createDialog.source")}
          value={selectedSource || fieldsLocked ? source : null}
          placeholder={t("sandboxes.createDialog.chooseSource")}
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
        <QueryBoundary
          query={templates.boundary}
          loading={t("templates.loading")}
        >
          {() => (
            <MorePagesNotice
              noun={ListNoun.Templates}
              pages={templates.pages}
            />
          )}
        </QueryBoundary>
      )}
      {snapshots.boundary.isPending ? null : (
        <QueryBoundary
          query={snapshots.boundary}
          loading={t("snapshots.loading")}
        >
          {() => (
            <MorePagesNotice
              noun={ListNoun.Snapshots}
              pages={snapshots.pages}
            />
          )}
        </QueryBoundary>
      )}
      <div className="field-row">
        <NumberInput
          label={t("sandboxes.createDialog.vcpus")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
          value={vcpu}
          disabled={fieldsLocked}
          onValueChange={setVcpu}
          draftRef={vcpuDraft}
          min={1}
          preserveBelowMin
          error={errors.vcpu}
        />
        <NumberInput
          label={t("sandboxes.createDialog.memory")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
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
          label={t("sandboxes.createDialog.disk")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
          value={disk}
          disabled={fieldsLocked}
          onValueChange={setDisk}
          draftRef={diskDraft}
          min={1}
          preserveBelowMin
          error={errors.disk}
        />
      </div>
      <div className="field-row">
        <NumberInput
          label={t("sandboxes.createDialog.idleTtl")}
          description={t("sandboxes.createDialog.idleTtlDescription")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
          value={idleMinutes}
          disabled={fieldsLocked}
          onValueChange={setIdleMinutes}
          min={1}
          error={errors.idle}
        />
        <NumberInput
          label={t("sandboxes.createDialog.maxTtl")}
          description={t("sandboxes.createDialog.maxTtlDescription")}
          decrementLabel={t("common.decrease")}
          incrementLabel={t("common.increase")}
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
          label={t("sandboxes.createDialog.network")}
          value={network}
          disabled={fieldsLocked}
          onValueChange={(v) => v && setNetwork(v)}
        >
          <SelectOption value={String(NetworkMode.OUTBOUND)}>
            {t("sandboxes.createDialog.networkOutbound")}
          </SelectOption>
          <SelectOption value={String(NetworkMode.NONE)}>
            {t("sandboxes.createDialog.networkNone")}
          </SelectOption>
        </Select>
        <Select
          label={t("sandboxes.createDialog.device")}
          value={deviceUnavailable && !fieldsLocked ? null : device}
          placeholder={t("sandboxes.createDialog.chooseDevice")}
          error={
            deviceUnavailable && !fieldsLocked
              ? t("sandboxes.createDialog.deviceUnavailable", {
                  anyDevice: t("sandboxes.createDialog.anyDevice"),
                })
              : undefined
          }
          disabled={fieldsLocked}
          onValueChange={(v) => v && setDevice(v)}
        >
          <SelectOption value={ALL}>
            {t("sandboxes.createDialog.anyDevice")}
          </SelectOption>
          {devices.map((d) => (
            <SelectOption key={d.deviceId} value={d.deviceId}>
              {deviceOptionLabel(d.name, d.state, i18n)}
            </SelectOption>
          ))}
          {!selectedDevice && attempt?.deviceLabel ? (
            <SelectOption value={device}>{attempt.deviceLabel}</SelectOption>
          ) : null}
        </Select>
      </div>
      {open ? <DeviceListNotice list={deviceList} /> : null}
      <Textarea
        label={t("sandboxes.createDialog.labels")}
        description={t("sandboxes.createDialog.labelsDescription")}
        value={labelText}
        disabled={fieldsLocked}
        onValueChange={setLabelText}
        rows={3}
        error={errors.labels}
      />
      {recovering ? (
        <div className="notice notice-error" role="alert">
          <p>
            {expired
              ? t("sandboxes.createDialog.unconfirmedExpired")
              : t("sandboxes.createDialog.unconfirmedRetry")}
          </p>
        </div>
      ) : null}
    </FormDialog>
  );
}
