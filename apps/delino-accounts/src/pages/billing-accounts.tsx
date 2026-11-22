// SPDX-License-Identifier: Apache-2.0
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge } from "delino-ui/badge";
import { Button } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { useState } from "react";
import { invalidateServices, LIST_PAGE_SIZE } from "../api.ts";
import { Page } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  FormDialog,
  Loading,
  PageHeader,
} from "../components/ui.tsx";
import { checkAccountName } from "../forms.ts";
import { labels } from "../format.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/** Route `billing`: the caller's billing accounts and account creation. */
export function BillingAccountsPage() {
  const accounts = useQuery(BillingAccountService.method.listBillingAccounts, {
    pageSize: LIST_PAGE_SIZE,
  });
  const [creating, setCreating] = useState(false);
  return (
    <Page narrow>
      <PageHeader
        title="Billing accounts"
        description="A billing account holds prepaid credit and pays for the organizations that are linked to it."
        action={
          <Button onClick={() => setCreating(true)}>
            Create billing account
          </Button>
        }
      />
      {accounts.isPending ? (
        <Loading label="Loading billing accounts" />
      ) : accounts.error ? (
        <ErrorNotice
          error={accounts.error}
          onRetry={() => void accounts.refetch()}
        />
      ) : accounts.data.billingAccounts.length === 0 ? (
        <Empty title="No billing accounts yet">
          Create one to pay for Delino services, or accept an invitation from a
          billing account owner.
        </Empty>
      ) : (
        <ul className="list" aria-label="Billing accounts">
          {accounts.data.billingAccounts.map((account) => (
            <li key={account.billingAccountId} className="list-row">
              <Link
                to={routePath({
                  id: RouteId.BillingAccount,
                  billingAccountId: account.billingAccountId,
                })}
                className="list-link"
              >
                <span className="list-title">{account.name}</span>
                <Badge>{labels.role[account.callerRole]}</Badge>
              </Link>
            </li>
          ))}
        </ul>
      )}
      <CreateBillingAccountDialog open={creating} onOpenChange={setCreating} />
    </Page>
  );
}

function CreateBillingAccountDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const create = useMutation(BillingAccountService.method.createBillingAccount);
  const [name, setName] = useState("");
  const [error, setError] = useState<string>();
  const reset = (next: boolean) => {
    if (!next) {
      setName("");
      setError(undefined);
    }
    onOpenChange(next);
  };
  return (
    <FormDialog
      open={open}
      onOpenChange={reset}
      title="Create a billing account"
      description="You become its owner. To use it, start the $10/month subscription."
      submitLabel="Create"
      onSubmit={async () => {
        const checked = checkAccountName(name);
        if (!checked.ok) {
          setError(checked.error);
          return;
        }
        setError(undefined);
        const res = await create.mutateAsync({ name: checked.value });
        await invalidateServices(queryClient, BillingAccountService);
        reset(false);
        navigate(
          routePath({
            id: RouteId.BillingAccount,
            billingAccountId: res.billingAccount!.billingAccountId,
          }),
        );
      }}
    >
      <TextField
        label="Name"
        value={name}
        onChange={(event) => setName(event.target.value)}
        error={error}
        autoFocus
        autoComplete="off"
        placeholder="For example, Acme Robotics"
      />
    </FormDialog>
  );
}
