// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery, useMutation } from "@connectrpc/connect-query";
import { skipToken, useQuery, useQueryClient } from "@tanstack/react-query";
import { BadgeVariant } from "delino-ui/badge";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import {
  RequestState,
  WorkloadKind,
} from "@delino/sandrun-protos/v1/common_pb";
import { DevSessionService } from "@delino/sandrun-protos/v1/dev_session_pb";
import { type Request, WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { useState } from "react";
import { invalidateServices, PAGE_SIZE } from "../api.ts";
import { SetupEmpty } from "../components/setup.tsx";
import {
  RunnerProfileFilter,
  RunnerProfileNotice,
  useWorkRunnerProfiles,
} from "../components/work-runner-profiles.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  ListPagination,
  PageHeader,
  QueryBoundary,
  ShortId,
  StateBadge,
} from "../components/ui.tsx";
import {
  formatSpan,
  formatTime,
  labels,
  queueWaitMs,
  TERMINAL_REQUEST_STATES,
} from "../format.ts";
import { useI18n } from "../i18n.ts";
import { can, Permission } from "../roles.ts";
import { useQueueWaitClock } from "../use-queue-wait-clock.ts";

const ALL = "all";

const KINDS = [
  WorkloadKind.GITHUB_JOB,
  WorkloadKind.AGENT_SESSION,
  WorkloadKind.DEV_SESSION,
];

const STATES = [
  RequestState.QUEUED,
  RequestState.STARTING,
  RequestState.RUNNING,
  RequestState.ENDED,
  RequestState.CANCELED,
  RequestState.EXPIRED,
  RequestState.FAILED,
];

