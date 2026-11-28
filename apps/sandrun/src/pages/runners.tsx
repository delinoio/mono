// SPDX-License-Identifier: Apache-2.0
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { BadgeVariant } from "delino-ui/badge";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import { RunnerState } from "@delino/sandrun-protos/v1/common_pb";
import { type Runner, WorkService } from "@delino/sandrun-protos/v1/work_pb";
import { useMemo, useState } from "react";
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
  formatTime,
  guestOsName,
  labels,
  TERMINAL_RUNNER_STATES,
} from "../format.ts";
import { useI18n } from "../i18n.ts";
import { can, Permission } from "../roles.ts";

const ALL = "all";

const STATES = [
  RunnerState.CREATING,
  RunnerState.PREPARING,
  RunnerState.WARM,
  RunnerState.WAITING_FOR_WORK,
  RunnerState.BUSY,
  RunnerState.DELETING,
  RunnerState.DELETED,
  RunnerState.FAILED,
];

function runnerTone(state: RunnerState): BadgeVariant {
  if (state === RunnerState.FAILED) return BadgeVariant.Danger;
  if (state === RunnerState.BUSY) return BadgeVariant.Accent;
  return BadgeVariant.Neutral;
}

/** Route `runners`: runners with profile, device, state and end reason. */
export function RunnersPage() {
  const { lang, t } = useI18n();
  const { organizationId, role } = useOrg();
  const queryClient = useQueryClient();
  const [state, setState] = useState<string>(ALL);
  const [profileId, setProfileId] = useState("");
  const [includeEnded, setIncludeEnded] = useState(false);
  const [deleting, setDeleting] = useState<Runner>();
  const { query: profiles, profileNames } =
    useWorkRunnerProfiles(organizationId);
  const effectiveProfileId = profileNames?.has(profileId) ? profileId : "";
  const devices = useQuery(WorkService.method.getCapacity, { organizationId });
  const deviceNames = useMemo(
    () =>
      new Map(
        (devices.data?.devices ?? []).map((d) => [d.ownsandDeviceId, d.name]),
      ),
    [devices.data],
  );
  const stateFilter = state === ALL ? [] : [Number(state) as RunnerState];
  const list = useInfiniteQuery(
    WorkService.method.listRunners,
    {
      organizationId,
      pageSize: PAGE_SIZE,
      pageToken: "",
      runnerProfileId: effectiveProfileId,
      states: stateFilter,
      includeEnded:
        includeEnded || stateFilter.some((s) => TERMINAL_RUNNER_STATES.has(s)),
    },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  );
  const remove = useMutation(WorkService.method.deleteRunner, {
    onSuccess: () => invalidateServices(queryClient, WorkService),
  });
  const admin = can(role, Permission.DeleteRunners);

  return (
    <>
      <PageHeader
        title={t("runners.title")}
        description={t("runners.description")}
      />
      <div className="filters">
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
              {t(labels.runnerState[s])}
            </SelectOption>
          ))}
        </Select>
        <RunnerProfileFilter
          profileNames={profileNames}
          value={effectiveProfileId}
          onValueChange={setProfileId}
        />
        <Checkbox
          label={t("runners.filter.showEnded")}
          checked={includeEnded}
          onCheckedChange={(checked) => setIncludeEnded(checked === true)}
        />
      </div>
      <RunnerProfileNotice query={profiles} />
      <QueryBoundary query={list} loading={t("runners.loading")}>
        {(data) => {
          const runners = data.pages.flatMap((p) => p.runners);
          return runners.length === 0 ? (
            <SetupEmpty
              organizationId={organizationId}
              title={t("runners.empty.title")}
            >
              {t("runners.empty.description")}
            </SetupEmpty>
          ) : (
            <div className="table-wrap">
              <table className="table" aria-label={t("runners.title")}>
                <thead>
                  <tr>
                    <th scope="col">{t("runners.column.runner")}</th>
                    <th scope="col">{t("common.column.profile")}</th>
                    <th scope="col">{t("common.column.kind")}</th>
                    <th scope="col">{t("common.column.device")}</th>
                    <th scope="col">{t("common.column.state")}</th>
                    <th scope="col">{t("common.column.endReason")}</th>
                    <th scope="col">{t("common.column.created")}</th>
                    {admin ? (
                      <th scope="col">{t("common.column.actions")}</th>
                    ) : null}
                  </tr>
                </thead>
                <tbody>
                  {runners.map((r) => {
                    const ended = TERMINAL_RUNNER_STATES.has(r.state);
                    return (
                      <tr key={r.runnerId}>
                        <td>
                          <ShortId id={r.runnerId} />
                        </td>
                        <td>
                          {profileNames?.get(r.runnerProfileId) ?? (
                            <ShortId id={r.runnerProfileId} />
                          )}
                        </td>
                        <td>
                          {t(labels.workloadKind[r.kind])} ·{" "}
                          {guestOsName(r.guestOs, t)}
                        </td>
                        <td>
                          {r.ownsandDeviceId === ""
                            ? "—"
                            : (deviceNames.get(r.ownsandDeviceId) ?? (
                                <ShortId id={r.ownsandDeviceId} />
                              ))}
                        </td>
                        <td>
                          <StateBadge
                            text={t(labels.runnerState[r.state])}
                            tone={runnerTone(r.state)}
                          />
                        </td>
                        <td className="wrap">
                          {!ended
                            ? "—"
                            : r.ownsandEndReason
                              ? t("runners.endReasonWithDetail", {
                                  reason: t(
                                    labels.runnerEndReason[r.endReason],
                                  ),
                                  detail: r.ownsandEndReason,
                                })
                              : t(labels.runnerEndReason[r.endReason])}
                        </td>
                        <td>{formatTime(r.createdAt, lang)}</td>
                        {admin ? (
                          <td>
                            {!ended && r.state !== RunnerState.DELETING ? (
                              <Button
                                variant={ButtonVariant.Danger}
                                size={ButtonSize.Small}
                                onClick={() => setDeleting(r)}
                              >
                                {t("common.delete")}
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
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={t("runners.deleteDialog.title")}
        description={t("runners.deleteDialog.description")}
        confirmLabel={t("runners.deleteDialog.confirm")}
        onConfirm={() =>
          remove.mutateAsync({ organizationId, runnerId: deleting!.runnerId })
        }
      />
    </>
  );
}
