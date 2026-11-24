// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
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
  PageHeader,
  QueryBoundary,
  ShortId,
  isTerminalError,
  shortIdText,
  toastError,
} from "../components/ui.tsx";
import { formatBytes, formatTime, labels } from "../format.ts";
import { Link, RouteId, routePath } from "../router.tsx";
import { useSnapshots, useTemplates } from "../snapshot-lists.ts";

/** Route `snapshots`: snapshots, their export, and templates. */
export function SnapshotsPage() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
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
    onSuccess: refresh,
  });
  const deleteTemplate = useMutation(SnapshotService.method.deleteTemplate, {
    onSuccess: refresh,
  });
  const [templateFrom, setTemplateFrom] = useState<Snapshot>();
  const [templateName, setTemplateName] = useState("");
  const [deletingSnapshot, setDeletingSnapshot] = useState<Snapshot>();
  const [deletingTemplate, setDeletingTemplate] = useState<Template>();

  // An open action belongs to its list. Discard it when access to that list ends.
  if (isTerminalError(snapshots.boundary.error)) {
    if (templateFrom) {
      setTemplateFrom(undefined);
      setTemplateName("");
    }
    if (deletingSnapshot) setDeletingSnapshot(undefined);
  }
  if (deletingTemplate && isTerminalError(templates.boundary.error)) {
    setDeletingTemplate(undefined);
  }

  return (
    <>
      <PageHeader
        title="Snapshots"
        description="Snapshots stay on their device. Export one to use it on other devices with the same OS and architecture."
      />
      <QueryBoundary query={snapshots.boundary} loading="Loading snapshots">
        {() =>
          snapshots.rows.length === 0 ? (
            <Empty title="No snapshots">
              Take a snapshot from a running or paused sandbox.
            </Empty>
          ) : (
            <ul className="list" aria-label="Snapshots">
              {snapshots.rows.map((s) => {
                const exportable =
                  s.state === SnapshotState.READY &&
                  (s.exportState === SnapshotExportState.NOT_EXPORTED ||
                    s.exportState === SnapshotExportState.EXPORT_FAILED);
                return (
                  <li key={s.snapshotId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        Snapshot <ShortId id={s.snapshotId} /> ·{" "}
                        {labels.snapshotState[s.state]} ·{" "}
                        {labels.exportState[s.exportState]}
                      </span>
                      <span className="muted">
                        {labels.guestOs[s.guestOs]} · {formatBytes(s.sizeBytes)}
                        {s.hasMemoryState ? " · with memory state" : ""} · from{" "}
                        <Link
                          to={routePath({
                            id: RouteId.Sandbox,
                            organizationId,
                            sandboxId: s.sourceSandboxId,
                          })}
                        >
                          <ShortId id={s.sourceSandboxId} />
                        </Link>{" "}
                        · {formatTime(s.createTime)}
                      </span>
                    </div>
                    <div className="list-actions">
                      {exportable ? (
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          aria-label={`Export snapshot ${shortIdText(s.snapshotId)}`}
                          onClick={async () => {
                            try {
                              await exportSnapshot.mutateAsync({
                                organizationId,
                                snapshotId: s.snapshotId,
                              });
                              toast.success("The export started.");
                            } catch (error) {
                              toastError(error);
                            }
                          }}
                        >
                          Export
                        </Button>
                      ) : null}
                      {s.state === SnapshotState.READY ? (
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          aria-label={`Make template from snapshot ${shortIdText(s.snapshotId)}`}
                          onClick={() => setTemplateFrom(s)}
                        >
                          Make template
                        </Button>
                      ) : null}
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        aria-label={`Delete snapshot ${shortIdText(s.snapshotId)}`}
                        onClick={() => setDeletingSnapshot(s)}
                      >
                        Delete
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ul>
          )
        }
      </QueryBoundary>
      <ShowMore noun="snapshots" list={snapshots} />
      <section className="section" aria-labelledby="templates-title">
        <h2 id="templates-title">Templates</h2>
        <QueryBoundary query={templates.boundary} loading="Loading templates">
          {() =>
            templates.rows.length === 0 ? (
              <Empty title="No templates">
                A template gives a snapshot a name to create sandboxes from.
              </Empty>
            ) : (
              <ul className="list" aria-label="Templates">
                {templates.rows.map((t) => (
                  <li key={t.templateId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">{t.name}</span>
                      <span className="muted">
                        From snapshot <ShortId id={t.snapshotId} /> ·{" "}
                        {formatTime(t.createTime)}
                      </span>
                    </div>
                    <Button
                      variant={ButtonVariant.Danger}
                      size={ButtonSize.Small}
                      aria-label={`Delete template ${t.name} from snapshot ${shortIdText(t.snapshotId)}`}
                      onClick={() => setDeletingTemplate(t)}
                    >
                      Delete
                    </Button>
                  </li>
                ))}
              </ul>
            )
          }
        </QueryBoundary>
        <ShowMore noun="templates" list={templates} />
      </section>
      <FormDialog
        open={templateFrom !== undefined}
        onOpenChange={(open) => {
          if (!open) {
            setTemplateFrom(undefined);
            setTemplateName("");
          }
        }}
        title="Make a template"
        submitLabel="Create template"
        onSubmit={async (opening) => {
          await createTemplate.mutateAsync({
            organizationId,
            snapshotId: templateFrom!.snapshotId,
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
          label="Template name"
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
        title={`Delete the snapshot ${deletingSnapshot ? shortIdText(deletingSnapshot.snapshotId) : ""}?`}
        description="Delete its templates first. Sandboxes made from it keep running."
        confirmLabel="Delete snapshot"
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
        title={`Delete the template ${deletingTemplate?.name ?? ""}?`}
        description="Its snapshot stays."
        confirmLabel="Delete template"
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

function ShowMore({
  noun,
  list,
}: {
  noun: "snapshots" | "templates";
  list: ReturnType<typeof useSnapshots> | ReturnType<typeof useTemplates>;
}) {
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
        aria-label={`Show more ${noun}`}
        disabled={boundary.isFetching}
        onClick={() => void fetchNextPage({ cancelRefetch: false })}
      >
        Show more
      </Button>
    </>
  );
}
