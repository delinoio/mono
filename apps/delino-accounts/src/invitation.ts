// SPDX-License-Identifier: Apache-2.0

/**
 * Billing account invitation tokens arrive in the URL fragment of the email
 * link (`/billing/invitations/accept#token=<token>`), so they do not reach
 * the Cloudflare Pages logs. The app removes the fragment after it reads it
 * and keeps the token in `sessionStorage` only across the Logto sign-in
 * redirect (docs/apps-delino-accounts-foundation.md). The token is a secret:
 * never log it.
 */

/** The only browser storage key the app writes itself. */
export const PENDING_INVITATION_KEY = "delino-accounts.pending-invitation";

/** Fragment parameter that carries the token: `#token=<value>`. */
export const TOKEN_PARAM = "token";

/** Reads the token from a fragment such as `#token=...`. */
export function tokenFromFragment(hash: string): string | undefined {
  const params = new URLSearchParams(
    hash.startsWith("#") ? hash.slice(1) : hash,
  );
  const token = params.get(TOKEN_PARAM)?.trim();
  return token === undefined || token === "" ? undefined : token;
}

/**
 * Reads the token from the current address and removes the fragment from the
 * address bar without a navigation.
 */
export function takeTokenFromAddress(
  location: Pick<Location, "hash" | "pathname" | "search">,
  history: Pick<History, "replaceState">,
): string | undefined {
  const token = tokenFromFragment(location.hash);
  if (location.hash !== "") {
    history.replaceState(null, "", location.pathname + location.search);
  }
  return token;
}

type SessionStore = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function store(): SessionStore | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

/** The token that waits for the sign-in redirect to return. */
export const pendingInvitation = {
  save(token: string, storage: SessionStore | undefined = store()): void {
    try {
      storage?.setItem(PENDING_INVITATION_KEY, token);
    } catch {
      // Without storage, the user opens the link again after sign-in.
    }
  },
  read(storage: SessionStore | undefined = store()): string | undefined {
    try {
      return storage?.getItem(PENDING_INVITATION_KEY) ?? undefined;
    } catch {
      return undefined;
    }
  },
  clear(storage: SessionStore | undefined = store()): void {
    try {
      storage?.removeItem(PENDING_INVITATION_KEY);
    } catch {
      // Nothing to clear.
    }
  },
};
