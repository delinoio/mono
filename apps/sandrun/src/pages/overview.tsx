// SPDX-License-Identifier: Apache-2.0
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { BadgeVariant } from "delino-ui/badge";
import {
  DeviceState,
  OwnSandLinkState,
  RunnerState,
} from "@delino/sandrun-protos/v1/common_pb";
import { OwnSandLinkService } from "@delino/sandrun-protos/v1/ownsand_link_pb";
import {
  type DeviceCapacity,
  WorkService,
} from "@delino/sandrun-protos/v1/work_pb";
import { useEffect, useReducer, useState } from "react";
import { watchQueue } from "sandrun-sdk";
import { SetupGuide } from "../components/setup.tsx";
import {
  RunnerProfileNotice,
  useWorkRunnerProfiles,
} from "../components/work-runner-profiles.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  Loading,
  PageHeader,
  QueryBoundary,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import { ownsandDevicesUrl } from "../config.ts";
import { formatSpan, labels, queueWaitMs } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import {
  applyQueueEvent,
  initialQueueView,
  LiveState,
  QueueViewActionKind,
  waitingInQueueOrder,
} from "../queue.ts";
import { Link, orgPath, RouteId } from "../router.tsx";
import { useQueueWaitClock } from "../use-queue-wait-clock.ts";

const RUNNER_ORDER = [
  RunnerState.CREATING,
  RunnerState.PREPARING,
  RunnerState.WARM,
  RunnerState.WAITING_FOR_WORK,
  RunnerState.BUSY,
  RunnerState.DELETING,
];

const LIVE_TEXT = {
  [LiveState.Connecting]: "overview.live.connecting",
  [LiveState.Live]: "overview.live.live",
  [LiveState.Reconnecting]: "overview.live.reconnecting",
  [LiveState.Disconnected]: "overview.live.disconnected",
} as const satisfies Record<LiveState, MessageKey>;

/**
 * Route `overview`: the live queue from `WatchQueue` (reconnects with the
 * SDK helper), runners by state and the device pool from `GetCapacity`.
 */
