// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { FieldMaskSchema } from "@bufbuild/protobuf/wkt";
import {
  createConnectQueryKey,
  createQueryOptions,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useTransport,
} from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { BadgeVariant } from "delino-ui/badge";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { PasswordInput } from "delino-ui/password-input";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import {
  AgentCredential,
  type AgentIntegration,
  AgentIntegrationService,
  type CreateAgentIntegrationRequestSchema,
  type UpdateAgentIntegrationRequestSchema,
} from "@delino/sandrun-protos/v1/agent_integration_pb";
import {
  AgentVendor,
  GitHubInstallationState,
  OwnSandLinkState,
} from "@delino/sandrun-protos/v1/common_pb";
import {
  type GitHubInstallation,
  GitHubService,
} from "@delino/sandrun-protos/v1/github_pb";
import {
  type GetOwnSandLinkResponse,
  type OwnSandLink,
  OwnSandLinkService,
} from "@delino/sandrun-protos/v1/ownsand_link_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { useLayoutEffect, useRef, useState } from "react";
import {
  invalidateServices,
  MAX_PAGE_SIZE,
  useAgentIntegrations,
} from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  CopyValue,
  Detail,
  Empty,
  ErrorNotice,
  FormDialog,
  ListPagination,
  Loading,
  PageHeader,
  QueryBoundary,
  StateBadge,
  showsFullError,
  useActionSignInNotice,
} from "../components/ui.tsx";
import { ownsandDevicesUrl } from "../config.ts";
import { formatTime, labels, vendorName } from "../format.ts";
import { type Localizer, type MessageKey, useI18n } from "../i18n.ts";
import { AGENT_INTEGRATION_NAME_LIMIT, isNameLengthValid } from "../name.ts";
import { externalNavigation } from "../navigation.ts";
import { can, Permission } from "../roles.ts";
import { useRouter } from "../router.tsx";

/** Route `integrations`: the OwnSand link, GitHub and agent integrations. */
export function IntegrationsPage() {
  const { t } = useI18n();
  const { role } = useOrg();
  const admin = can(role, Permission.ManageIntegrations);
  return (
    <>
      <PageHeader
        title={t("integrations.title")}
        description={t("integrations.description")}
      />
      <OwnSandLinkSection admin={admin} />
      <GitHubSection admin={admin} />
      <AgentIntegrationsSection admin={admin} />
    </>
  );
}

/** "Set at <time>" for a credential that SandRun stores and never shows. */
function credentialText(
  set: boolean,
  at: Parameters<typeof formatTime>[0],
  { lang, t }: Localizer,
) {
  return set
    ? t("integrations.credential.setAt", { time: formatTime(at, lang) })
    : t("integrations.credential.notSet");
}

type OwnSandLinkDialog =
  | { kind: "link" | "stale-replacement" }
  | { kind: "replace"; ownsandOrganizationId: string };

type OwnSandLinkIdentity = Pick<
  OwnSandLink,
  "organizationId" | "ownsandOrganizationId" | "createTime"
>;

function isSameOwnSandLink(
  target: OwnSandLinkIdentity | undefined,
  current: OwnSandLink | undefined,
): boolean {
  return (
    target !== undefined &&
    current !== undefined &&
    target.organizationId === current.organizationId &&
    target.ownsandOrganizationId === current.ownsandOrganizationId &&
    target.createTime?.seconds === current.createTime?.seconds &&
    target.createTime?.nanos === current.createTime?.nanos
  );
}