export function requestStateTone(state: RequestState): BadgeVariant {
  if (state === RequestState.FAILED || state === RequestState.EXPIRED)
    return BadgeVariant.Danger;
  if (state === RequestState.RUNNING) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

/** The source of a request, for display. */
export function requestSource(r: Request): string {
  if (r.githubJob !== undefined) {
    return `${r.githubJob.repositoryFullName} · ${r.githubJob.jobName}`;
  }
  return r.sourceKey;
}

/** Route `requests`: requests with filters; cancel for `admin`. */
export function RequestsPage() {
  const i18n = useI18n();
  const { lang, t } = i18n;
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const [kind, setKind] = useState<string>(ALL);
  const [state, setState] = useState<string>(ALL);
  const [profileId, setProfileId] = useState("");
  const [includeEnded, setIncludeEnded] = useState(false);
  const [canceling, setCanceling] = useState<Request>();
  const [showCancellationRejection, setShowCancellationRejection] =
    useState(false);
  // Keep rejections with the query cache so they also cover stale filtered
  // lists and cached rows shown after navigating back to Requests.
  const rejectedCancellationsKey = [
    "rejectedRequestCancellations",
    organizationId,
  ];
  const { data: rejectedAttempts } = useQuery<ReadonlyMap<string, number>>({
    queryKey: rejectedCancellationsKey,
    queryFn: skipToken,
  });
  const isCancellationRejected = (request: Request) =>
    request.startAttempts <= (rejectedAttempts?.get(request.requestId) ?? -1);
  const { query: profiles, profileNames } =
    useWorkRunnerProfiles(organizationId);
  const effectiveProfileId = profileNames?.has(profileId) ? profileId : "";
  const stateFilter = state === ALL ? [] : [Number(state) as RequestState];
  const list = useInfiniteQuery(
    WorkService.method.listRequests,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      kind:
        kind === ALL
          ? WorkloadKind.UNSPECIFIED
          : (Number(kind) as WorkloadKind),
      runnerProfileId: effectiveProfileId,
      states: stateFilter,
      includeEnded:
        includeEnded || stateFilter.some((s) => TERMINAL_REQUEST_STATES.has(s)),
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const cancel = useMutation(WorkService.method.cancelRequest, {
    onSuccess: () =>
      invalidateServices(queryClient, WorkService, DevSessionService),
  });
  const admin = can(role, Permission.CancelRequests);
  const now = useQueueWaitClock();

  return (
    <>
      <PageHeader
        title={t("requests.title")}
        description={t("requests.description")}
      />
      <div className="filters">
        <Select
          label={t("common.column.kind")}
          value={kind}
          onValueChange={(v) => setKind(v ?? ALL)}
        >
          <SelectOption value={ALL}>
            {t("requests.filter.allKinds")}
          </SelectOption>
          {KINDS.map((k) => (
            <SelectOption key={k} value={String(k)}>
              {t(labels.workloadKind[k])}
            </SelectOption>
          ))}
        </Select>
        <Select
          label={t("common.column.state")}
          value={state}
          onValueChange={(v) => setState(v ?? ALL)}
        >
          <SelectOption value={ALL}>
            {t("common.filter.allStates")}
          </SelectOption>
          {STATES.map((s) => (
            <SelectOption key={s} value={String(s)}>
              {t(labels.requestState[s])}
            </SelectOption>
          ))}
        </Select>
        <RunnerProfileFilter
          profileNames={profileNames}
          value={effectiveProfileId}
          onValueChange={setProfileId}
        />
        <Checkbox
          label={t("requests.filter.showEnded")}
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      {showCancellationRejection ? (
        <div className="notice" role="alert">
          <p>{t("requests.cancellationRejected")}</p>
        </div>
      ) : null}
      <RunnerProfileNotice query={profiles} />
      <QueryBoundary query={list} loading={t("requests.loading")}>
        {(data) => {
          const requests = new Map<string, Request>();
          // A change in queue order can repeat a request at a page boundary.
          for (const page of data.pages) {
            for (const request of page.requests) {
              if (!requests.has(request.requestId)) {
                requests.set(request.requestId, request);
              }
            }
          }
          return requests.size === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title={t("requests.empty.title")}
            >
              {t("requests.empty.description")}
            </SetupEmpty>
          ) : (
            <div
              className="table-wrap"
              role="region"
              aria-label={t("requests.tableRegion")}
              tabIndex={0}
            >
              <table className="table" aria-label={t("requests.title")}>
                <thead>
                  <tr>
                    <th scope="col">{t("common.column.request")}</th>
                    <th scope="col">{t("common.column.kind")}</th>
                    <th scope="col">{t("common.column.profile")}</th>
                    <th scope="col">{t("requests.column.source")}</th>
                    <th scope="col">{t("common.column.state")}</th>
                    <th scope="col">{t("common.column.endReason")}</th>
                    <th scope="col">{t("common.column.priority")}</th>
                    <th scope="col">{t("requests.column.queueWait")}</th>
                    <th scope="col">{t("common.column.queued")}</th>
                    {admin ? (
                      <th scope="col">{t("common.column.actions")}</th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {Array.from(requests.values(), (r) => {
                    const wait = queueWaitMs(r, now);
                    return (
                      <tr key={r.requestId}>
                        <td>
                          <ShortId id={r.requestId} />
                        </td>
                        <td>{t(labels.workloadKind[r.kind])}</td>
                        <td>
                          {profileNames?.get(r.runnerProfileId) ?? (
                            <ShortId id={r.runnerProfileId} />
                          )}
                        </td>
                        <td className="wrap">{requestSource(r) || "—"}</td>
                        <td>
                          <StateBadge
                            text={t(labels.requestState[r.state])}
                            tone={requestStateTone(r.state)}
                          />
                        </td>
                        <td>
                          {TERMINAL_REQUEST_STATES.has(r.state)
                            ? t(labels.requestEndReason[r.endReason])
                            : "—"}
                        </td>
                        <td>{t(labels.priority[r.priority])}</td>
                        <td>
                          {wait === undefined ? "—" : formatSpan(wait, i18n)}
                        </td>
                        <td>{formatTime(r.queuedAt, lang)}</td>
                        {admin ? (
                          <td>
                            {r.state === RequestState.QUEUED &&
                            !isCancellationRejected(r) ? (
                              <Button
                                variant={ButtonVariant.Danger}
                                size={ButtonSize.Small}
                                onClick={() => {
                                  setShowCancellationRejection(false);
                                  setCanceling(r);
                                }}
                              >
                                {t("common.cancel")}
                              </Button>
                            ) : null}
                          </td>
                        ) : null}
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
      <ConfirmDialog
        open={canceling !== undefined}
        onOpenChange={(open) => !open && setCanceling(undefined)}
        title={t("requests.cancelDialog.title")}
        description={t("requests.cancelDialog.description")}
        confirmLabel={t("requests.cancelDialog.confirm")}
        confirmDisabled={
          canceling !== undefined && isCancellationRejected(canceling)
        }
        onConfirm={async (opening) => {
          if (canceling === undefined) return;
          const { requestId, startAttempts } = canceling;
          try {
            await cancel.mutateAsync({ organizationId, requestId });
          } catch (error) {
            if (
              !(error instanceof ConnectError) ||
              error.code !== Code.FailedPrecondition
            ) {
              throw error;
            }
            // A request can return to the queue after a failed runner start.
            // A higher start-attempt count identifies that new queue entry.
            queryClient.setQueryData<ReadonlyMap<string, number>>(
              rejectedCancellationsKey,
              (previous) =>
                new Map(previous).set(
                  requestId,
                  Math.max(previous?.get(requestId) ?? -1, startAttempts),
                ),
            );
            if (opening.isCurrent()) {
              setShowCancellationRejection(true);
              setCanceling(undefined);
            }
            await invalidateServices(queryClient, WorkService);
          }
        }}
      />
    </>
  );
}