export function OverviewPage() {
  const i18n = useI18n();
  const { t, plural } = i18n;
  const { organizationId } = useOrg();
  const transport = useTransport();
  const [view, dispatch] = useReducer(
    applyQueueEvent,
    undefined,
    initialQueueView,
  );
  const [streamError, setStreamError] = useState<unknown>();
  const [attempt, setAttempt] = useState(0);
  const now = useQueueWaitClock();
  useEffect(() => {
    const abort = new AbortController();
    const work = createClient(WorkService, transport);
    // A new iterator sends no initial gap, so start a new snapshot here.
    // Otherwise a retry merges its snapshot into the old state and keeps
    // requests and runners that ended while no stream was open.
    dispatch({ kind: QueueViewActionKind.Restart });
    void (async () => {
      try {
        for await (const event of watchQueue(work, {
          organizationId,
          signal: abort.signal,
        })) {
          dispatch(event);
        }
      } catch (error) {
        if (!abort.signal.aborted) {
          dispatch({ kind: QueueViewActionKind.StreamFailed });
          setStreamError(error);
        }
      }
    })();
    return () => abort.abort();
  }, [transport, organizationId, attempt]);

  const { query: profiles, profileNames } =
    useWorkRunnerProfiles(organizationId);
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });

  const waiting = waitingInQueueOrder(view.requests.values());
  const running = [...view.requests.values()].filter(
    (r) => !waiting.includes(r),
  );
  const runnersByState = RUNNER_ORDER.map((state) => ({
    state,
    runners: [...view.runners.values()].filter((r) => r.state === state),
  }));

  return (
    <>
      <PageHeader
        title={t("overview.title")}
        description={t("overview.description")}
        action={
          <span
            className="live"
            role="status"
            aria-label={t("overview.live.label")}
          >
            <StateBadge
              text={t(LIVE_TEXT[view.live])}
              tone={
                view.live === LiveState.Live
                  ? BadgeVariant.Accent
                  : BadgeVariant.Neutral
              }
            />
          </span>
        }
      />
      <SetupGuide organizationId={organizationId} />
      {streamError !== undefined ? (
        <ErrorNotice
          error={streamError}
          onRetry={() => {
            setStreamError(undefined);
            setAttempt((n) => n + 1);
          }}
        />
      ) : null}
      <section className="section" aria-labelledby="queue-title">
        <h2 id="queue-title">{t("overview.queue.title")}</h2>
        <RunnerProfileNotice query={profiles} />
        {view.live === LiveState.Connecting ? (
          <Loading label={t("overview.queue.loading")} />
        ) : !view.hasSnapshot ? null : waiting.length === 0 ? (
          <Empty title={t("overview.queue.empty.title")}>
            {t("overview.queue.empty.description")}
          </Empty>
        ) : (
          <div
            className="table-wrap"
            role="region"
            aria-label={t("overview.queue.tableRegion")}
            tabIndex={0}
          >
            <table className="table" aria-label={t("overview.queue.title")}>
              <thead>
                <tr>
                  <th scope="col">{t("overview.queue.column.position")}</th>
                  <th scope="col">{t("common.column.request")}</th>
                  <th scope="col">{t("common.column.kind")}</th>
                  <th scope="col">{t("common.column.profile")}</th>
                  <th scope="col">{t("common.column.priority")}</th>
                  <th scope="col">{t("overview.queue.column.waiting")}</th>
                </tr>
              </thead>
              <tbody>
                {waiting.map((r, index) => (
                  <tr key={r.requestId}>
                    <td>{index + 1}</td>
                    <td>
                      <ShortId id={r.requestId} />
                    </td>
                    <td>{t(labels.workloadKind[r.kind])}</td>
                    <td>
                      {profileNames?.get(r.runnerProfileId) ?? (
                        <ShortId id={r.runnerProfileId} />
                      )}
                    </td>
                    <td>{t(labels.priority[r.priority])}</td>
                    <td>{formatSpan(queueWaitMs(r, now) ?? 0, i18n)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {view.hasSnapshot && running.length > 0 ? (
          <p className="muted">
            {plural("overview.queue.running", running.length)}
          </p>
        ) : null}
      </section>
      <section className="section" aria-labelledby="runners-title">
        <h2 id="runners-title">{t("overview.runners.title")}</h2>
        {view.live === LiveState.Connecting ? (
          <Loading label={t("overview.runners.loading")} />
        ) : view.hasSnapshot ? (
          <dl className="details details-flat">
            {runnersByState.map(({ state, runners }) => (
              <div className="detail" key={state}>
                <dt>{t(labels.runnerState[state])}</dt>
                <dd>{runners.length}</dd>
              </div>
            ))}
          </dl>
        ) : null}
      </section>
      <section className="section" aria-labelledby="devices-title">
        <h2 id="devices-title">{t("overview.devices.title")}</h2>
        <p className="muted">
          {t("overview.devices.description")}
          {link.data?.link !== undefined ? (
            <>
              {" "}
              <a
                href={ownsandDevicesUrl(link.data.link.ownsandOrganizationId)}
                target="_blank"
                rel="noreferrer"
              >
                {t("overview.devices.manage")}
              </a>
            </>
          ) : null}
        </p>
        <QueryBoundary
          query={link}
          loading={t("overview.devices.loading")}
          errorTitle={t("overview.devices.linkLoadFailed")}
        >
          {({ link: ownSandLink }) =>
            ownSandLink?.state === OwnSandLinkState.ACTIVE ? (
              <DeviceCapacityTable organizationId={organizationId} />
            ) : (
              <Empty title={t("overview.devices.linkRequired.title")}>
                {ownSandLink === undefined
                  ? t("overview.devices.linkRequired.missing")
                  : t("overview.devices.linkRequired.invalid")}{" "}
                <Link to={orgPath(RouteId.Integrations, organizationId)}>
                  {t("common.goToIntegrations")}
                </Link>
              </Empty>
            )
          }
        </QueryBoundary>
      </section>
    </>
  );
}

function DeviceCapacityTable({ organizationId }: { organizationId: string }) {
  const { t } = useI18n();
  const capacity = useQuery(WorkService.method.getCapacity, { organizationId });
  return (
    <QueryBoundary query={capacity} loading={t("overview.devices.loading")}>
      {(data) =>
        data.devices.length === 0 ? (
          <Empty title={t("overview.devices.empty.title")}>
            {t("overview.devices.empty.description")}
          </Empty>
        ) : (
          <div
            className="table-wrap"
            role="region"
            aria-label={t("overview.devices.tableRegion")}
            tabIndex={0}
          >
            <table className="table" aria-label={t("overview.devices.table")}>
              <thead>
                <tr>
                  <th scope="col">{t("common.column.device")}</th>
                  <th scope="col">{t("common.column.state")}</th>
                  <th scope="col">{t("overview.devices.column.platform")}</th>
                  <th scope="col">{t("overview.devices.column.vcpus")}</th>
                  <th scope="col">{t("overview.devices.column.memory")}</th>
                  <th scope="col">{t("overview.devices.column.disk")}</th>
                  <th scope="col">{t("overview.devices.column.runners")}</th>
                </tr>
              </thead>
              <tbody>
                {data.devices.map((d) => (
                  <DeviceRow key={d.ownsandDeviceId} device={d} />
                ))}
              </tbody>
            </table>
          </div>
        )
      }
    </QueryBoundary>
  );
}

function DeviceRow({ device: d }: { device: DeviceCapacity }) {
  const { lang, t } = useI18n();
  const runners = d.runners.reduce((sum, r) => sum + r.count, 0);
  return (
    <tr>
      <td>{d.name || <ShortId id={d.ownsandDeviceId} />}</td>
      <td>
        <StateBadge
          text={t(labels.deviceState[d.state])}
          tone={
            d.state === DeviceState.ONLINE
              ? BadgeVariant.Accent
              : BadgeVariant.Neutral
          }
        />
      </td>
      <td>
        {d.hostOs} · {d.arch}
      </td>
      <td>
        {t("overview.devices.vcpuUsage", {
          used: String(d.allocation?.vcpuCount ?? 0n),
          limit: String(d.effectiveVcpuLimit),
        })}
        {d.limits !== undefined && d.limits.vcpuOvercommitPercent !== 100 ? (
          <span className="muted">
            {" "}
            {t("overview.devices.overcommit", {
              vcpus: d.limits.vcpuCount,
              percent: d.limits.vcpuOvercommitPercent,
            })}
          </span>
        ) : null}
      </td>
      <td>
        {t("overview.devices.memoryUsage", {
          used: String(d.allocation?.memoryMib ?? 0n),
          limit: String(d.limits?.memoryMib ?? 0n),
        })}
      </td>
      <td>
        {t("overview.devices.diskUsage", {
          used: String(d.allocation?.diskGib ?? 0n),
          limit: String(d.limits?.diskGib ?? 0n),
        })}
      </td>
      <td>
        {runners}
        {d.runners.length > 0 ? (
          <span className="muted">
            {" "}
            {t("overview.devices.runnerBreakdown", {
              runners: new Intl.ListFormat(lang, {
                type: "unit",
                style: "short",
              }).format(
                d.runners
                  .filter((r) => r.count > 0)
                  .map((r) =>
                    t("overview.devices.runnerCount", {
                      state: t(labels.runnerState[r.state]),
                      count: r.count,
                    }),
                  ),
              ),
            })}
          </span>
        ) : null}
      </td>
    </tr>
  );
}
