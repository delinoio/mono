// SPDX-License-Identifier: Apache-2.0
import {
  callUnaryMethod,
  useMutation,
  useTransport,
} from "@connectrpc/connect-query";
import { CredentialScope } from "@delino/sandcrew-protos/v1/common_pb";
import {
  CredentialService,
  type SessionSecret,
} from "@delino/sandcrew-protos/v1/credential_pb";
import {
  NotificationService,
  type PushSubscription,
} from "@delino/sandcrew-protos/v1/notification_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { PasswordInput } from "delino-ui/password-input";
import { TextField } from "delino-ui/text-field";
import { toast } from "delino-ui/toast";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { VendorCredentialSection } from "../components/credentials.tsx";
import { useOrg } from "../components/shell.tsx";
import {
  ConfirmDialog,
  Empty,
  ErrorNotice,
  FormDialog,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { formatTime } from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { usePushSubscriptions, useSessionSecrets } from "../queries.ts";
import {
  browserLabel,
  type PushEnvironment,
  PushOptInError,
  PushOptInFailure,
  PushSupport,
  browserPushEnvironment,
  pushSupport,
  subscribeToPush,
} from "../web-push.ts";

/** Valid session secret names. */
export const SECRET_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

/** The largest session secret value, in bytes. */
export const MAX_SECRET_VALUE_BYTES = 32 * 1024;

/**
 * Checks a session secret name. Returns the catalog key of an error message,
 * or undefined.
 */
export function secretNameError(name: string): MessageKey | undefined {
  if (!SECRET_NAME_PATTERN.test(name)) {
    return "secrets.error.namePattern";
  }
  if (name.toUpperCase().startsWith("SANDCREW_")) {
    return "secrets.error.nameReserved";
  }
  return undefined;
}

/** Catalog keys of the opt-in failures that the user can act on. */
const PUSH_OPT_IN_MESSAGES: Record<PushOptInFailure, MessageKey> = {
  [PushOptInFailure.Unsupported]: "notifications.unsupported",
  [PushOptInFailure.PermissionNotGranted]: "notifications.notGranted",
};

/** Route `SETTINGS_USER`. */
export function UserSettingsPage({
  pushEnvironment,
}: {
  /** Tests pass fake browser APIs. */
  pushEnvironment?: PushEnvironment;
}) {
  const { t } = useI18n();
  return (
    <>
      <PageHeader
        title={t("nav.settings")}
        description={t("userSettings.description")}
      />
      <section className="section" aria-labelledby="credentials-heading">
        <h2 id="credentials-heading">{t("credentials.userList")}</h2>
        <p className="muted">{t("userSettings.credentialsHelp")}</p>
        <VendorCredentialSection scope={CredentialScope.USER} />
      </section>
      <SessionSecretsSection />
      <NotificationsSection pushEnvironment={pushEnvironment} />
    </>
  );
}

function SessionSecretsSection() {
  const { organizationId } = useOrg();
  const { t, lang } = useI18n();
  const queryClient = useQueryClient();
  const secrets = useSessionSecrets(organizationId);
  const refresh = () => invalidateServices(queryClient, CredentialService);
  const set = useMutation(CredentialService.method.setSessionSecret, {
    onSuccess: refresh,
  });
  const remove = useMutation(CredentialService.method.deleteSessionSecret, {
    onSuccess: refresh,
  });
  /** Undefined: closed. A secret: replace its value. Null: add a secret. */
  const [editing, setEditing] = useState<SessionSecret | null>();
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [errors, setErrors] = useState<{
    name?: MessageKey;
    value?: MessageKey;
  }>({});
  const [deleting, setDeleting] = useState<SessionSecret>();

  const close = () => {
    setEditing(undefined);
    setName("");
    setValue("");
    setErrors({});
  };

  return (
    <section className="section" aria-labelledby="secrets-heading">
      <div className="section-header">
        <div>
          <h2 id="secrets-heading">{t("secrets.title")}</h2>
          <p className="muted">{t("secrets.description")}</p>
        </div>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setEditing(null)}
        >
          {t("secrets.add")}
        </Button>
      </div>
      <QueryBoundary query={secrets} loading={t("secrets.loading")}>
        {(list) =>
          list.length === 0 ? (
            <Empty title={t("secrets.empty")} />
          ) : (
            <ul className="list" aria-label={t("secrets.title")}>
              {list.map((s) => (
                <li key={s.sessionSecretId} className="list-row">
                  <div className="list-main">
                    <code className="list-title">{s.name}</code>
                    <span className="muted">
                      {t("secrets.valueSet", {
                        time: formatTime(s.valueSetAt, lang),
                      })}
                    </span>
                  </div>
                  <div className="row-actions">
                    <Button
                      size={ButtonSize.Small}
                      variant={ButtonVariant.Secondary}
                      onClick={() => {
                        setEditing(s);
                        setName(s.name);
                      }}
                      aria-label={t("secrets.replaceLabel", { name: s.name })}
                    >
                      {t("credentials.replaceButton")}
                    </Button>
                    <Button
                      size={ButtonSize.Small}
                      variant={ButtonVariant.Danger}
                      onClick={() => setDeleting(s)}
                      aria-label={t("secrets.deleteLabel", { name: s.name })}
                    >
                      {t("credentials.deleteButton")}
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )
        }
      </QueryBoundary>
      <FormDialog
        open={editing !== undefined}
        onOpenChange={(open) => !open && close()}
        title={
          editing
            ? t("secrets.replaceTitle", { name: editing.name })
            : t("secrets.addTitle")
        }
        description={t("secrets.dialogDescription")}
        submitLabel={t("secrets.save")}
        onSubmit={async (opening) => {
          const trimmedName = name.trim();
          const nextErrors = {
            name: secretNameError(trimmedName),
            value:
              value === ""
                ? ("credentials.error.enterValue" as const)
                : new TextEncoder().encode(value).byteLength >
                    MAX_SECRET_VALUE_BYTES
                  ? ("secrets.error.valueTooLarge" as const)
                  : undefined,
          };
          setErrors(nextErrors);
          if (nextErrors.name || nextErrors.value) return;
          await set.mutateAsync({ organizationId, name: trimmedName, value });
          // The value never stays in the form after it is saved. A dismissal
          // already cleared it, and a later opening keeps its own draft.
          if (opening.isCurrent()) close();
        }}
      >
        {editing ? null : (
          <TextField
            label={t("secrets.name")}
            description={t("secrets.nameHelp")}
            value={name}
            onValueChange={(v: string) => {
              setName(v);
              setErrors((e) => ({ ...e, name: undefined }));
            }}
            error={errors.name === undefined ? undefined : t(errors.name)}
            autoComplete="off"
            autoFocus
          />
        )}
        <PasswordInput
          label={t("credentials.value")}
          toggleLabel={t("credentials.showValue")}
          value={value}
          onValueChange={(v: string) => {
            setValue(v);
            setErrors((e) => ({ ...e, value: undefined }));
          }}
          error={errors.value === undefined ? undefined : t(errors.value)}
          autoComplete="off"
          autoFocus={editing !== null}
        />
      </FormDialog>
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={t("secrets.deleteTitle", {
          name: deleting?.name ?? t("secrets.thisSecret"),
        })}
        description={t("secrets.deleteDescription")}
        confirmLabel={t("secrets.deleteConfirm")}
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            sessionSecretId: deleting!.sessionSecretId,
          })
        }
      />
    </section>
  );
}

