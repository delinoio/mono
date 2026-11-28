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
} from "../format.ts";
import { useVendorCredentials } from "../queries.ts";
import { useOrg } from "./shell.tsx";
import { ConfirmDialog, FormDialog, QueryBoundary } from "./ui.tsx";

/** The largest Codex auth.json that the control plane accepts. */
export const MAX_AUTH_JSON_BYTES = 64 * 1024;

/** Help text for each credential kind. */
const KIND_HELP: Record<VendorCredentialKind, string> = {
  [VendorCredentialKind.UNSPECIFIED]: "",
  [VendorCredentialKind.API_KEY]: "An API key of the vendor.",
  [VendorCredentialKind.CLAUDE_OAUTH_TOKEN]:
    "The token that `claude setup-token` prints. It is valid for one year.",
  [VendorCredentialKind.CODEX_AUTH_JSON]:
    "The auth.json file of a ChatGPT sign-in, usually ~/.codex/auth.json. Codex refreshes it during sessions.",
};

/**
 * Checks a Codex auth.json file: a JSON object of at most 64 KiB. Returns an
 * error message, or undefined when the file is valid.
 */
export function authJsonError(bytes: Uint8Array): string | undefined {
  if (bytes.byteLength > MAX_AUTH_JSON_BYTES) {
    return "The file is larger than 64 KiB.";
  }
  try {
    const value: unknown = JSON.parse(new TextDecoder().decode(bytes));
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      return "The file is not a JSON object.";
    }
  } catch {
    return "The file is not valid JSON.";
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
      <QueryBoundary query={credentials} loading="Loading vendor credentials">
        {(list) => (
          <ul
            className="list"
            aria-label={
              organizationScope
                ? "Organization vendor credentials"
                : "Your vendor credentials"
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
                    <span className="list-title">{labels.vendor[vendor]}</span>
                    <span className="muted">
                      {own === undefined
                        ? organizationScope
                          ? "Not set. Members need their own credential."
                          : organization === undefined
                            ? "Not set. The organization has no credential either."
                            : "Not set. Your sessions use the organization credential."
                        : `${labels.credentialKind[own.kind]} · set ${formatTime(own.valueSetAt)}${
                            own.updatedBy ===
                            CredentialUpdateSource.AGENT_REFRESH
                              ? " by an agent refresh"
                              : ""
                          }`}
                    </span>
                  </div>
                  <div className="row-actions">
                    <Button
                      size={ButtonSize.Small}
                      variant={ButtonVariant.Secondary}
                      onClick={() => setEditing(vendor)}
                      aria-label={`${own === undefined ? "Set" : "Replace"} the ${labels.vendor[vendor]} credential`}
                    >
                      {own === undefined ? "Set" : "Replace"}
                    </Button>
                    {own !== undefined ? (
                      <Button
                        size={ButtonSize.Small}
                        variant={ButtonVariant.Danger}
                        onClick={() => setDeleting(own)}
                        aria-label={`Delete the ${labels.vendor[vendor]} credential`}
                      >
                        Delete
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
        title={`Delete the ${deleting === undefined ? "" : labels.vendor[deleting.vendor]} credential?`}
        description={
          organizationScope
            ? "New sessions of members without their own credential cannot start until an owner or admin sets a new one. Running sessions keep the value they started with."
            : "New sessions use the organization credential, if it has one. Running sessions keep the value they started with."
        }
        confirmLabel="Delete credential"
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
  const set = useMutation(CredentialService.method.setVendorCredential, {
    onSuccess: onSaved,
  });
  const kinds = vendor === undefined ? [] : VENDOR_CREDENTIAL_KINDS[vendor];
  const [kind, setKind] = useState<VendorCredentialKind>();
  const [value, setValue] = useState("");
  const [file, setFile] = useState<{ name: string; bytes: Uint8Array }>();
  const [error, setError] = useState<string>();
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
      title={`${vendor === undefined ? "" : labels.vendor[vendor]} credential`}
      description="SandCrew stores the value encrypted and never shows it again. Code in a session sandbox can read the credential of its session."
      submitLabel="Save credential"
      onSubmit={async (opening) => {
        if (vendor === undefined || selected === undefined) return;
        let bytes: Uint8Array;
        if (isFile) {
          if (file === undefined) {
            setError("Choose the auth.json file.");
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
            setError("Enter the value.");
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
        // The value never stays in the form after it is saved.
        reset();
        if (opening.isCurrent()) onClose();
      }}
    >
      {kinds.length > 1 ? (
        <Select
          label="Kind"
          description={selected === undefined ? undefined : KIND_HELP[selected]}
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
              {labels.credentialKind[k]}
            </SelectOption>
          ))}
        </Select>
      ) : null}
      {isFile ? (
        <div className="file-field">
          <label htmlFor="codex-auth-json">auth.json file</label>
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
            <p className="muted">Selected: {file.name}</p>
          ) : null}
          {error !== undefined ? (
            <p className="field-error" role="alert">
              {error}
            </p>
          ) : null}
        </div>
      ) : (
        <PasswordInput
          label={
            selected === undefined ? "Value" : labels.credentialKind[selected]
          }
          toggleLabel="Show value"
          value={value}
          onValueChange={(v: string) => {
            setValue(v);
            setError(undefined);
          }}
          error={error}
          autoComplete="off"
          autoFocus
        />
      )}
    </FormDialog>
  );
}
