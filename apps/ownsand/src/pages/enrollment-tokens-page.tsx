// SPDX-License-Identifier: Apache-2.0
import {
  createConnectQueryKey,
  useInfiniteQuery,
  useMutation,
} from "@connectrpc/connect-query";
import { EnrollmentTokenState } from "@delino/ownsand-sdk/gen/common_pb";
import {
  DeviceService,
  type EnrollmentToken,
} from "@delino/ownsand-sdk/gen/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import {
  Dialog,
  DialogActions,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "delino-ui/dialog";
import { Select, SelectOption } from "delino-ui/select";
import { TextField } from "delino-ui/text-field";
import { useState, type FormEvent } from "react";
import { errorMessage } from "../api/errors.ts";
import { PAGE_SIZE, pageOptions } from "../api/paging.ts";
import {
  EmptyState,
  ErrorState,
  LoadMore,
  LoadingState,
  PageHeader,
  formText,
} from "../components/common.tsx";
import { ConfirmDialog } from "../components/confirm-dialog.tsx";
import { SecretDialog } from "../components/secret-dialog.tsx";
import { formatEnum, formatTime } from "../format.ts";
import type { OrganizationPageProps } from "./organization-layout.tsx";

/** Lifetimes offered for a new enrollment token (at most 24 hours). */
export enum TokenLifetime {
  OneHour = "1h",
  SixHours = "6h",
  OneDay = "24h",
}

const LIFETIME_SECONDS: Readonly<Record<TokenLifetime, bigint>> = {
  [TokenLifetime.OneHour]: 3600n,
  [TokenLifetime.SixHours]: 21600n,
  [TokenLifetime.OneDay]: 86400n,
};

const LIFETIME_LABELS: Readonly<Record<TokenLifetime, string>> = {
  [TokenLifetime.OneHour]: "1 hour",
  [TokenLifetime.SixHours]: "6 hours",
  [TokenLifetime.OneDay]: "24 hours",
};

function CreateTokenDialog({
  open,
  onClose,
  onCreate,
}: {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onCreate: (
    deviceName: string,
    lifetime: TokenLifetime,
  ) => Promise<void>;
}) {
  const [lifetime, setLifetime] = useState(TokenLifetime.OneHour);
  const [error, setError] = useState<string | undefined>(undefined);
  const [pending, setPending] = useState(false);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const deviceName = formText(event.currentTarget, "deviceName");
    if (deviceName === "") {
      setError("Enter a device name.");
      return;
    }
    setError(undefined);
    setPending(true);
    try {
      await onCreate(deviceName, lifetime);
      onClose();
    } catch (caught) {
      setError(errorMessage(caught));
    } finally {
      setPending(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) {
          setError(undefined);
          onClose();
        }
      }}
    >
      <DialogContent>
        <form onSubmit={(event) => void submit(event)} noValidate>
          <DialogTitle>Create an enrollment token</DialogTitle>
          <DialogDescription>
            Run the device agent with this token to add one device to the
            organization. The token works once.
          </DialogDescription>
          <TextField
            label="Device name"
            name="deviceName"
            autoFocus
            required
            maxLength={100}
            error={error}
          />
          <Select
            label="Expires after"
            value={lifetime}
            onValueChange={(value) => {
              const next = Object.values(TokenLifetime).find(
                (candidate) => candidate === value,
              );
              if (next) {
                setLifetime(next);
              }
            }}
          >
            {Object.values(TokenLifetime).map((candidate) => (
              <SelectOption key={candidate} value={candidate}>
                {LIFETIME_LABELS[candidate]}
              </SelectOption>
            ))}
          </Select>
          <DialogActions>
            <DialogClose>Cancel</DialogClose>
            <Button type="submit" disabled={pending}>
              {pending ? "Creating…" : "Create token"}
            </Button>
          </DialogActions>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function EnrollmentTokensPage({
  organizationId,
}: OrganizationPageProps) {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<EnrollmentToken | undefined>(
    undefined,
  );
  const [newSecret, setNewSecret] = useState<string | undefined>(undefined);
  const query = useInfiniteQuery(
    DeviceService.method.listEnrollmentTokens,
    { organizationId, pageSize: PAGE_SIZE, pageToken: "" },
    pageOptions,
  );
  const create = useMutation(DeviceService.method.createEnrollmentToken);
  const revoke = useMutation(DeviceService.method.revokeEnrollmentToken);
  const invalidate = () =>
    queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({
        schema: DeviceService,
        cardinality: undefined,
      }),
    });
  const tokens =
    query.data?.pages.flatMap((page) => page.enrollmentTokens) ?? [];

  return (
    <main className="page">
      <PageHeader
        title="Enrollment tokens"
        description="One-time tokens that add a device to this organization. A token expires within 24 hours."
        actions={
          <Button onClick={() => setCreating(true)}>Create token</Button>
        }
      />
      {query.isPending ? (
        <LoadingState />
      ) : query.isError ? (
        <ErrorState error={query.error} />
      ) : tokens.length === 0 ? (
        <EmptyState>No enrollment tokens yet.</EmptyState>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">Starts with</th>
                <th scope="col">State</th>
                <th scope="col">Created</th>
                <th scope="col">Expires</th>
                <th scope="col">Device</th>
                <th scope="col">
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((token) => (
                <tr key={token.enrollmentTokenId}>
                  <td className="mono">{token.secretPrefix}…</td>
                  <td>{formatEnum(EnrollmentTokenState, token.state)}</td>
                  <td>{formatTime(token.createTime)}</td>
                  <td>{formatTime(token.expireTime)}</td>
                  <td className="mono">{token.deviceId || "—"}</td>
                  <td className="row-actions">
                    {token.state === EnrollmentTokenState.PENDING ? (
                      <Button
                        size={ButtonSize.Small}
                        variant={ButtonVariant.Danger}
                        onClick={() => setRevoking(token)}
                      >
                        Revoke
                      </Button>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <LoadMore
        hasMore={query.hasNextPage}
        loading={query.isFetchingNextPage}
        onLoadMore={() => void query.fetchNextPage()}
      />
      <CreateTokenDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreate={async (deviceName, lifetime) => {
          const response = await create.mutateAsync({
            organizationId,
            deviceName,
            expiresIn: { seconds: LIFETIME_SECONDS[lifetime] },
          });
          await invalidate();
          setNewSecret(response.enrollmentTokenSecret);
        }}
      />
      <ConfirmDialog
        open={revoking !== undefined}
        title="Revoke this enrollment token?"
        description="No device can enroll with it any more. Devices that already enrolled with it stay enrolled."
        confirmLabel="Revoke token"
        onClose={() => setRevoking(undefined)}
        onConfirm={async () => {
          if (revoking === undefined) {
            return;
          }
          await revoke.mutateAsync({
            organizationId,
            enrollmentTokenId: revoking.enrollmentTokenId,
          });
          await invalidate();
        }}
      />
      <SecretDialog
        secret={newSecret}
        title="Enrollment token"
        description="Give this token to the device agent to enroll the device. It works once."
        secretLabel="Enrollment token"
        onClose={() => setNewSecret(undefined)}
      />
    </main>
  );
}
