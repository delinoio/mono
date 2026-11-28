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
import { usePushSubscriptions, useSessionSecrets } from "../queries.ts";
import {
  browserLabel,
  type PushEnvironment,
  PushOptInError,
  PushSupport,
  browserPushEnvironment,
  pushSupport,
  subscribeToPush,
} from "../web-push.ts";

/** Valid session secret names. */
export const SECRET_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

/** The largest session secret value, in bytes. */
export const MAX_SECRET_VALUE_BYTES = 32 * 1024;

/** Checks a session secret name. Returns an error message or undefined. */
export function secretNameError(name: string): string | undefined {
  if (!SECRET_NAME_PATTERN.test(name)) {
    return "Use 1 to 128 letters, digits and underscores, starting with a letter or an underscore.";
  }
  if (name.toUpperCase().startsWith("SANDCREW_")) {
    return "Names that start with SANDCREW_ are reserved.";
  }
  return undefined;
}

/** Route `SETTINGS_USER`. */
export function UserSettingsPage({
  pushEnvironment,
}: {
  /** Tests pass fake browser APIs. */
  pushEnvironment?: PushEnvironment;
}) {
  return (
    <>
      <PageHeader
        title="Settings"
        description="Your vendor credentials, session secrets and notifications in this organization. SandCrew never shows a saved value again."
      />
      <section className="section" aria-labelledby="credentials-heading">
        <h2 id="credentials-heading">Your vendor credentials</h2>
        <p className="muted">
          Your credential replaces the organization credential of the same
          vendor for your sessions. Code in a session sandbox can read the
          credential of its session.
        </p>
        <VendorCredentialSection scope={CredentialScope.USER} />
      </section>
      <SessionSecretsSection />
      <NotificationsSection pushEnvironment={pushEnvironment} />
    </>
  );
}

