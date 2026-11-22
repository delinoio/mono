// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import { Select, SelectOption } from "delino-ui/select";
import { toast } from "delino-ui/toast";
import { EnrollmentTokenState } from "@delino/ownsand-protos/v1/common_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { useOrg } from "../components/shell.tsx";
import {
  Empty,
  FormDialog,
  PageHeader,
  QueryBoundary,
  SecretDialog,
  ShortId,
  toastError,
} from "../components/ui.tsx";
import { formatTime, labels } from "../format.ts";

/** Token lifetimes that the console offers; the maximum is 24 hours. */
const TTL_HOURS = [1, 6, 12, 24] as const;

/** Route `enrollment-tokens`: create, list and revoke enrollment tokens. */
export function EnrollmentTokensPage() {
  const { organizationId } = useOrg();
  const queryClient = useQueryClient();
  const refresh = () => invalidateServices(queryClient, DeviceService);
  const list = useQuery(DeviceService.method.listEnrollmentTokens, {
    organizationId,
    pageSize: 200,
  });
  const create = useMutation(DeviceService.method.createEnrollmentToken, {
    onSuccess: refresh,
  });
  const revoke = useMutation(DeviceService.method.revokeEnrollmentToken, {
    onSuccess: refresh,
  });
  const [creating, setCreating] = useState(false);
  const [ttl, setTtl] = useState("1");
  const [token, setToken] = useState<string>();

  return (
    <>
      <PageHeader
        title="Enrollment tokens"
        description="A token adds exactly one device to this organization. It works once and expires within 24 hours."
        action={<Button onClick={() => setCreating(true)}>Create token</Button>}
      />
      <QueryBoundary query={list} loading="Loading enrollment tokens">
        {(data) =>
          data.tokens.length === 0 ? (
            <Empty title="No enrollment tokens" />
          ) : (
            <ul className="list" aria-label="Enrollment tokens">
              {data.tokens.map((t) => (
                <li key={t.enrollmentTokenId} className="list-row">
                  <div className="list-main">
                    <span className="list-title">
                      <code>{t.tokenPrefix}…</code> ·{" "}
                      {labels.tokenState[t.state]}
                    </span>
                    <span className="muted">
                      Created {formatTime(t.createTime)} · expires{" "}
                      {formatTime(t.expireTime)}
                      {t.deviceId ? (
                        <>
                          {" "}
                          · device <ShortId id={t.deviceId} />
                        </>
                      ) : null}
                    </span>
                  </div>
                  {t.state === EnrollmentTokenState.PENDING ? (
                    <Button
                      variant={ButtonVariant.Secondary}
                      size={ButtonSize.Small}
                      onClick={async () => {
                        try {
                          await revoke.mutateAsync({
                            organizationId,
                            enrollmentTokenId: t.enrollmentTokenId,
                          });
                          toast.success("The token is revoked.");
                        } catch (error) {
                          toastError(error);
                        }
                      }}
                    >
                      Revoke
                    </Button>
                  ) : null}
                </li>
              ))}
            </ul>
          )
        }
      </QueryBoundary>
      <FormDialog
        open={creating}
        onOpenChange={setCreating}
        title="Create an enrollment token"
        submitLabel="Create token"
        onSubmit={async () => {
          const res = await create.mutateAsync({
            organizationId,
            ttl: { seconds: BigInt(Number(ttl) * 3600) },
          });
          setCreating(false);
          setToken(res.enrollmentToken);
        }}
      >
        <Select
          label="Expires after"
          value={ttl}
          onValueChange={(v) => v && setTtl(v)}
        >
          {TTL_HOURS.map((h) => (
            <SelectOption key={h} value={String(h)}>
              {h === 1 ? "1 hour" : `${h} hours`}
            </SelectOption>
          ))}
        </Select>
      </FormDialog>
      <SecretDialog
        title="Your new enrollment token"
        label="Enrollment token"
        secret={token}
        hint="On the device, run ownsand-agent enroll and paste the token on standard input. Do not pass it as a command-line argument."
        onClose={() => setToken(undefined)}
      />
    </>
  );
}
