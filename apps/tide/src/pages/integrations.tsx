// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { FieldMaskSchema } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
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
} from "@delino/tide-protos/v1/agent_integration_pb";
import {
  AgentVendor,
  GitHubInstallationState,
  OwnSandLinkState,
} from "@delino/tide-protos/v1/common_pb";
import {
  type GitHubInstallation,
  GitHubService,
} from "@delino/tide-protos/v1/github_pb";
import { OwnSandLinkService } from "@delino/tide-protos/v1/ownsand_link_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { useState } from "react";
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
  FormDialog,
  Loading,
  PageHeader,
  QueryBoundary,
  StateBadge,
  showsFullError,
  toastError,
} from "../components/ui.tsx";
import { ownsandDevicesUrl } from "../config.ts";
import { formatTime, labels } from "../format.ts";
import { externalNavigation } from "../navigation.ts";
import { can, Permission } from "../roles.ts";

/** Route `integrations`: the OwnSand link, GitHub and agent integrations. */
export function IntegrationsPage() {
  const { role } = useOrg();
  const admin = can(role, Permission.ManageIntegrations);
  return (
    <>
      <PageHeader
        title="Integrations"
        description="tide runs work on the devices of the linked OwnSand organization. GitHub installations and agent integrations bring the work."
      />
      <OwnSandLinkSection admin={admin} />
      <GitHubSection admin={admin} />
      <AgentIntegrationsSection admin={admin} />
    </>
  );
}

/** "Set at <time>" for a credential that tide stores and never shows. */
function credentialText(set: boolean, at: Parameters<typeof formatTime>[0]) {
  return set ? `Set at ${formatTime(at)}` : "Not set";
}