function OwnSandLinkSection({ admin }: { admin: boolean }) {
  const i18n = useI18n();
  const { t } = i18n;
  const { organizationId } = useOrg();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, OwnSandLinkService);
  const getLink = OwnSandLinkService.method.getOwnSandLink;
  const link = useQuery(getLink, { organizationId });
  const setLink = useMutation(OwnSandLinkService.method.setOwnSandLink, {
    onSuccess: refresh,
  });
  const removeLink = useMutation(OwnSandLinkService.method.removeOwnSandLink, {
    onSuccess: refresh,
  });
  const [dialog, setDialog] = useState<OwnSandLinkDialog>();
  const [ownsandOrg, setOwnsandOrg] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [errors, setErrors] = useState<{
    org?: MessageKey;
    key?: MessageKey;
  }>({});
  const [removalTarget, setRemovalTarget] = useState<OwnSandLinkIdentity>();
  const close = () => {
    setDialog(undefined);
    // Credential inputs are cleared, so the value exists only until it is sent.
    setApiKey("");
    setOwnsandOrg("");
    setErrors({});
  };
  const current = link.data?.link;
  if (
    removalTarget !== undefined &&
    link.data !== undefined &&
    !isSameOwnSandLink(removalTarget, current)
  ) {
    setRemovalTarget(undefined);
  }
  // Only a returned link result can invalidate this opening.
  if (
    dialog?.kind === "replace" &&
    link.data !== undefined &&
    current?.ownsandOrganizationId !== dialog.ownsandOrganizationId
  ) {
    setDialog({ kind: "stale-replacement" });
    setApiKey("");
    setErrors({});
  }
  const replacementUnavailable = dialog?.kind === "stale-replacement";
  const replacing = dialog?.kind === "replace" || replacementUnavailable;

  return (
    <section className="section" aria-labelledby="ownsand-title">
      <div className="section-header">
        <h2 id="ownsand-title">{t("integrations.ownSand.title")}</h2>
      </div>
      <QueryBoundary query={link} loading={t("integrations.ownSand.loading")}>
        {() =>
          current === undefined ? (
            <div className="empty">
              <p className="empty-title">
                {t("integrations.ownSand.empty.title")}
              </p>
              <p className="muted">
                {t("integrations.ownSand.empty.description")}
              </p>
              {admin ? (
                <Button onClick={() => setDialog({ kind: "link" })}>
                  {t("integrations.ownSand.link")}
                </Button>
              ) : (
                <p className="muted">
                  {t("integrations.ownSand.empty.member")}
                </p>
              )}
            </div>
          ) : (
            <>
              <dl className="details details-flat">
                <Detail term={t("integrations.ownSand.organization")}>
                  <code>{current.ownsandOrganizationId}</code>
                </Detail>
                <Detail term={t("common.column.state")}>
                  <StateBadge
                    text={t(labels.ownSandLinkState[current.state])}
                    tone={
                      current.state === OwnSandLinkState.INVALID
                        ? BadgeVariant.Danger
                        : BadgeVariant.Accent
                    }
                  />
                </Detail>
                <Detail term={t("integrations.ownSand.apiKey")}>
                  {credentialText(
                    current.credentialSet,
                    current.credentialSetAt,
                    i18n,
                  )}
                </Detail>
                <Detail term={t("integrations.ownSand.devices")}>
                  <a
                    href={ownsandDevicesUrl(current.ownsandOrganizationId)}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {t("integrations.ownSand.manageDevices")}
                  </a>
                </Detail>
              </dl>
              {admin ? (
                <div className="actions">
                  <Button
                    variant={ButtonVariant.Secondary}
                    onClick={() =>
                      setDialog({
                        kind: "replace",
                        ownsandOrganizationId: current.ownsandOrganizationId,
                      })
                    }
                  >
                    {t("integrations.ownSand.replaceKey")}
                  </Button>
                  <Button
                    variant={ButtonVariant.Danger}
                    onClick={() => setRemovalTarget(current)}
                  >
                    {t("integrations.ownSand.removeLink")}
                  </Button>
                </div>
              ) : null}
            </>
          )
        }
      </QueryBoundary>
      <FormDialog
        open={dialog !== undefined}
        onOpenChange={(open) => !open && close()}
        title={
          replacing
            ? t("integrations.ownSand.dialog.replaceTitle")
            : t("integrations.ownSand.link")
        }
        description={t("integrations.ownSand.dialog.description")}
        submitLabel={
          replacing
            ? t("integrations.ownSand.dialog.replaceSubmit")
            : t("integrations.ownSand.dialog.linkSubmit")
        }
        submitDisabled={replacementUnavailable}
        onSubmit={async (opening) => {
          if (dialog === undefined || dialog.kind === "stale-replacement")
            return;
          const org =
            dialog.kind === "replace"
              ? dialog.ownsandOrganizationId
              : ownsandOrg.trim();
          const found: typeof errors = {};
          if (org === "") found.org = "integrations.ownSand.dialog.orgRequired";
          if (apiKey === "")
            found.key = "integrations.ownSand.dialog.keyRequired";
          if (found.org || found.key) {
            setErrors(found);
            return;
          }
          const key = apiKey;
          setApiKey("");
          await setLink.mutateAsync({
            organizationId,
            ownsandOrganizationId: org,
            ownsandApiKey: key,
          });
          if (opening.isCurrent()) close();
        }}
      >
        {replacementUnavailable ? (
          <div className="notice notice-error" role="alert">
            <p>{t("integrations.ownSand.dialog.stale")}</p>
          </div>
        ) : null}
        {dialog?.kind === "link" ? (
          <TextField
            label={t("integrations.ownSand.dialog.orgLabel")}
            value={ownsandOrg}
            onValueChange={(v) => {
              setOwnsandOrg(v);
              setErrors((e) => ({ ...e, org: undefined }));
            }}
            error={errors.org && t(errors.org)}
            autoFocus
            autoComplete="off"
          />
        ) : null}
        <PasswordInput
          label={t("integrations.ownSand.dialog.keyLabel")}
          toggleLabel={t("integrations.showApiKey")}
          value={apiKey}
          onValueChange={(v: string) => {
            setApiKey(v);
            setErrors((e) => ({ ...e, key: undefined }));
          }}
          error={errors.key && t(errors.key)}
          disabled={replacementUnavailable}
          autoFocus={dialog?.kind === "replace"}
          autoComplete="off"
        />
      </FormDialog>
      <ConfirmDialog
        open={removalTarget !== undefined}
        onOpenChange={(open) => {
          if (!open) setRemovalTarget(undefined);
        }}
        title={t("integrations.ownSand.removeDialog.title")}
        description={t("integrations.ownSand.removeDialog.description")}
        confirmLabel={t("integrations.ownSand.removeLink")}
        onConfirm={async () => {
          // A refresh may update the cache before React displays the result.
          const { queryKey } = createQueryOptions(
            getLink,
            { organizationId },
            { transport },
          );
          const latest =
            queryClient.getQueryData<GetOwnSandLinkResponse>(queryKey);
          if (!isSameOwnSandLink(removalTarget, latest?.link)) {
            setRemovalTarget(undefined);
            return;
          }
          await removeLink.mutateAsync({ organizationId });
        }}
      />
    </section>
  );
}