function SessionSecretsSection() {
  const { organizationId } = useOrg();
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
  const [errors, setErrors] = useState<{ name?: string; value?: string }>({});
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
          <h2 id="secrets-heading">Session secrets</h2>
          <p className="muted">
            Environment variables that you can give to your sessions, for
            example a GitHub token. The new-session form selects them.
          </p>
        </div>
        <Button
          variant={ButtonVariant.Secondary}
          onClick={() => setEditing(null)}
        >
          Add secret
        </Button>
      </div>
      <QueryBoundary query={secrets} loading="Loading session secrets">
        {(list) =>
          list.length === 0 ? (
            <Empty title="No session secrets" />
          ) : (
            <ul className="list" aria-label="Session secrets">
              {list.map((s) => (
                <li key={s.sessionSecretId} className="list-row">
                  <div className="list-main">
                    <code className="list-title">{s.name}</code>
                    <span className="muted">
                      Value set {formatTime(s.valueSetAt)}
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
                      aria-label={`Replace the value of ${s.name}`}
                    >
                      Replace
                    </Button>
                    <Button
                      size={ButtonSize.Small}
                      variant={ButtonVariant.Danger}
                      onClick={() => setDeleting(s)}
                      aria-label={`Delete ${s.name}`}
                    >
                      Delete
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
        title={editing ? `Replace ${editing.name}` : "Add a session secret"}
        description="SandCrew stores the value encrypted and never shows it again."
        submitLabel="Save secret"
        onSubmit={async (opening) => {
          const trimmedName = name.trim();
          const nextErrors = {
            name: secretNameError(trimmedName),
            value:
              value === ""
                ? "Enter the value."
                : new TextEncoder().encode(value).byteLength >
                    MAX_SECRET_VALUE_BYTES
                  ? "The value is larger than 32 KiB."
                  : undefined,
          };
          setErrors(nextErrors);
          if (nextErrors.name || nextErrors.value) return;
          await set.mutateAsync({ organizationId, name: trimmedName, value });
          // The value never stays in the form after it is saved.
          setValue("");
          if (opening.isCurrent()) close();
        }}
      >
        {editing ? null : (
          <TextField
            label="Name"
            description="The environment variable name, for example GITHUB_TOKEN."
            value={name}
            onValueChange={(v: string) => {
              setName(v);
              setErrors((e) => ({ ...e, name: undefined }));
            }}
            error={errors.name}
            autoComplete="off"
            autoFocus
          />
        )}
        <PasswordInput
          label="Value"
          toggleLabel="Show value"
          value={value}
          onValueChange={(v: string) => {
            setValue(v);
            setErrors((e) => ({ ...e, value: undefined }));
          }}
          error={errors.value}
          autoComplete="off"
          autoFocus={editing !== null}
        />
      </FormDialog>
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={`Delete ${deleting?.name ?? "this secret"}?`}
        description="New sessions cannot select it. Running sessions keep it until their agent starts again."
        confirmLabel="Delete secret"
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
  const [optInMessage, setOptInMessage] = useState<string>();
  const [pending, setPending] = useState(false);
  const [deleting, setDeleting] = useState<PushSubscription>();

  const turnOn = async () => {
    if (pending) return;
    setPending(true);
    setError(undefined);
    setOptInMessage(undefined);
    try {
      const config = await callUnaryMethod(
        transport,
        NotificationService.method.getWebPushConfig,
        {},
      );
      const keys = await subscribeToPush(config.vapidPublicKey, env);
      await register.mutateAsync({
        ...keys,
        label: browserLabel(navigator.userAgent).slice(0, 100),
      });
      toast.success("Notifications are on for this browser");
    } catch (e) {
      if (e instanceof PushOptInError) {
        setOptInMessage(e.message);
      } else {
        setError(e);
      }
    } finally {
      setPending(false);
    }
  };

  return (
    <section className="section" aria-labelledby="notifications-heading">
      <h2 id="notifications-heading">Notifications</h2>
      <p className="muted">
        SandCrew notifies you when an agent waits for your input, asks for
        permission, or when a session ends unexpectedly. It does not notify you
        about a session that you have open.
      </p>
      {support === PushSupport.Unsupported ? (
        <p className="notice" role="status">
          This browser does not support notifications.
        </p>
      ) : support === PushSupport.Denied ? (
        <p className="notice" role="status">
          Notifications are blocked for this site. Allow them in the browser
          settings to turn them on.
        </p>
      ) : (
        <Button onClick={turnOn} disabled={pending}>
          Turn on notifications on this browser
        </Button>
      )}
      {optInMessage !== undefined ? (
        <p className="notice notice-warning" role="alert">
          {optInMessage}
        </p>
      ) : null}
      {error !== undefined ? <ErrorNotice error={error} /> : null}
      <h3>Browsers with notifications</h3>
      <QueryBoundary
        query={subscriptions}
        loading="Loading notification subscriptions"
      >
        {(list) =>
          list.length === 0 ? (
            <Empty title="No browser gets notifications" />
          ) : (
            <ul className="list" aria-label="Notification subscriptions">
              {list.map((s) => (
                <li key={s.pushSubscriptionId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">{s.label || "Browser"}</span>
                    <span className="muted">
                      Added {formatTime(s.createdAt)}
                      {s.endpointOrigin ? ` · ${s.endpointOrigin}` : ""}
                    </span>
                  </div>
                  <Button
                    size={ButtonSize.Small}
                    variant={ButtonVariant.Danger}
                    onClick={() => setDeleting(s)}
                    aria-label={`Turn off notifications for ${s.label || "this browser"}`}
                  >
                    Turn off
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
        title={`Turn off notifications for ${deleting?.label || "this browser"}?`}
        description="SandCrew stops sending notifications to that browser."
        confirmLabel="Turn off"
        onConfirm={() =>
          remove.mutateAsync({
            pushSubscriptionId: deleting!.pushSubscriptionId,
          })
        }
      />
    </section>
  );
}
