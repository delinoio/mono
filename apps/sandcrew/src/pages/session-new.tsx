// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import {
  AgentVendor,
  ConfigOptionCategory,
  CredentialScope,
} from "@delino/sandcrew-protos/v1/common_pb";
import { SessionService } from "@delino/sandcrew-protos/v1/session_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button } from "delino-ui/button";
import { Checkbox } from "delino-ui/checkbox";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { Textarea } from "delino-ui/textarea";
import { type FormEvent, useState } from "react";
import { invalidateServices } from "../api.ts";
import {
  useRunnerProfiles,
  useSessionSecrets,
  useVendorCredentials,
} from "../queries.ts";
import { canManageOrganization, useOrg } from "../components/shell.tsx";
import {
  ErrorNotice,
  Loading,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { labels, VENDORS } from "../format.ts";
import { Link, orgPath, RouteId, sessionPath, useRouter } from "../router.tsx";

/** The longest session label. */
export const LABEL_LIMIT = 100;

/** The select value for "use the agent's default". */
const AGENT_DEFAULT = "";

/** Route `SESSION_NEW`: the new-session form. */
export function SessionNewPage() {
  const { organizationId, role, userId } = useOrg();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const profiles = useRunnerProfiles(organizationId);
  const credentials = useVendorCredentials(organizationId);
  const secrets = useSessionSecrets(organizationId);

  const [vendor, setVendor] = useState(AgentVendor.CLAUDE_CODE);
  const [profileId, setProfileId] = useState<string>();
  const [message, setMessage] = useState("");
  const [label, setLabel] = useState("");
  const [secretIds, setSecretIds] = useState<ReadonlySet<string>>(new Set());
  const [modeId, setModeId] = useState(AGENT_DEFAULT);
  const [optionValues, setOptionValues] = useState<Record<string, string>>({});
  const [errors, setErrors] = useState<{ message?: string; label?: string }>(
    {},
  );
  const options = useQuery(SessionService.method.getAgentOptions, {
    organizationId,
    vendor,
  });
  const create = useMutation(SessionService.method.createSession, {
    onSuccess: () => invalidateServices(queryClient, SessionService),
  });

  const selectable = (profiles.data ?? []).filter(
    (p) => p.allowed && p.selectable,
  );
  const selectedProfile =
    selectable.find((p) => p.runnerProfileId === profileId) ?? selectable[0];
  const hasCredential = (credentials.data ?? []).some(
    (c) =>
      c.vendor === vendor &&
      (c.scope === CredentialScope.ORGANIZATION ||
        (c.scope === CredentialScope.USER && c.ownerUserId === userId)),
  );
  const modelOptions = (options.data?.configOptions ?? []).filter(
    (o) => o.category === ConfigOptionCategory.MODEL,
  );
  const modes = options.data?.availableModes ?? [];

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (create.isPending || selectedProfile === undefined) return;
    const text = message.trim();
    const trimmedLabel = label.trim();
    const nextErrors: typeof errors = {};
    if (text === "") nextErrors.message = "Enter the first message.";
    if ([...trimmedLabel].length > LABEL_LIMIT) {
      nextErrors.label = `Enter at most ${LABEL_LIMIT} characters.`;
    }
    setErrors(nextErrors);
    if (nextErrors.message || nextErrors.label) return;
    try {
      const res = await create.mutateAsync({
        organizationId,
        vendor,
        runnerProfileId: selectedProfile.runnerProfileId,
        firstMessage: text,
        label: trimmedLabel,
        sessionSecretIds: [...secretIds],
        modeId,
        configOptions: Object.entries(optionValues)
          .filter(([, valueId]) => valueId !== AGENT_DEFAULT)
          .map(([configOptionId, valueId]) => ({ configOptionId, valueId })),
      });
      navigate(sessionPath(organizationId, res.session!.sessionId));
    } catch {
      // The form shows the mutation error.
    }
  };

  return (
    <>
      <PageHeader
        title="New session"
        description="The agent starts in an empty workspace in a SandRun sandbox. Tell it what to do, for example which repository to clone."
      />
      <QueryBoundary query={profiles} loading="Loading runner profiles">
        {() =>
          selectable.length === 0 ? (
            <NoProfileNotice
              canManage={canManageOrganization(role)}
              organizationId={organizationId}
            />
          ) : (
            <form className="form form-page" onSubmit={submit} noValidate>
              <Textarea
                label="First message"
                description="The first prompt of the session."
                value={message}
                onValueChange={(v: string) => {
                  setMessage(v);
                  setErrors((e) => ({ ...e, message: undefined }));
                }}
                error={errors.message}
                autoFocus
                autoResize
                rows={4}
              />
              <Select
                label="Agent"
                value={String(vendor)}
                onValueChange={(v) => {
                  if (v === null) return;
                  setVendor(Number(v) as AgentVendor);
                  setModeId(AGENT_DEFAULT);
                  setOptionValues({});
                }}
              >
                {VENDORS.map((v) => (
                  <SelectOption key={v} value={String(v)}>
                    {labels.vendor[v]}
                  </SelectOption>
                ))}
              </Select>
              {credentials.data !== undefined && !hasCredential ? (
                <div className="notice notice-warning" role="alert">
                  <p>
                    You have no {labels.vendor[vendor]} credential, and the
                    organization has none either. Add an API key or a token
                    before you start a session.
                  </p>
                  <Link to={orgPath(RouteId.SettingsUser, organizationId)}>
                    Add a credential in your settings
                  </Link>
                </div>
              ) : null}
              <Select
                label="Runner profile"
                description={
                  selectedProfile === undefined
                    ? undefined
                    : `${labels.guestOs[selectedProfile.guestOs]} · ${selectedProfile.vcpuCount} vCPU · ${selectedProfile.memoryMib} MiB memory · ${selectedProfile.diskGib} GiB disk`
                }
                value={selectedProfile?.runnerProfileId ?? null}
                onValueChange={(v) => v !== null && setProfileId(v)}
              >
                {selectable.map((p) => (
                  <SelectOption
                    key={p.runnerProfileId}
                    value={p.runnerProfileId}
                  >
                    {p.name}
                  </SelectOption>
                ))}
              </Select>
              {options.isPending ? (
                <Loading label="Loading agent options" />
              ) : null}
              {modes.length > 0 ? (
                <Select
                  label="Mode"
                  value={modeId}
                  onValueChange={(v) => setModeId(v ?? AGENT_DEFAULT)}
                >
                  <SelectOption value={AGENT_DEFAULT}>
                    Agent default
                  </SelectOption>
                  {modes.map((m) => (
                    <SelectOption key={m.modeId} value={m.modeId}>
                      {m.name}
                    </SelectOption>
                  ))}
                </Select>
              ) : null}
              {modelOptions.map((o) => (
                <Select
                  key={o.configOptionId}
                  label={o.name}
                  value={optionValues[o.configOptionId] ?? AGENT_DEFAULT}
                  onValueChange={(v) =>
                    setOptionValues((current) => ({
                      ...current,
                      [o.configOptionId]: v ?? AGENT_DEFAULT,
                    }))
                  }
                >
                  <SelectOption value={AGENT_DEFAULT}>
                    Agent default
                  </SelectOption>
                  {o.values.map((value) => (
                    <SelectOption key={value.valueId} value={value.valueId}>
                      {value.name}
                    </SelectOption>
                  ))}
                </Select>
              ))}
              <TextField
                label="Label (optional)"
                description="Helps you find the session in the list."
                value={label}
                onValueChange={(v: string) => {
                  setLabel(v);
                  setErrors((e) => ({ ...e, label: undefined }));
                }}
                error={errors.label}
                autoComplete="off"
              />
              <fieldset className="fieldset">
                <legend>Session secrets</legend>
                <p className="muted">
                  The agent gets the selected secrets as environment variables.
                  Code in the sandbox can read them.
                </p>
                <QueryBoundary
                  query={secrets}
                  loading="Loading session secrets"
                >
                  {(list) =>
                    list.length === 0 ? (
                      <p className="muted">
                        You have no session secrets.{" "}
                        <Link
                          to={orgPath(RouteId.SettingsUser, organizationId)}
                        >
                          Add one in your settings
                        </Link>
                      </p>
                    ) : (
                      <div className="checkbox-list">
                        {list.map((s) => (
                          <Checkbox
                            key={s.sessionSecretId}
                            label={s.name}
                            checked={secretIds.has(s.sessionSecretId)}
                            onCheckedChange={(checked) =>
                              setSecretIds((current) => {
                                const next = new Set(current);
                                if (checked === true) {
                                  next.add(s.sessionSecretId);
                                } else {
                                  next.delete(s.sessionSecretId);
                                }
                                return next;
                              })
                            }
                          />
                        ))}
                      </div>
                    )
                  }
                </QueryBoundary>
              </fieldset>
              {create.error ? <ErrorNotice error={create.error} /> : null}
              <div className="form-actions">
                <Button
                  type="submit"
                  disabled={create.isPending || !hasCredential}
                >
                  Start session
                </Button>
              </div>
            </form>
          )
        }
      </QueryBoundary>
    </>
  );
}

function NoProfileNotice({
  canManage,
  organizationId,
}: {
  canManage: boolean;
  organizationId: string;
}) {
  return (
    <div className="empty">
      <p className="empty-title">No runner profile is available</p>
      {canManage ? (
        <p className="muted">
          Allow a runner profile for SandCrew sessions in the organization
          settings.{" "}
          <Link to={orgPath(RouteId.SettingsOrg, organizationId)}>
            Go to the organization settings
          </Link>
        </p>
      ) : (
        <p className="muted">
          Ask an owner or admin of the organization to allow a runner profile
          for SandCrew sessions.
        </p>
      )}
    </div>
  );
}
