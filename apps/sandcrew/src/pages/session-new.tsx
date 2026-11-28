// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { ConfigOptionCategory } from "@delino/sandcrew-protos/v1/bridge_pb";
import {
  AgentVendor,
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
import { profileSummary, VENDORS, vendorName } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { Link, orgPath, RouteId, sessionPath, useRouter } from "../router.tsx";

/** The longest session label. */
export const LABEL_LIMIT = 100;

/** The select value for "use the agent's default". */
const AGENT_DEFAULT = "";

/** Route `SESSION_NEW`: the new-session form. */
export function SessionNewPage() {
  const { organizationId, role, userId } = useOrg();
  const { navigate, isCurrentVisit } = useRouter();
  const i18n = useI18n();
  const { t } = i18n;
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
  const [errors, setErrors] = useState<{
    message?: MessageKey;
    label?: MessageKey;
  }>({});
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
    if (text === "") nextErrors.message = "newSession.error.message";
    if ([...trimmedLabel].length > LABEL_LIMIT) {
      nextErrors.label = "newSession.error.label";
    }
    setErrors(nextErrors);
    if (nextErrors.message || nextErrors.label) return;
    // The user can leave the form while the session starts; the redirect
    // then must not pull them away from the page they went to.
    const stillHere = isCurrentVisit;
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
      if (stillHere()) {
        navigate(sessionPath(organizationId, res.session!.sessionId));
      }
    } catch {
      // The form shows the mutation error.
    }
  };

  return (
    <>
      <PageHeader
        title={t("nav.newSession")}
        description={t("newSession.description")}
      />
      <QueryBoundary query={profiles} loading={t("profiles.loading")}>
        {() =>
          selectable.length === 0 ? (
            <NoProfileNotice
              canManage={canManageOrganization(role)}
              organizationId={organizationId}
            />
          ) : (
            <form className="form form-page" onSubmit={submit} noValidate>
              <Textarea
                label={t("newSession.firstMessage")}
                description={t("newSession.firstMessageHelp")}
                value={message}
                onValueChange={(v: string) => {
                  setMessage(v);
                  setErrors((e) => ({ ...e, message: undefined }));
                }}
                error={
                  errors.message === undefined ? undefined : t(errors.message)
                }
                autoFocus
                autoResize
                rows={4}
              />
              <Select
                label={t("newSession.agent")}
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
                    {vendorName(v, t)}
                  </SelectOption>
                ))}
              </Select>
              {/* A failed credential read is not a missing credential: show
                  the failure and its retry instead of the notice. */}
              <QueryBoundary
                query={credentials}
                loading={t("credentials.loading")}
              >
                {() =>
                  hasCredential ? null : (
                    <div className="notice notice-warning" role="alert">
                      <p>
                        {t("newSession.noCredential", {
                          vendor: vendorName(vendor, t),
                        })}
                      </p>
                      <Link to={orgPath(RouteId.SettingsUser, organizationId)}>
                        {t("newSession.addCredential")}
                      </Link>
                    </div>
                  )
                }
              </QueryBoundary>
              <Select
                label={t("newSession.runnerProfile")}
                description={
                  selectedProfile === undefined
                    ? undefined
                    : profileSummary(selectedProfile, i18n)
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
                <Loading label={t("newSession.loadingOptions")} />
              ) : null}
              {modes.length > 0 ? (
                <Select
                  label={t("newSession.mode")}
                  value={modeId}
                  onValueChange={(v) => setModeId(v ?? AGENT_DEFAULT)}
                >
                  <SelectOption value={AGENT_DEFAULT}>
                    {t("chat.agentDefaultMode")}
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
                    {t("chat.agentDefaultMode")}
                  </SelectOption>
                  {o.values.map((value) => (
                    <SelectOption key={value.valueId} value={value.valueId}>
                      {value.name}
                    </SelectOption>
                  ))}
                </Select>
              ))}
              <TextField
                label={t("newSession.label")}
                description={t("newSession.labelHelp")}
                value={label}
                onValueChange={(v: string) => {
                  setLabel(v);
                  setErrors((e) => ({ ...e, label: undefined }));
                }}
                error={
                  errors.label === undefined
                    ? undefined
                    : t(errors.label, { limit: LABEL_LIMIT })
                }
                autoComplete="off"
              />
              <fieldset className="fieldset">
                <legend>{t("secrets.title")}</legend>
                <p className="muted">{t("newSession.secretsHelp")}</p>
                <QueryBoundary query={secrets} loading={t("secrets.loading")}>
                  {(list) =>
                    list.length === 0 ? (
                      <p className="muted">
                        {t("newSession.noSecrets")}{" "}
                        <Link
                          to={orgPath(RouteId.SettingsUser, organizationId)}
                        >
                          {t("newSession.addSecret")}
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
                  {t("newSession.start")}
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
  const { t } = useI18n();
  return (
    <div className="empty">
      <p className="empty-title">{t("newSession.noProfile.title")}</p>
      {canManage ? (
        <p className="muted">
          {t("newSession.noProfile.manager")}{" "}
          <Link to={orgPath(RouteId.SettingsOrg, organizationId)}>
            {t("newSession.noProfile.goToSettings")}
          </Link>
        </p>
      ) : (
        <p className="muted">{t("newSession.noProfile.member")}</p>
      )}
    </div>
  );
}
