// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Badge } from "delino-ui/badge";
import { Button } from "delino-ui/button";
import { TextField } from "delino-ui/text-field";
import { useState } from "react";
import { invalidateServices } from "../api.ts";
import { Page } from "../components/shell.tsx";
import {
  Empty,
  ErrorNotice,
  FormDialog,
  formInvalid,
  formSubmitted,
  Loading,
  MorePages,
  PageHeader,
  RefetchNotice,
} from "../components/ui.tsx";
import { checkAccountName } from "../forms.ts";
import { useAllPages } from "../paging.ts";
import { labels } from "../format.ts";
import { useI18n } from "../i18n.ts";
import { BillingAccountService } from "@delino/accounts-protos/v1/billing_account_pb";
import { Link, RouteId, routePath, useRouter } from "../router.tsx";

/** Route `billing`: the caller's billing accounts and account creation. */
export function BillingAccountsPage() {
  const accounts = useAllPages(
    BillingAccountService.method.listBillingAccounts,
    {},
    (page) => ({
      items: page.billingAccounts,
      nextPageToken: page.nextPageToken,
    }),
    (account) => account.billingAccountId,
  );
  const [creating, setCreating] = useState(false);
  const { t } = useI18n();
  return (
    <Page narrow>
      <PageHeader
        title={t("billingAccounts.title")}
        description={t("billingAccounts.description")}
        action={
          <Button onClick={() => setCreating(true)}>
            {t("billingAccounts.create")}
          </Button>
        }
      />
      <RefetchNotice error={accounts.refreshError} onRetry={accounts.refetch} />
      {accounts.isPending ? (
        <Loading label={t("billingAccounts.loading")} />
      ) : accounts.error ? (
        <ErrorNotice error={accounts.error} onRetry={accounts.refetch} />
      ) : accounts.items.length === 0 ? (
        <Empty title={t("billingAccounts.empty.title")}>
          {t("billingAccounts.empty.description")}
        </Empty>
      ) : (
        <>
          <ul className="list" aria-label={t("billingAccounts.title")}>
            {accounts.items.map((account) => (
              <li key={account.billingAccountId} className="list-row">
                <Link
                  to={routePath({
                    id: RouteId.BillingAccount,
                    billingAccountId: account.billingAccountId,
                  })}
                  className="list-link"
                >
                  <span className="list-title">{account.name}</span>
                  <Badge>{t(labels.role[account.callerRole])}</Badge>
                </Link>
              </li>
            ))}
          </ul>
          <MorePages pages={accounts} />
        </>
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
  const i18n = useI18n();
  const { t } = i18n;
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
      title={t("createAccount.title")}
      description={t("createAccount.description")}
      submitLabel={t("createAccount.submit")}
      onSubmit={async () => {
        const checked = checkAccountName(name, i18n);
        if (!checked.ok) {
          setError(checked.error);
          return formInvalid();
        }
        setError(undefined);
        const res = await create.mutateAsync({ name: checked.value });
        // Refresh the cached lists in the background. The dialog must not wait
        // for a read of the list: a stalled refetch would keep it open with a
        // disabled button although the account exists. The refetch reports its
        // failure through the query state, so the catch only keeps an
        // unexpected rejection from becoming an unhandled one.
        void invalidateServices(queryClient, BillingAccountService).catch(
          () => undefined,
        );
        // Close and navigate only when this opening is still current.
        return formSubmitted(() => {
          reset(false);
          navigate(
            routePath({
              id: RouteId.BillingAccount,
              billingAccountId: res.billingAccount!.billingAccountId,
            }),
          );
        });
      }}
    >
      <TextField
        label={t("createAccount.name")}
        value={name}
        onChange={(event) => setName(event.target.value)}
        error={error}
        autoFocus
        autoComplete="off"
        placeholder={t("createAccount.namePlaceholder")}
      />
    </FormDialog>
  );
}