function GitHubSection({ admin }: { admin: boolean }) {
  const { lang, t } = useI18n();
  const { organizationId } = useOrg();
  const { isCurrentVisit } = useRouter();
  const { signInError, reportActionError } = useActionSignInNotice();
  const mounted = useRef(false);
  useLayoutEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, GitHubService);
  const list = useInfiniteQuery(
    GitHubService.method.listGitHubInstallations,
    { organizationId, pageSize: MAX_PAGE_SIZE, pageToken: "" },
    {
      pageParamKey: "pageToken",
      getNextPageParam: (page) => page.nextPageToken || undefined,
    },
  );
  const installUrl = useMutation(GitHubService.method.createGitHubInstallUrl);
  const unlink = useMutation(GitHubService.method.unlinkGitHubInstallation, {
    onSuccess: refresh,
  });
  const [unlinking, setUnlinking] = useState<GitHubInstallation>();
  const [grouping, setGrouping] = useState<GitHubInstallation>();

  const connect = async () => {
    try {
      const res = await installUrl.mutateAsync({ organizationId });
      if (mounted.current && isCurrentVisit()) {
        externalNavigation.assign(res.url);
      }
    } catch (error) {
      if (mounted.current && isCurrentVisit()) {
        reportActionError(error);
      }
    }
  };

  return (
    <section className="section" aria-labelledby="github-title">
      <div className="section-header">
        <h2 id="github-title">GitHub</h2>
        {admin ? (
          <Button
            variant={ButtonVariant.Secondary}
            onClick={() => void connect()}
            disabled={installUrl.isPending}
          >
            {t("integrations.github.connect")}
          </Button>
        ) : null}
      </div>
      <p className="muted">
        {t("integrations.github.description", { label: "runs-on" })}
      </p>
      {signInError ? <ErrorNotice error={signInError} /> : null}
      <QueryBoundary query={list} loading={t("integrations.github.loading")}>
        {(data) => {
          const installations = new Map<string, GitHubInstallation>();
          for (const page of data.pages) {
            for (const installation of page.installations) {
              installations.set(installation.installationId, installation);
            }
          }
          return installations.size === 0 ? (
            <Empty title={t("integrations.github.empty.title")}>
              {admin
                ? t("integrations.github.empty.admin")
                : t("integrations.github.empty.member")}
            </Empty>
          ) : (
            <ul
              className="list"
              aria-label={t("integrations.github.listLabel")}
            >
              {Array.from(installations.values(), (i) => (
                <li key={i.installationId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {i.githubAccountLogin} ·{" "}
                      {t(labels.gitHubInstallationState[i.state])}
                    </span>
                    <span className="muted">
                      {t("integrations.github.runnerGroup", {
                        name:
                          i.runnerGroupName ||
                          t("integrations.github.defaultGroup"),
                      })}{" "}
                      ·{" "}
                      {t("integrations.github.linked", {
                        time: formatTime(i.createTime, lang),
                      })}
                    </span>
                  </div>
                  {admin ? (
                    <div className="list-actions">
                      {i.state === GitHubInstallationState.ACTIVE ? (
                        <Button
                          variant={ButtonVariant.Secondary}
                          size={ButtonSize.Small}
                          onClick={() => setGrouping(i)}
                        >
                          {t("integrations.github.changeGroup")}
                        </Button>
                      ) : null}
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        onClick={() => setUnlinking(i)}
                      >
                        {t("integrations.github.unlink")}
                      </Button>
                    </div>
                  ) : null}
                </li>
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      <ListPagination query={list} />
      {grouping !== undefined ? (
        <RunnerGroupDialog
          installation={grouping}
          onClose={() => setGrouping(undefined)}
        />
      ) : null}
      <ConfirmDialog
        open={unlinking !== undefined}
        onOpenChange={(open) => !open && setUnlinking(undefined)}
        title={
          unlinking === undefined
            ? t("integrations.github.unlinkDialog.titleUnnamed")
            : t("integrations.github.unlinkDialog.title", {
                name: unlinking.githubAccountLogin,
              })
        }
        description={t("integrations.github.unlinkDialog.description")}
        confirmLabel={t("integrations.github.unlink")}
        onConfirm={() =>
          unlink.mutateAsync({
            organizationId,
            installationId: unlinking!.installationId,
          })
        }
      />
    </section>
  );
}

function RunnerGroupDialog({
  installation,
  onClose,
}: {
  installation: GitHubInstallation;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const groups = useQuery(GitHubService.method.listGitHubRunnerGroups, {
    organizationId,
    installationId: installation.installationId,
  });
  const update = useMutation(GitHubService.method.updateGitHubInstallation, {
    onSuccess: () => invalidateServices(queryClient, GitHubService),
  });
  const [group, setGroup] = useState(String(installation.githubRunnerGroupId));
  return (
    <FormDialog
      open
      onOpenChange={(open) => !open && onClose()}
      title={t("integrations.github.groupDialog.title")}
      description={t("integrations.github.groupDialog.description")}
      submitLabel={t("common.save")}
      submitDisabled={groups.data === undefined || showsFullError(groups)}
      onSubmit={async (opening) => {
        await update.mutateAsync({
          organizationId,
          installationId: installation.installationId,
          githubRunnerGroupId: BigInt(group),
        });
        if (opening.isCurrent()) onClose();
      }}
    >
      <QueryBoundary
        query={groups}
        loading={t("integrations.github.groupDialog.loading")}
      >
        {(data) => (
          <Select
            label={t("integrations.github.groupDialog.label")}
            value={group}
            onValueChange={(v) => v && setGroup(v)}
          >
            {data.runnerGroups.map((g) => (
              <SelectOption
                key={String(g.githubRunnerGroupId)}
                value={String(g.githubRunnerGroupId)}
              >
                {t(
                  g.isDefault
                    ? "integrations.github.groupDialog.defaultOption"
                    : "integrations.github.groupDialog.option",
                  { name: g.name, visibility: g.visibility },
                )}
              </SelectOption>
            ))}
          </Select>
        )}
      </QueryBoundary>
    </FormDialog>
  );
}

const VENDORS = [
  AgentVendor.CLAUDE_CODE,
  AgentVendor.CURSOR,
  AgentVendor.OPENAI_CODEX,
  AgentVendor.DEVIN,
];

/** The credential fields of each vendor, in the order of the protos contract. */
export const VENDOR_CREDENTIALS: Record<
  AgentVendor,
  readonly {
    key: CredentialKey;
    credential: AgentCredential;
    /** The catalog key of the field label; `{scope}` names an API scope. */
    label: MessageKey;
    /** The catalog key of the accessible name of the show toggle. */
    show: MessageKey;
    /** The catalog key of the error of an empty field. */
    required: MessageKey;
  }[]
> = {
  [AgentVendor.UNSPECIFIED]: [],
  [AgentVendor.CLAUDE_CODE]: [
    {
      key: "environmentKey",
      credential: AgentCredential.ENVIRONMENT_KEY,
      label: "integrations.agent.credential.environmentKey",
      show: "integrations.agent.credential.environmentKeyShow",
      required: "integrations.agent.credential.environmentKeyRequired",
    },
  ],
  [AgentVendor.CURSOR]: [
    {
      key: "serviceAccountApiKey",
      credential: AgentCredential.SERVICE_ACCOUNT_API_KEY,
      label: "integrations.agent.credential.serviceAccountApiKey",
      show: "integrations.agent.credential.serviceAccountApiKeyShow",
      required: "integrations.agent.credential.serviceAccountApiKeyRequired",
    },
  ],
  [AgentVendor.OPENAI_CODEX]: [
    {
      key: "environmentKey",
      credential: AgentCredential.ENVIRONMENT_KEY,
      label: "integrations.agent.credential.environmentKey",
      show: "integrations.agent.credential.environmentKeyShow",
      required: "integrations.agent.credential.environmentKeyRequired",
    },
    {
      key: "webhookSecret",
      credential: AgentCredential.WEBHOOK_SECRET,
      label: "integrations.agent.credential.webhookSecret",
      show: "integrations.agent.credential.webhookSecretShow",
      required: "integrations.agent.credential.webhookSecretRequired",
    },
    {
      key: "apiKey",
      credential: AgentCredential.API_KEY,
      label: "integrations.agent.credential.openAiApiKey",
      show: "integrations.agent.credential.openAiApiKeyShow",
      required: "integrations.agent.credential.openAiApiKeyRequired",
    },
  ],
  [AgentVendor.DEVIN]: [
    {
      key: "apiToken",
      credential: AgentCredential.API_TOKEN,
      label: "integrations.agent.credential.apiToken",
      show: "integrations.agent.credential.apiTokenShow",
      required: "integrations.agent.credential.apiTokenRequired",
    },
    {
      key: "outpostToken",
      credential: AgentCredential.OUTPOST_TOKEN,
      label: "integrations.agent.credential.outpostToken",
      show: "integrations.agent.credential.outpostTokenShow",
      required: "integrations.agent.credential.outpostTokenRequired",
    },
  ],
};

type CredentialKey =
  | "environmentKey"
  | "serviceAccountApiKey"
  | "webhookSecret"
  | "apiKey"
  | "apiToken"
  | "outpostToken";

type CredentialValues = Partial<Record<CredentialKey, string>>;

type Settings = MessageInitShape<
  typeof CreateAgentIntegrationRequestSchema
>["settings"];
type Credentials = MessageInitShape<
  typeof CreateAgentIntegrationRequestSchema
>["credentials"];

/** The settings oneof of a vendor. */
export function vendorSettings(
  vendor: AgentVendor,
  settingName: string,
): Settings {
  switch (vendor) {
    case AgentVendor.CLAUDE_CODE:
      return { case: "claudeCode", value: {} };
    case AgentVendor.CURSOR:
      return { case: "cursor", value: { poolName: settingName } };
    case AgentVendor.OPENAI_CODEX:
      return { case: "openaiCodex", value: {} };
    case AgentVendor.DEVIN:
      return { case: "devin", value: { outpostName: settingName } };
    default:
      return undefined;
  }
}

/** The credentials oneof of a vendor. Empty values stay empty. */
export function vendorCredentials(
  vendor: AgentVendor,
  values: CredentialValues,
): Credentials {
  switch (vendor) {
    case AgentVendor.CLAUDE_CODE:
      return {
        case: "claudeCodeCredentials",
        value: { environmentKey: values.environmentKey ?? "" },
      };
    case AgentVendor.CURSOR:
      return {
        case: "cursorCredentials",
        value: { serviceAccountApiKey: values.serviceAccountApiKey ?? "" },
      };
    case AgentVendor.OPENAI_CODEX:
      return {
        case: "openaiCodexCredentials",
        value: {
          environmentKey: values.environmentKey ?? "",
          webhookSecret: values.webhookSecret ?? "",
          apiKey: values.apiKey ?? "",
        },
      };
    case AgentVendor.DEVIN:
      return {
        case: "devinCredentials",
        value: {
          apiToken: values.apiToken ?? "",
          outpostToken: values.outpostToken ?? "",
        },
      };
    default:
      return undefined;
  }
}

/**
 * The catalog key of the vendor setting (pool or outpost name) of a vendor,
 * if it has one.
 */
function settingLabel(vendor: AgentVendor): MessageKey | undefined {
  if (vendor === AgentVendor.CURSOR) return "integrations.agent.cursorPool";
  if (vendor === AgentVendor.DEVIN) return "integrations.agent.devinOutpost";
  return undefined;
}

/** The OpenAI API key scope that the Codex integration needs. */
const OPENAI_API_KEY_SCOPE = "api.agents.read";

function settingValue(i: AgentIntegration): string {
  if (i.settings.case === "cursor") return i.settings.value.poolName;
  if (i.settings.case === "devin") return i.settings.value.outpostName;
  return "";
}

function AgentIntegrationsSection({ admin }: { admin: boolean }) {
  const i18n = useI18n();
  const { t, rich } = i18n;
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const { integrations, hasNextPage, isLoadingMore, query } =
    useAgentIntegrations(organizationId);
  const remove = useMutation(
    AgentIntegrationService.method.deleteAgentIntegration,
    {
      onSuccess: () => invalidateServices(queryClient, AgentIntegrationService),
    },
  );
  const [editing, setEditing] = useState<AgentIntegration | "new">();
  const [deleting, setDeleting] = useState<AgentIntegration>();

  return (
    <section className="section" aria-labelledby="agents-title">
      <div className="section-header">
        <h2 id="agents-title">{t("integrations.agent.title")}</h2>
        {admin ? (
          <Button
            variant={ButtonVariant.Secondary}
            onClick={() => setEditing("new")}
          >
            {t("integrations.agent.create")}
          </Button>
        ) : null}
      </div>
      <p className="muted">{t("integrations.agent.description")}</p>
      <QueryBoundary query={query} loading={t("profiles.loadingIntegrations")}>
        {() => (
          <>
            {isLoadingMore ? (
              <Loading label={t("profiles.loadingIntegrations")} />
            ) : null}
            {integrations.length === 0 && !hasNextPage ? (
              <Empty title={t("integrations.agent.empty.title")}>
                {admin
                  ? t("integrations.agent.empty.admin")
                  : t("integrations.agent.empty.member")}
              </Empty>
            ) : integrations.length > 0 ? (
              <ul className="list" aria-label={t("integrations.agent.title")}>
                {integrations.map((i) => {
                  const settingKey = settingLabel(i.vendor);
                  return (
                    <li key={i.agentIntegrationId} className="list-row">
                      <div className="list-main">
                        <span className="list-title">
                          {i.name} · {vendorName(i.vendor, t)}
                        </span>
                        {settingKey !== undefined ? (
                          <span className="muted">
                            {t("integrations.agent.settingValue", {
                              label: t(settingKey),
                              value: settingValue(i),
                            })}
                          </span>
                        ) : null}
                        {i.credentials.map((c) => (
                          <span className="muted" key={c.credential}>
                            {t("integrations.agent.credentialState", {
                              credential: t(labels.credential[c.credential]),
                              state: credentialText(
                                c.credentialSet,
                                c.credentialSetAt,
                                i18n,
                              ),
                            })}
                          </span>
                        ))}
                        {i.vendor === AgentVendor.OPENAI_CODEX &&
                        i.webhookUrl !== "" ? (
                          <span>
                            {rich("integrations.agent.webhookUrl", {
                              value: (
                                <CopyValue
                                  label={t(
                                    "integrations.agent.webhookUrlLabel",
                                    {
                                      name: i.name,
                                    },
                                  )}
                                  value={i.webhookUrl}
                                />
                              ),
                            })}
                          </span>
                        ) : null}
                      </div>
                      {admin ? (
                        <div className="list-actions">
                          <Button
                            variant={ButtonVariant.Secondary}
                            size={ButtonSize.Small}
                            onClick={() => setEditing(i)}
                          >
                            {t("profiles.edit")}
                          </Button>
                          <Button
                            variant={ButtonVariant.Danger}
                            size={ButtonSize.Small}
                            onClick={() => setDeleting(i)}
                          >
                            {t("common.delete")}
                          </Button>
                        </div>
                      ) : null}
                    </li>
                  );
                })}
              </ul>
            ) : null}
          </>
        )}
      </QueryBoundary>
      {editing !== undefined ? (
        <IntegrationDialog
          integration={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
        />
      ) : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={
          deleting === undefined
            ? t("integrations.agent.deleteDialog.titleUnnamed")
            : t("integrations.agent.deleteDialog.title", {
                name: deleting.name,
              })
        }
        description={t("integrations.agent.deleteDialog.description")}
        confirmLabel={t("integrations.agent.deleteDialog.confirm")}
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            agentIntegrationId: deleting!.agentIntegrationId,
          })
        }
      />
    </section>
  );
}

function IntegrationDialog({
  integration,
  onClose,
}: {
  integration: AgentIntegration | undefined;
  onClose: () => void;
}) {
  const i18n = useI18n();
  const { t } = i18n;
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () =>
    invalidateServices(queryClient, AgentIntegrationService);
  const createIntegration = useMutation(
    AgentIntegrationService.method.createAgentIntegration,
    {
      onSuccess: async (_, { organizationId }) => {
        // Invalidation alone reuses an in-flight fetch when no page is cached.
        // That first response may contain the list from before creation.
        await queryClient.cancelQueries({
          queryKey: createConnectQueryKey({
            schema: AgentIntegrationService.method.listAgentIntegrations,
            input: { organizationId },
            cardinality: "infinite",
          }),
          fetchStatus: "fetching",
          predicate: (query) => query.state.data === undefined,
        });
        await refresh();
      },
    },
  );
  const updateIntegration = useMutation(
    AgentIntegrationService.method.updateAgentIntegration,
    { onSuccess: refresh },
  );
  const creating = integration === undefined;
  const [vendor, setVendor] = useState(
    integration?.vendor ?? AgentVendor.CLAUDE_CODE,
  );
  const [name, setName] = useState(integration?.name ?? "");
  const initialSetting =
    integration === undefined ? "" : settingValue(integration);
  const [setting, setSetting] = useState(initialSetting);
  const [secrets, setSecrets] = useState<CredentialValues>({});
  const [errors, setErrors] = useState<Record<string, string | undefined>>({});
  const fields = VENDOR_CREDENTIALS[vendor];
  const label = settingLabel(vendor);

  return (
    <FormDialog
      open
      onOpenChange={(open) => {
        if (!open) {
          setSecrets({});
          onClose();
        }
      }}
      title={
        creating
          ? t("integrations.agent.dialog.createTitle")
          : t("profiles.dialog.editTitle", { name: integration.name })
      }
      description={
        creating
          ? t("integrations.agent.dialog.createDescription")
          : t("integrations.agent.dialog.editDescription")
      }
      submitLabel={
        creating
          ? t("integrations.agent.dialog.createSubmit")
          : t("common.saveChanges")
      }
      onSubmit={async (opening) => {
        const found: Record<string, string> = {};
        const trimmedName = name.trim();
        if (!isNameLengthValid(trimmedName, AGENT_INTEGRATION_NAME_LIMIT)) {
          found.name = t("integrations.agent.dialog.nameInvalid", {
            max: AGENT_INTEGRATION_NAME_LIMIT,
          });
        }
        if (label !== undefined && setting.trim() === "") {
          found.setting = t(
            vendor === AgentVendor.CURSOR
              ? "integrations.agent.cursorPoolRequired"
              : "integrations.agent.devinOutpostRequired",
          );
        }
        if (creating) {
          for (const f of fields) {
            if ((secrets[f.key] ?? "") === "") {
              found[f.key] = t(f.required);
            }
          }
        }
        if (Object.keys(found).length > 0) {
          setErrors(found);
          return;
        }
        const values = secrets;
        // Clear the inputs before the request: the values exist only in
        // this submission.
        setSecrets({});
        if (creating) {
          await createIntegration.mutateAsync({
            organizationId,
            vendor,
            name: trimmedName,
            settings: vendorSettings(vendor, setting.trim()),
            credentials: vendorCredentials(vendor, values),
          });
        } else {
          const paths: string[] = [];
          if (trimmedName !== integration.name) paths.push("name");
          if (label !== undefined && setting.trim() !== initialSetting) {
            paths.push("settings");
          }
          const replaced = fields.some((f) => (values[f.key] ?? "") !== "");
          if (replaced) paths.push("credentials");
          if (paths.length > 0) {
            const request: MessageInitShape<
              typeof UpdateAgentIntegrationRequestSchema
            > = {
              organizationId,
              agentIntegrationId: integration.agentIntegrationId,
              name: trimmedName,
              settings: vendorSettings(vendor, setting.trim()),
              credentials: replaced
                ? vendorCredentials(vendor, values)
                : undefined,
              updateMask: create(FieldMaskSchema, { paths }),
            };
            await updateIntegration.mutateAsync(request);
          }
        }
        if (opening.isCurrent()) onClose();
      }}
    >
      {creating ? (
        <Select
          label={t("integrations.agent.dialog.vendor")}
          value={String(vendor)}
          onValueChange={(v) => {
            if (!v) return;
            setVendor(Number(v) as AgentVendor);
            setSecrets({});
            setErrors({});
          }}
        >
          {VENDORS.map((v) => (
            <SelectOption key={v} value={String(v)}>
              {vendorName(v, t)}
            </SelectOption>
          ))}
        </Select>
      ) : (
        <p className="muted">
          {t("integrations.agent.dialog.vendorValue", {
            vendor: vendorName(vendor, t),
          })}
        </p>
      )}
      <TextField
        label={t("profiles.dialog.name")}
        value={name}
        onValueChange={(v) => {
          setName(v);
          setErrors((e) => ({ ...e, name: undefined }));
        }}
        error={errors.name}
        autoFocus
        autoComplete="off"
      />
      {label !== undefined ? (
        <TextField
          label={t(label)}
          value={setting}
          onValueChange={(v) => {
            setSetting(v);
            setErrors((e) => ({ ...e, setting: undefined }));
          }}
          error={errors.setting}
          autoComplete="off"
        />
      ) : null}
      {fields.map((f) => {
        const state = integration?.credentials.find(
          (c) => c.credential === f.credential,
        );
        return (
          <PasswordInput
            key={f.key}
            label={t(f.label, { scope: OPENAI_API_KEY_SCOPE })}
            toggleLabel={t(f.show)}
            description={
              state === undefined
                ? undefined
                : t("integrations.agent.dialog.replaceHint", {
                    state: credentialText(
                      state.credentialSet,
                      state.credentialSetAt,
                      i18n,
                    ),
                  })
            }
            value={secrets[f.key] ?? ""}
            onValueChange={(v: string) => {
              setSecrets((s) => ({ ...s, [f.key]: v }));
              setErrors((e) => ({ ...e, [f.key]: undefined }));
            }}
            error={errors[f.key]}
            autoComplete="off"
          />
        );
      })}
    </FormDialog>
  );
}
