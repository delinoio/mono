// SPDX-License-Identifier: Apache-2.0
import { Button, ButtonSize, ButtonVariant } from "delino-ui/button";
import type { ReactNode } from "react";
import { errorMessage } from "../api/errors.ts";

export function PageHeader({
  title,
  description,
  actions,
}: {
  readonly title: string;
  readonly description?: string;
  readonly actions?: ReactNode;
}) {
  return (
    <header className="page-header">
      <div>
        <h1>{title}</h1>
        {description ? <p className="muted">{description}</p> : null}
      </div>
      {actions ? <div className="page-actions">{actions}</div> : null}
    </header>
  );
}

export function LoadingState({
  label = "Loading…",
}: {
  readonly label?: string;
}) {
  return (
    <p className="muted" role="status">
      {label}
    </p>
  );
}

export function ErrorState({ error }: { readonly error: unknown }) {
  return (
    <p className="form-error" role="alert">
      {errorMessage(error)}
    </p>
  );
}

export function EmptyState({ children }: { readonly children: ReactNode }) {
  return <p className="empty">{children}</p>;
}

export function LoadMore({
  hasMore,
  loading,
  onLoadMore,
}: {
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly onLoadMore: () => void;
}) {
  if (!hasMore) {
    return null;
  }
  return (
    <div className="load-more">
      <Button
        variant={ButtonVariant.Secondary}
        size={ButtonSize.Small}
        onClick={onLoadMore}
        disabled={loading}
      >
        {loading ? "Loading…" : "Load more"}
      </Button>
    </div>
  );
}

/** Reads a trimmed text value from submitted form data. */
export function formText(form: HTMLFormElement, name: string): string {
  const value = new FormData(form).get(name);
  return typeof value === "string" ? value.trim() : "";
}