function NotificationsSection({
  pushEnvironment,
}: {
  pushEnvironment?: PushEnvironment;
}) {
  const transport = useTransport();
  const { t, lang } = useI18n();
  const queryClient = useQueryClient();
  const [env] = useState(() => pushEnvironment ?? browserPushEnvironment());
  const support = pushSupport(env);
  const subscriptions = usePushSubscriptions();
  const refresh = () => invalidateServices(queryClient, NotificationService);
  const register = useMutation(
    NotificationService.method.registerPushSubscription,
    { onSuccess: refresh },
  );
  const remove = useMutation(
    NotificationService.method.deletePushSubscription,
    {
      onSuccess: refresh,
    },
  );
  const [error, setError] = useState<unknown>();
  const [optInMessage, setOptInMessage] = useState<MessageKey>();
  const [pending, setPending] = useState(false);
  const [deleting, setDeleting] = useState<PushSubscription>();

  const turnOn = async () => {
    if (pending) return;
    setPending(true);
    setError(undefined);
    setOptInMessage(undefined);
    try {
      const keys = await subscribeToPush(async () => {
        const config = await callUnaryMethod(
          transport,
          NotificationService.method.getWebPushConfig,
          {},
        );
        return config.vapidPublicKey;
      }, env);
      await register.mutateAsync({
        ...keys,
        label: browserLabel(navigator.userAgent).slice(0, 100),
      });
      toast.success(t("notifications.turnedOn"));
    } catch (e) {
      if (e instanceof PushOptInError) {
        setOptInMessage(PUSH_OPT_IN_MESSAGES[e.failure]);
      } else {
        setError(e);
      }
    } finally {
      setPending(false);
    }
  };

  return (
    <section className="section" aria-labelledby="notifications-heading">
      <h2 id="notifications-heading">{t("notifications.title")}</h2>
      <p className="muted">{t("notifications.description")}</p>
      {support === PushSupport.Unsupported ? (
        <p className="notice" role="status">
          {t("notifications.unsupported")}
        </p>
      ) : support === PushSupport.Denied ? (
        <p className="notice" role="status">
          {t("notifications.blocked")}
        </p>
      ) : (
        <Button onClick={turnOn} disabled={pending}>
          {t("notifications.turnOn")}
        </Button>
      )}
      {optInMessage !== undefined ? (
        <p className="notice notice-warning" role="alert">
          {t(optInMessage)}
        </p>
      ) : null}
      {error !== undefined ? <ErrorNotice error={error} /> : null}
      <h3>{t("notifications.browsers")}</h3>
      <QueryBoundary query={subscriptions} loading={t("notifications.loading")}>
        {(list) =>
          list.length === 0 ? (
            <Empty title={t("notifications.empty")} />
          ) : (
            <ul className="list" aria-label={t("notifications.list")}>
              {list.map((s) => (
                <li key={s.pushSubscriptionId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      {s.label || t("notifications.browser")}
                    </span>
                    <span className="muted">
                      {t("notifications.added", {
                        time: formatTime(s.createdAt, lang),
                      })}
                      {s.endpointOrigin ? ` · ${s.endpointOrigin}` : ""}
                    </span>
                  </div>
                  <Button
                    size={ButtonSize.Small}
                    variant={ButtonVariant.Danger}
                    onClick={() => setDeleting(s)}
                    aria-label={t("notifications.turnOffLabel", {
                      browser: s.label || t("notifications.thisBrowser"),
                    })}
                  >
                    {t("notifications.turnOff")}
                  </Button>
                </li>
              ))}
            </ul>
          )
        }
      </QueryBoundary>
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={t("notifications.turnOffTitle", {
          browser: deleting?.label || t("notifications.thisBrowser"),
        })}
        description={t("notifications.turnOffDescription")}
        confirmLabel={t("notifications.turnOff")}
        onConfirm={() =>
          remove.mutateAsync({
            pushSubscriptionId: deleting!.pushSubscriptionId,
          })
        }
      />
    </section>
  );
}
