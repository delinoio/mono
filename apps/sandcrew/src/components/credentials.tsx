// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import {
  type AgentVendor,
  CredentialScope,
  CredentialUpdateSource,
  VendorCredentialKind,
} from "@delino/sandcrew-protos/v1/common_pb";
import {
  CredentialService,
  type VendorCredential,
} from "@delino/sandcrew-protos/v1/credential_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { PasswordInput } from "delino-ui/password-input";
import { Select, SelectOption } from "delino-ui/select";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import {
  formatTime,
  labels,
  VENDOR_CREDENTIAL_KINDS,
  VENDORS,
  vendorName,
} from "../format.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { useVendorCredentials } from "../queries.ts";
import { useOrg } from "./shell.tsx";
import { ConfirmDialog, FormDialog, QueryBoundary } from "./ui.tsx";

/** The largest Codex auth.json that the control plane accepts. */
export const MAX_AUTH_JSON_BYTES = 64 * 1024;

/** Catalog keys of the help text of each credential kind. */
const KIND_HELP: Partial<Record<VendorCredentialKind, MessageKey>> = {
  [VendorCredentialKind.API_KEY]: "credentials.help.apiKey",
  [VendorCredentialKind.CLAUDE_OAUTH_TOKEN]:
    "credentials.help.claudeOauthToken",
  [VendorCredentialKind.CODEX_AUTH_JSON]: "credentials.help.codexAuthJson",
};

/**
 * Checks a Codex auth.json file: a JSON object of at most 64 KiB. Returns
 * the catalog key of an error message, or undefined when the file is valid.
 */
export function authJsonError(bytes: Uint8Array): MessageKey | undefined {
  if (bytes.byteLength > MAX_AUTH_JSON_BYTES) {
    return "credentials.error.fileTooLarge";
  }
  try {
    const value: unknown = JSON.parse(new TextDecoder().decode(bytes));
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      return "credentials.error.notObject";
    }
  } catch {
    return "credentials.error.notJson";
  }
  return undefined;
}

/** The credential of a scope and vendor, if any. */
export function credentialFor(
  credentials: readonly VendorCredential[],
  scope: CredentialScope,
  vendor: AgentVendor,
  userId: string | undefined,
): VendorCredential | undefined {
  return credentials.find(
    (c) =>
      c.scope === scope &&
      c.vendor === vendor &&
      (scope === CredentialScope.ORGANIZATION || c.ownerUserId === userId),
  );
}

/**
 * The vendor credentials of one scope: the user's own credentials or the
 * organization credentials. Values are write-only: the section shows only
 * the kind and the times.
 */
