// SPDX-License-Identifier: Apache-2.0
import { useMutation, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import {
  SnapshotExportState,
  SnapshotState,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  type Snapshot,
  SnapshotService,
  type Template,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  ListNoun,
  PageHeader,
  QueryBoundary,
  ShortId,
  isTerminalError,
  shortIdText,
  toastError,
} from "../components/ui.tsx";
import { formatBytes, formatTime, labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { cancelUncachedList } from "../pagination.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import {
  templateListOptions,
  useSnapshots,
  useTemplates,
} from "../snapshot-lists.ts";
import { enumText, type MessageKey } from "../text.ts";

/** Route `snapshots`: snapshots, their export, and templates. */
export function SnapshotsPage() {
  const { organizationId } = useOrg();
  const i18n = useI18n();
  const { t, lang } = i18n;
  const queryClient = useQueryClient();
  const transport = useTransport();
  const refresh = () => invalidateServices(queryClient, SnapshotService);
  const snapshots = useSnapshots(organizationId, "management");
  const templates = useTemplates(organizationId, "management");
  const exportSnapshot = useMutation(SnapshotService.method.exportSnapshot, {
    onSuccess: refresh,
  });
  const deleteSnapshot = useMutation(SnapshotService.method.deleteSnapshot, {
    onSuccess: refresh,
  });
  const createTemplate = useMutation(SnapshotService.method.createTemplate, {
    onSuccess: async (_, { organizationId: submittedOrganizationId }) => {
      if (submittedOrganizationId !== undefined) {
        await cancelUncachedList(
          queryClient,
          templateListOptions(transport, submittedOrganizationId, "management")
            .queryKey,
        );
      }
      return refresh();
    },
  });
  const deleteTemplate = useMutation(SnapshotService.method.deleteTemplate, {
    onSuccess: refresh,
  });
  const [templateFrom, setTemplateFrom] = useState<{
    snapshotId: string;
    unavailable: boolean;
  }>();
  const [templateName, setTemplateName] = useState("");
  const [deletingSnapshot, setDeletingSnapshot] = useState<Snapshot>();
  const [deletingTemplate, setDeletingTemplate] = useState<Template>();

  const terminalSnapshotError = isTerminalError(snapshots.boundary.error);
  const sourceSnapshot = templateFrom
    ? snapshots.rows.find((row) => row.snapshotId === templateFrom.snapshotId)
    : undefined;
  // A missing row only confirms unavailability after a complete read.
  const sourceUnavailable = sourceSnapshot
    ? sourceSnapshot.state !== SnapshotState.READY
    : snapshots.isComplete || templateFrom?.unavailable === true;

  // An open action belongs to its list. Discard it when access to that list ends.
  if (terminalSnapshotError) {
    if (templateFrom) {
      setTemplateFrom(undefined);
      setTemplateName("");
    }
    if (deletingSnapshot) setDeletingSnapshot(undefined);
  } else if (templateFrom && templateFrom.unavailable !== sourceUnavailable) {
    setTemplateFrom({ ...templateFrom, unavailable: sourceUnavailable });
  }
  if (deletingTemplate && isTerminalError(templates.boundary.error)) {
    setDeletingTemplate(undefined);
  }

  return (
    <>
      <PageHeader
        title={t("snapshots.title")}
        description={t("snapshots.description")}
      />
      <QueryBoundary
        query={snapshots.boundary}
        loading={t("snapshots.loading")}
      >
        {() =>
          snapshots.rows.length === 0 ? (
            <Empty title={t("snapshots.empty.title")}>
              {t("snapshots.empty.description")}
            </Empty>
          ) : (
            <ul className="list" aria-label={t("snapshots.list")}>
              {snapshots.rows.map((s) => {
                const exportable =
                  s.state === SnapshotState.READY &&
                  (s.exportState === SnapshotExportState.NOT_EXPORTED ||
                    s.exportState === SnapshotExportState.EXPORT_FAILED);
                return (
                  <li key={s.snapshotId} className="list-row snapshot-row">
                    <div className="list-main">
                      <span className="list-title">
                        {i18n.rich("snapshots.rowTitle", {
                          id: <ShortId id={s.snapshotId} />,
                          state: enumText(i18n, labels.snapshotState, s.state),
                          exportState: enumText(
                            i18n,
                            labels.exportState,
                            s.exportState,
                          ),
                        })}
                      </span>
                      <span className="muted">
                        {i18n.rich(
                          s.hasMemoryState
                            ? "snapshots.rowDetailsWithMemory"
                            : "snapshots.rowDetails",
                          {
                            os: enumText(i18n, labels.guestOs, s.guestOs),
                            size: formatBytes(s.sizeBytes, lang),
                            sandbox: (
                              <Link
                                to={routePath({
                                  id: RouteId.Sandbox,
                                  organizationId,
                                  sandboxId: s.sourceSandboxId,
                                })}
                              >
                                <ShortId id={s.sourceSandboxId} />
                              </Link>
                            ),
                            time: formatTime(s.createTime, lang),
                          },
                        )}
                      </span>
                    </div>
                    <div className="list-actions">
                      {exportable ? (
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          aria-label={t("snapshots.exportNamed", {
                            id: shortIdText(s.snapshotId),
                          })}
                          onClick={async () => {
                            try {
                              await exportSnapshot.mutateAsync({
                                organizationId,
                                snapshotId: s.snapshotId,
                              });
                              toast.success(t("snapshots.exportStarted"));
                            } catch (error) {
                              toastError(error, i18n);
                            }
                          }}
                        >
                          {t("snapshots.export")}
                        </Button>
                      ) : null}
                      {s.state === SnapshotState.READY ? (
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          aria-label={t("snapshots.makeTemplateNamed", {
                            id: shortIdText(s.snapshotId),
                          })}
                          onClick={() =>
                            setTemplateFrom({
                              snapshotId: s.snapshotId,
                              unavailable: false,
                            })
                          }
                        >
                          {t("snapshots.makeTemplate")}
                        </Button>
                      ) : null}
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        aria-label={t("snapshots.deleteNamed", {
                          id: shortIdText(s.snapshotId),
                        })}
                        onClick={() => setDeletingSnapshot(s)}
                      >
                        {t("common.delete")}
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ul>
          )
        }
      </QueryBoundary>
      <ShowMore noun={ListNoun.Snapshots} list={snapshots} />
      <section className="section" aria-labelledby="templates-title">
        <h2 id="templates-title">{t("templates.title")}</h2>
        <QueryBoundary
          query={templates.boundary}
          loading={t("templates.loading")}
        >
          {() =>
            templates.rows.length === 0 ? (
              <Empty title={t("templates.empty.title")}>
                {t("templates.empty.description")}
              </Empty>
            ) : (
              <ul className="list" aria-label={t("templates.list")}>
                {templates.rows.map((template) => (
                  <li key={template.templateId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">{template.name}</span>
                      <span className="muted">
                        {i18n.rich("templates.rowDetails", {
                          snapshot: <ShortId id={template.snapshotId} />,
                          time: formatTime(template.createTime, lang),
                        })}
                      </span>
                    </div>
                    <Button
                      variant={ButtonVariant.Danger}
                      size={ButtonSize.Small}
                      aria-label={t("templates.deleteNamed", {
                        name: template.name,
                        id: shortIdText(template.snapshotId),
                      })}
                      onClick={() => setDeletingTemplate(template)}
                    >
                      {t("common.delete")}
                    </Button>
                  </li>
                ))}
              </ul>
            )
          }
        </QueryBoundary>
        <ShowMore noun={ListNoun.Templates} list={templates} />
      </section>
      <FormDialog
        open={templateFrom !== undefined}
        onOpenChange={(open) => {
          if (!open) {
            setTemplateFrom(undefined);
            setTemplateName("");
          }
        }}
        title={t("templates.createDialog.title")}
        description={
          sourceUnavailable ? (
            <span role="alert">
              {t("templates.createDialog.sourceUnavailable")}
            </span>
          ) : undefined
        }
        submitLabel={t("templates.createDialog.submit")}
        submitDisabled={sourceUnavailable}
        onSubmit={async (opening) => {
          if (!templateFrom || sourceUnavailable) return;
          await createTemplate.mutateAsync({
            organizationId,
            snapshotId: templateFrom.snapshotId,
            name: templateName.trim(),
          });
          // The list refreshes either way (`onSuccess`). A request of a
          // dismissed opening does not close the form of a later opening.
          if (!opening.isCurrent()) return;
          setTemplateFrom(undefined);
          setTemplateName("");
        }}
      >
        <TextField
          label={t("templates.createDialog.name")}
          value={templateName}
          onValueChange={setTemplateName}
          autoFocus
          required
          maxLength={100}
        />
      </FormDialog>
      <ConfirmDialog
        open={deletingSnapshot !== undefined}
        onOpenChange={(open) => !open && setDeletingSnapshot(undefined)}
        title={t("snapshots.deleteDialog.title", {
          id: deletingSnapshot ? shortIdText(deletingSnapshot.snapshotId) : "",
        })}
        description={t("snapshots.deleteDialog.description")}
        confirmLabel={t("snapshots.deleteDialog.confirm")}
        onConfirm={() =>
          deleteSnapshot.mutateAsync({
            organizationId,
            snapshotId: deletingSnapshot!.snapshotId,
          })
        }
      />
      <ConfirmDialog
        open={deletingTemplate !== undefined}
        onOpenChange={(open) => !open && setDeletingTemplate(undefined)}
        title={t("templates.deleteDialog.title", {
          name: deletingTemplate?.name ?? "",
        })}
        description={t("templates.deleteDialog.description")}
        confirmLabel={t("templates.deleteDialog.confirm")}
        onConfirm={() =>
          deleteTemplate.mutateAsync({
            organizationId,
            templateId: deletingTemplate!.templateId,
          })
        }
      />
    </>
  );
}

/** The accessible names of the Show more buttons of the two lists. */
const SHOW_MORE_LABELS = {
  [ListNoun.Snapshots]: "snapshots.showMore",
  [ListNoun.Templates]: "templates.showMore",
} as const satisfies Partial<Record<ListNoun, MessageKey>>;

function ShowMore({
  noun,
  list,
}: {
  noun: keyof typeof SHOW_MORE_LABELS;
  list: ReturnType<typeof useSnapshots> | ReturnType<typeof useTemplates>;
}) {
  const { t } = useI18n();
  const { boundary, pages, fetchNextPage } = list;
  if (!pages.isIncomplete || isTerminalError(boundary.error)) return null;

  return (
    <>
      {pages.loadMoreError && !pages.isLoadingMore ? (
        <ErrorNotice
          error={pages.loadMoreError}
          onRetry={pages.retryLoadMore}
        />
      ) : null}
      <Button
        variant={ButtonVariant.Secondary}
        aria-label={t(SHOW_MORE_LABELS[noun])}
        disabled={boundary.isFetching}
        onClick={() => void fetchNextPage({ cancelRefetch: false })}
      >
        {t("common.showMore")}
      </Button>
    </>
  );
}