function OwnSandLinkSection({ admin }: { admin: boolean }) {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, OwnSandLinkService);
  const link = useQuery(OwnSandLinkService.method.getOwnSandLink, {
    organizationId,
  });
  const setLink = useMutation(OwnSandLinkService.method.setOwnSandLink, {
    onSuccess: refresh,
  });
  const removeLink = useMutation(OwnSandLinkService.method.removeOwnSandLink, {
    onSuccess: refresh,
  });
  const [dialog, setDialog] = useState<"link" | "replace">();
  const [ownsandOrg, setOwnsandOrg] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [errors, setErrors] = useState<{ org?: string; key?: string }>({});
  const [removing, setRemoving] = useState(false);
  const close = () => {
    setDialog(undefined);
    // Credential inputs are cleared, so the value exists only until it is sent.
    setApiKey("");
    setOwnsandOrg("");
    setErrors({});
  };
  const current = link.data?.link;

  return (
    <section className="section" aria-labelledby="ownsand-title">
      <div className="section-header">
        <h2 id="ownsand-title">OwnSand link</h2>
      </div>
      <QueryBoundary query={link} loading="Loading the OwnSand link">
        {() =>
          current === undefined ? (
            <div className="empty">
              <p className="empty-title">OwnSand is not linked</p>
              <p className="muted">
                First step: link the OwnSand organization whose devices run your
                work, with an organization API key from the OwnSand console.
              </p>
              {admin ? (
                <Button onClick={() => setDialog("link")}>Link OwnSand</Button>
              ) : (
                <p className="muted">Ask an admin to link OwnSand.</p>
              )}
            </div>
          ) : (
            <>
              <dl className="details details-flat">
                <Detail term="OwnSand organization">
                  <code>{current.ownsandOrganizationId}</code>
                </Detail>
                <Detail term="State">
                  <StateBadge
                    text={labels.ownSandLinkState[current.state]}
                    tone={
                      current.state === OwnSandLinkState.INVALID
                        ? BadgeVariant.Danger
                        : BadgeVariant.Accent
                    }
                  />
                </Detail>
                <Detail term="API key">
                  {credentialText(
                    current.credentialSet,
                    current.credentialSetAt,
                  )}
                </Detail>
                <Detail term="Devices">
                  <a
                    href={ownsandDevicesUrl(current.ownsandOrganizationId)}
                    target="_blank"
                    rel="noreferrer"
                  >
                    Manage devices in the OwnSand console
                  </a>
                </Detail>
              </dl>
              {admin ? (
                <div className="actions">
                  <Button
                    variant={ButtonVariant.Secondary}
                    onClick={() => setDialog("replace")}
                  >
                    Replace API key
                  </Button>
                  <Button
                    variant={ButtonVariant.Danger}
                    onClick={() => setRemoving(true)}
                  >
                    Remove link
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
        title={dialog === "replace" ? "Replace the API key" : "Link OwnSand"}
        description="tide stores the key encrypted and never shows it again. Create an organization API key in the OwnSand console."
        submitLabel={dialog === "replace" ? "Replace key" : "Link"}
        onSubmit={async () => {
          const org =
            dialog === "replace"
              ? current!.ownsandOrganizationId
              : ownsandOrg.trim();
          const found: typeof errors = {};
          if (org === "") found.org = "Enter the OwnSand organization ID.";
          if (apiKey === "") found.key = "Enter the OwnSand API key.";
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
          close();
        }}
      >
        {dialog === "link" ? (
          <TextField
            label="OwnSand organization ID"
            value={ownsandOrg}
            onValueChange={(v) => {
              setOwnsandOrg(v);
              setErrors((e) => ({ ...e, org: undefined }));
            }}
            error={errors.org}
            autoFocus
            autoComplete="off"
          />
        ) : null}
        <PasswordInput
          label="OwnSand API key"
          toggleLabel="Show API key"
          value={apiKey}
          onValueChange={(v: string) => {
            setApiKey(v);
            setErrors((e) => ({ ...e, key: undefined }));
          }}
          error={errors.key}
          autoFocus={dialog === "replace"}
          autoComplete="off"
        />
      </FormDialog>
      <ConfirmDialog
        open={removing}
        onOpenChange={setRemoving}
        title="Remove the OwnSand link?"
        description="tide stops dispatch, ends every queued request with Canceled, deletes every runner of this organization and then deletes the stored API key."
        confirmLabel="Remove link"
        onConfirm={() => removeLink.mutateAsync({ organizationId })}
      />
    </section>
  );
}

function GitHubSection({ admin }: { admin: boolean }) {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, GitHubService);
  const list = useQuery(GitHubService.method.listGitHubInstallations, {
    organizationId,
    pageSize: MAX_PAGE_SIZE,
  });
  const installUrl = useMutation(GitHubService.method.createGitHubInstallUrl);
  const unlink = useMutation(GitHubService.method.unlinkGitHubInstallation, {
    onSuccess: refresh,
  });
  const [unlinking, setUnlinking] = useState<GitHubInstallation>();
  const [grouping, setGrouping] = useState<GitHubInstallation>();

  const connect = async () => {
    try {
      const res = await installUrl.mutateAsync({ organizationId });
      externalNavigation.assign(res.url);
    } catch (error) {
      toastError(error);
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
            Connect GitHub
          </Button>
        ) : null}
      </div>
      <p className="muted">
        tide registers organization-level runners for GitHub Actions jobs whose
        runs-on label names a runner profile. Install the tide GitHub App on a
        GitHub organization; GitHub then returns to this console.
      </p>
      <QueryBoundary query={list} loading="Loading GitHub installations">
        {(data) => {
          const { installations } = data;
          return installations.length === 0 ? (
            <Empty title="No GitHub organization is connected">
              {admin
                ? "Connect GitHub to run GitHub Actions jobs, or create an agent integration below."
                : "Ask an admin to connect GitHub."}
            </Empty>
          ) : (
            <ul className="list" aria-label="GitHub installations">
              {installations.map((i) => (
                <li key={i.installationId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {i.githubAccountLogin} ·{" "}
                      {labels.gitHubInstallationState[i.state]}
                    </span>
                    <span className="muted">
                      Runner group: {i.runnerGroupName || "Default"} · linked{" "}
                      {formatTime(i.createTime)}
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
                          Change runner group
                        </Button>
                      ) : null}
                      <Button
                        variant={ButtonVariant.Danger}
                        size={ButtonSize.Small}
                        onClick={() => setUnlinking(i)}
                      >
                        Unlink
                      </Button>
                    </div>
                  ) : null}
                </li>
              ))}
            </ul>
          );
        }}
      </QueryBoundary>
      {grouping !== undefined ? (
        <RunnerGroupDialog
          installation={grouping}
          onClose={() => setGrouping(undefined)}
        />
      ) : null}
      <ConfirmDialog
        open={unlinking !== undefined}
        onOpenChange={(open) => !open && setUnlinking(undefined)}
        title={`Unlink ${unlinking?.githubAccountLogin ?? "this installation"}?`}
        description="tide cancels the queued requests of this installation and deletes its waiting runners. The GitHub App stays installed in GitHub until you uninstall it there."
        confirmLabel="Unlink"
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
      title="Change the runner group"
      description="GitHub's runner group settings control which repositories can use the tide runners."
      submitLabel="Save"
      submitDisabled={groups.data === undefined || showsFullError(groups)}
      onSubmit={async () => {
        await update.mutateAsync({
          organizationId,
          installationId: installation.installationId,
          githubRunnerGroupId: BigInt(group),
        });
        onClose();
      }}
    >
      <QueryBoundary query={groups} loading="Loading runner groups">
        {(data) => (
          <Select
            label="Runner group"
            value={group}
            onValueChange={(v) => v && setGroup(v)}
          >
            {data.runnerGroups.map((g) => (
              <SelectOption
                key={String(g.githubRunnerGroupId)}
                value={String(g.githubRunnerGroupId)}
              >
                {`${g.name}${g.isDefault ? " (default)" : ""} · ${g.visibility}`}
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
  readonly { key: CredentialKey; credential: AgentCredential; label: string }[]
> = {
  [AgentVendor.UNSPECIFIED]: [],
  [AgentVendor.CLAUDE_CODE]: [
    {
      key: "environmentKey",
      credential: AgentCredential.ENVIRONMENT_KEY,
      label: "Environment key",
    },
  ],
  [AgentVendor.CURSOR]: [
    {
      key: "serviceAccountApiKey",
      credential: AgentCredential.SERVICE_ACCOUNT_API_KEY,
      label: "Service account API key",
    },
  ],
  [AgentVendor.OPENAI_CODEX]: [
    {
      key: "environmentKey",
      credential: AgentCredential.ENVIRONMENT_KEY,
      label: "Environment key",
    },
    {
      key: "webhookSecret",
      credential: AgentCredential.WEBHOOK_SECRET,
      label: "Webhook secret",
    },
    {
      key: "apiKey",
      credential: AgentCredential.API_KEY,
      label: "OpenAI API key (api.agents.read)",
    },
  ],
  [AgentVendor.DEVIN]: [
    {
      key: "apiToken",
      credential: AgentCredential.API_TOKEN,
      label: "API token",
    },
    {
      key: "outpostToken",
      credential: AgentCredential.OUTPOST_TOKEN,
      label: "Outpost token",
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

/** The vendor setting (pool or outpost name) of a vendor, if it has one. */
function settingLabel(vendor: AgentVendor): string | undefined {
  if (vendor === AgentVendor.CURSOR) return "Cursor self-hosted pool name";
  if (vendor === AgentVendor.DEVIN) return "Devin outpost name";
  return undefined;
}

function settingValue(i: AgentIntegration): string {
  if (i.settings.case === "cursor") return i.settings.value.poolName;
  if (i.settings.case === "devin") return i.settings.value.outpostName;
  return "";
}

function AgentIntegrationsSection({ admin }: { admin: boolean }) {
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
        <h2 id="agents-title">Agent integrations</h2>
        {admin ? (
          <Button
            variant={ButtonVariant.Secondary}
            onClick={() => setEditing("new")}
          >
            Create agent integration
          </Button>
        ) : null}
      </div>
      <p className="muted">
        An agent integration holds the account or pool of one coding-agent
        vendor. Agent session runner profiles name one integration.
      </p>
      <QueryBoundary query={query} loading="Loading agent integrations">
        {() => (
          <>
            {isLoadingMore ? (
              <Loading label="Loading agent integrations" />
            ) : null}
            {integrations.length === 0 && !hasNextPage ? (
              <Empty title="No agent integrations">
                {admin
                  ? "Create an integration for Claude Code, Cursor, OpenAI Codex or Devin to run their sessions, or connect GitHub above."
                  : "Ask an admin to create an agent integration."}
              </Empty>
            ) : integrations.length > 0 ? (
              <ul className="list" aria-label="Agent integrations">
                {integrations.map((i) => (
                  <li key={i.agentIntegrationId} className="list-row">
                    <div className="list-main">
                      <span className="list-title">
                        {i.name} · {labels.vendor[i.vendor]}
                      </span>
                      {settingLabel(i.vendor) !== undefined ? (
                        <span className="muted">
                          {settingLabel(i.vendor)}: {settingValue(i)}
                        </span>
                      ) : null}
                      {i.credentials.map((c) => (
                        <span className="muted" key={c.credential}>
                          {labels.credential[c.credential]}:{" "}
                          {credentialText(c.credentialSet, c.credentialSetAt)}
                        </span>
                      ))}
                      {i.vendor === AgentVendor.OPENAI_CODEX &&
                      i.webhookUrl !== "" ? (
                        <span>
                          Webhook URL to register in the OpenAI project:{" "}
                          <CopyValue
                            label={`webhook URL of ${i.name}`}
                            value={i.webhookUrl}
                          />
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
                          Edit
                        </Button>
                        <Button
                          variant={ButtonVariant.Danger}
                          size={ButtonSize.Small}
                          onClick={() => setDeleting(i)}
                        >
                          Delete
                        </Button>
                      </div>
                    ) : null}
                  </li>
                ))}
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
        title={`Delete ${deleting?.name ?? "this integration"}?`}
        description="tide deletes the stored credentials. An integration that a runner profile names cannot be deleted; delete the profile first."
        confirmLabel="Delete integration"
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
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () =>
    invalidateServices(queryClient, AgentIntegrationService);
  const createIntegration = useMutation(
    AgentIntegrationService.method.createAgentIntegration,
    { onSuccess: refresh },
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
        creating ? "Create an agent integration" : `Edit ${integration.name}`
      }
      description={
        creating
          ? "tide stores the credentials encrypted and never shows them again."
          : "Leave a credential empty to keep the stored value. A new value replaces it; tide never shows it again."
      }
      submitLabel={creating ? "Create integration" : "Save changes"}
      onSubmit={async () => {
        const found: Record<string, string> = {};
        if (name.trim() === "" || name.trim().length > 64) {
          found.name = "Enter a name of 1 to 64 characters.";
        }
        if (label !== undefined && setting.trim() === "") {
          found.setting = `Enter the ${label.toLowerCase()}.`;
        }
        if (creating) {
          for (const f of fields) {
            if ((secrets[f.key] ?? "") === "") {
              found[f.key] = `Enter the ${f.label.toLowerCase()}.`;
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
            name: name.trim(),
            settings: vendorSettings(vendor, setting.trim()),
            credentials: vendorCredentials(vendor, values),
          });
        } else {
          const paths: string[] = [];
          if (name.trim() !== integration.name) paths.push("name");
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
              name: name.trim(),
              settings: vendorSettings(vendor, setting.trim()),
              credentials: replaced
                ? vendorCredentials(vendor, values)
                : undefined,
              updateMask: create(FieldMaskSchema, { paths }),
            };
            await updateIntegration.mutateAsync(request);
          }
        }
        onClose();
      }}
    >
      {creating ? (
        <Select
          label="Vendor"
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
              {labels.vendor[v]}
            </SelectOption>
          ))}
        </Select>
      ) : (
        <p className="muted">Vendor: {labels.vendor[vendor]}</p>
      )}
      <TextField
        label="Name"
        value={name}
        onValueChange={(v) => {
          setName(v);
          setErrors((e) => ({ ...e, name: undefined }));
        }}
        error={errors.name}
        autoFocus
        maxLength={64}
        autoComplete="off"
      />
      {label !== undefined ? (
        <TextField
          label={label}
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
            label={f.label}
            toggleLabel={`Show ${f.label.toLowerCase()}`}
            description={
              state === undefined
                ? undefined
                : `${credentialText(state.credentialSet, state.credentialSetAt)}. Enter a new value to replace it.`
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
