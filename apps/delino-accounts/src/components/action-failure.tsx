// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { toast } from "delino-ui/toast";
import { type ReactNode, useCallback, useMemo, useState } from "react";
import { errorMessage } from "../format.ts";
import { ErrorNotice } from "./ui.tsx";

/**
 * The failure state of the direct actions of a billing section: a button that
 * runs a request on click and reports its outcome without a form.
 */
export interface ActionFailure {
  /**
   * The notice of an ended session, or `null`. Render it inside the loaded
   * section, in the same position of the tree in every state, so that the
   * loaded content below it does not remount and stays visible.
   */
  notice: ReactNode;
  /**
   * Reports a failed action. A `Code.Unauthenticated` failure shows
   * `notice`, which offers the sign-in redirect, because a toast cannot offer
   * it and retrying cannot succeed. Any other failure keeps its error toast.
   * Nothing replays the action: the redirect starts only when the user clicks
   * the button of the notice.
   */
  report: (error: unknown) => void;
  /** Hides `notice` after a later action succeeded. */
  clear: () => void;
}

/**
 * Gives a section with direct action buttons (open an invoice, resume a
 * subscription, change a role, revoke an invitation) a way to offer the
 * sign-in redirect when the action fails with `Code.Unauthenticated`.
 */
export function useActionFailure(): ActionFailure {
  const [signInError, setSignInError] = useState<ConnectError>();
  const report = useCallback((error: unknown) => {
    if (error instanceof ConnectError && error.code === Code.Unauthenticated) {
      setSignInError(error);
      return;
    }
    toast.error(errorMessage(error));
  }, []);
  const clear = useCallback(() => setSignInError(undefined), []);
  return useMemo(
    () => ({
      notice:
        signInError === undefined ? null : <ErrorNotice error={signInError} />,
      report,
      clear,
    }),
    [signInError, report, clear],
  );
}