export function VendorCredentialSection({ scope }: { scope: CredentialScope }) {
  const { organizationId, userId } = useOrg();
  const { t, lang } = useI18n();
  const queryClient = useQueryClient();
  const credentials = useVendorCredentials(organizationId);
  const refresh = () => invalidateServices(queryClient, CredentialService);
  const remove = useMutation(CredentialService.method.deleteVendorCredential, {
    onSuccess: refresh,
  });
  const [editing, setEditing] = useState<AgentVendor>();
  const [deleting, setDeleting] = useState<VendorCredential>();
  const organizationScope = scope === CredentialScope.ORGANIZATION;

  return (
    <>
      <QueryBoundary query={credentials} loading={t("credentials.loading")}>
        {(list) => (
          <ul
            className="list"
            aria-label={
              organizationScope
                ? t("credentials.organizationList")
                : t("credentials.userList")
            }
          >
            {VENDORS.map((vendor) => {
              const own = credentialFor(list, scope, vendor, userId);
              const organization = organizationScope
                ? undefined
                : credentialFor(
                    list,
                    CredentialScope.ORGANIZATION,
                    vendor,
                    userId,
                  );
              return (
                <li key={vendor} className="list-row">
                  <div className="list-main">
                    <span className="list-title">{vendorName(vendor, t)}</span>
                    <span className="muted">
                      {own === undefined
                        ? organizationScope
                          ? t("credentials.notSet.organization")
                          : organization === undefined
                            ? t("credentials.notSet.none")
                            : t("credentials.notSet.usesOrganization")
                        : t(
                            own.updatedBy ===
                              CredentialUpdateSource.AGENT_REFRESH
                              ? "credentials.setByAgentRefresh"
                              : "credentials.set",
                            {
                              kind: t(labels.credentialKind[own.kind]),
                              time: formatTime(own.valueSetAt, lang),
                            },
                          )}
                    </span>
                  </div>
                  <div className="row-actions">
                    <Button
                      size={ButtonSize.Small}
                      variant={ButtonVariant.Secondary}
                      onClick={() => setEditing(vendor)}
                      aria-label={t(
                        own === undefined
                          ? "credentials.setLabel"
                          : "credentials.replaceLabel",
                        { vendor: vendorName(vendor, t) },
                      )}
                    >
                      {own === undefined
                        ? t("credentials.setButton")
                        : t("credentials.replaceButton")}
                    </Button>
                    {own !== undefined ? (
                      <Button
                        size={ButtonSize.Small}
                        variant={ButtonVariant.Danger}
                        onClick={() => setDeleting(own)}
                        aria-label={t("credentials.deleteLabel", {
                          vendor: vendorName(vendor, t),
                        })}
                      >
                        {t("credentials.deleteButton")}
                      </Button>
                    ) : null}
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </QueryBoundary>
      <CredentialDialog
        scope={scope}
        vendor={editing}
        onClose={() => setEditing(undefined)}
        onSaved={refresh}
      />
      <ConfirmDialog
        open={deleting !== undefined}
        onOpenChange={(open) => !open && setDeleting(undefined)}
        title={t("credentials.deleteTitle", {
          vendor: deleting === undefined ? "" : vendorName(deleting.vendor, t),
        })}
        description={
          organizationScope
            ? t("credentials.deleteOrganizationDescription")
            : t("credentials.deleteUserDescription")
        }
        confirmLabel={t("credentials.deleteConfirm")}
        onConfirm={() =>
          remove.mutateAsync({
            organizationId,
            vendorCredentialId: deleting!.vendorCredentialId,
          })
        }
      />
    </>
  );
}

function CredentialDialog({
  scope,
  vendor,
  onClose,
  onSaved,
}: {
  scope: CredentialScope;
  vendor: AgentVendor | undefined;
  onClose: () => void;
  onSaved: () => unknown;
}) {
  const { organizationId } = useOrg();
  const { t } = useI18n();
  const set = useMutation(CredentialService.method.setVendorCredential, {
    onSuccess: onSaved,
  });
  const kinds = vendor === undefined ? [] : VENDOR_CREDENTIAL_KINDS[vendor];
  const [kind, setKind] = useState<VendorCredentialKind>();
  const [value, setValue] = useState("");
  const [file, setFile] = useState<{ name: string; bytes: Uint8Array }>();
  const [error, setError] = useState<MessageKey>();
  const selected = kind !== undefined && kinds.includes(kind) ? kind : kinds[0];
  const isFile = selected === VendorCredentialKind.CODEX_AUTH_JSON;

  const reset = () => {
    setKind(undefined);
    setValue("");
    setFile(undefined);
    setError(undefined);
  };

  return (
    <FormDialog
      open={vendor !== undefined}
      onOpenChange={(open) => {
        if (!open) {
          reset();
          onClose();
        }
      }}
      title={t("credentials.dialogTitle", {
        vendor: vendor === undefined ? "" : vendorName(vendor, t),
      })}
      description={t("credentials.dialogDescription")}
      submitLabel={t("credentials.save")}
      onSubmit={async (opening) => {
        if (vendor === undefined || selected === undefined) return;
        let bytes: Uint8Array;
        if (isFile) {
          if (file === undefined) {
            setError("credentials.error.chooseFile");
            return;
          }
          const fileError = authJsonError(file.bytes);
          if (fileError !== undefined) {
            setError(fileError);
            return;
          }
          bytes = file.bytes;
        } else {
          const trimmed = value.trim();
          if (trimmed === "") {
            setError("credentials.error.enterValue");
            return;
          }
          bytes = new TextEncoder().encode(trimmed);
        }
        await set.mutateAsync({
          organizationId,
          scope,
          vendor,
          kind: selected,
          value: bytes,
        });
        // The value never stays in the form after it is saved. A dismissal
        // already cleared it, and a later opening keeps its own draft.
        if (opening.isCurrent()) {
          reset();
          onClose();
        }
      }}
    >
      {kinds.length > 1 ? (
        <Select
          label={t("credentials.kind")}
          description={
            selected === undefined || KIND_HELP[selected] === undefined
              ? undefined
              : t(KIND_HELP[selected])
          }
          value={selected === undefined ? null : String(selected)}
          onValueChange={(v) => {
            if (v === null) return;
            setKind(Number(v) as VendorCredentialKind);
            setValue("");
            setFile(undefined);
            setError(undefined);
          }}
        >
          {kinds.map((k) => (
            <SelectOption key={k} value={String(k)}>
              {t(labels.credentialKind[k])}
            </SelectOption>
          ))}
        </Select>
      ) : null}
      {isFile ? (
        <div className="file-field">
          <label htmlFor="codex-auth-json">
            {t("credentials.authJsonFile")}
          </label>
          <input
            id="codex-auth-json"
            type="file"
            accept="application/json,.json"
            onChange={(event) => {
              const chosen = event.target.files?.[0];
              setError(undefined);
              if (chosen === undefined) {
                setFile(undefined);
                return;
              }
              void chosen.arrayBuffer().then((buffer) => {
                setFile({ name: chosen.name, bytes: new Uint8Array(buffer) });
              });
              // Clear the input so the browser keeps no reference.
              event.target.value = "";
            }}
          />
          {file !== undefined ? (
            <p className="muted">
              {t("credentials.selectedFile", { name: file.name })}
            </p>
          ) : null}
          {error !== undefined ? (
            <p className="field-error" role="alert">
              {t(error)}
            </p>
          ) : null}
        </div>
      ) : (
        <PasswordInput
          label={
            selected === undefined
              ? t("credentials.value")
              : t(labels.credentialKind[selected])
          }
          toggleLabel={t("credentials.showValue")}
          value={value}
          onValueChange={(v: string) => {
            setValue(v);
            setError(undefined);
          }}
          error={error === undefined ? undefined : t(error)}
          autoComplete="off"
          autoFocus
        />
      )}
    </FormDialog>
  );
}
