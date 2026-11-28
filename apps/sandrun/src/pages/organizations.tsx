// SPDX-License-Identifier: Apache-2.0
import { useMutation } from "@connectrpc/connect-query";
import { Badge } from "delino-ui/badge";
import { Button, ButtonVariant } from "delino-ui/button";
import { PasswordInput } from "delino-ui/password-input";
import { TextField } from "delino-ui/text-field";
import { OrganizationService } from "@delino/sandrun-protos/v1/organization_pb";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { TopBar } from "../components/shell.tsx";
import {
  Empty,
  FormDialog,
  PageHeader,
  QueryBoundary,
} from "../components/ui.tsx";
import { labels } from "../format.ts";
import { secretFromInput, SECRET_PARAM } from "../invitation.ts";
import {
  ORGANIZATION_NAME_LIMIT,
  DisplayNameError,
  validateDisplayName,
} from "../name.ts";
import { type MessageKey, useI18n } from "../i18n.ts";
import { refreshOrganizations, useOrganizations } from "../organizations.ts";
import { Link, orgPath, RouteId, routePath, useRouter } from "../router.tsx";

/** Route `organizations`: the organization list, creation and invitations. */
export function OrganizationsPage() {
  const { t } = useI18n();
  const { navigate } = useRouter();
  const queryClient = useQueryClient();
  const orgs = useOrganizations();
  const create = useMutation(OrganizationService.method.createOrganization);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<MessageKey>();
  const [link, setLink] = useState("");
  const [linkError, setLinkError] = useState<MessageKey>();

  const join = (event: FormEvent) => {
    event.preventDefault();
    const secret = secretFromInput(link);
    if (secret === undefined) {
      setLinkError("organizations.join.inputRequired");
      return;
    }
    setLink("");
    navigate(
      `${routePath({ id: RouteId.Invitation })}#${SECRET_PARAM}=${encodeURIComponent(secret)}`,
    );
  };

  return (
    <>
      <TopBar />
      <main className="content content-narrow">
        <PageHeader
          title={t("organizations.title")}
          description={t("organizations.description")}
          action={
            <Button onClick={() => setCreating(true)}>
              {t("organizations.create")}
            </Button>
          }
        />
        <QueryBoundary query={orgs} loading={t("organizations.loading")}>
          {(data) =>
            data.length === 0 ? (
              <Empty title={t("organizations.empty.title")}>
                {t("organizations.empty.description")}
              </Empty>
            ) : (
              <ul className="list" aria-label={t("organizations.listLabel")}>
                {data.map((org) => (
                  <li key={org.organizationId} className="list-row">
                    <Link
                      className="list-link"
                      to={orgPath(RouteId.Overview, org.organizationId)}
                    >
                      <span className="list-title">{org.name}</span>
                      <Badge>{t(labels.role[org.callerRole])}</Badge>
                    </Link>
                  </li>
                ))}
              </ul>
            )
          }
        </QueryBoundary>
        <section className="section" aria-labelledby="join-title">
          <h2 id="join-title">{t("organizations.join.title")}</h2>
          <form className="inline-form" onSubmit={join}>
            <PasswordInput
              label={t("invitation.inputLabel")}
              toggleLabel={t("invitation.showInput")}
              value={link}
              onValueChange={(value) => {
                setLink(value);
                setLinkError(undefined);
              }}
              error={linkError && t(linkError)}
              autoComplete="off"
            />
            <Button type="submit" variant={ButtonVariant.Secondary}>
              {t("organizations.join.continue")}
            </Button>
          </form>
        </section>
      </main>
      <FormDialog
        open={creating}
        title={t("organizations.createDialog.title")}
        description={t("organizations.createDialog.description")}
        submitLabel={t("organizations.createDialog.submit")}
        onOpenChange={(open) => {
          setCreating(open);
          if (!open) {
            setName("");
            setNameError(undefined);
          }
        }}
        onSubmit={async (opening) => {
          const trimmed = name.trim();
          const error = validateDisplayName(trimmed, ORGANIZATION_NAME_LIMIT);
          if (error !== undefined) {
            setNameError(
              error === DisplayNameError.ControlCharacter
                ? "common.nameControlCharacter"
                : "organizations.createDialog.nameInvalid",
            );
            return;
          }
          const res = await create.mutateAsync({ name: trimmed });
          void refreshOrganizations(queryClient);
          if (!opening.isCurrent()) return;
          setCreating(false);
          setName("");
          navigate(orgPath(RouteId.Overview, res.organization!.organizationId));
        }}
      >
        <TextField
          label={t("organizations.createDialog.nameLabel")}
          value={name}
          onValueChange={(value) => {
            setName(value);
            setNameError(undefined);
          }}
          error={nameError && t(nameError, { max: ORGANIZATION_NAME_LIMIT })}
          required
          autoFocus
        />
      </FormDialog>
    </>
  );
}
