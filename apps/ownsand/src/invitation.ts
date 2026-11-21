// SPDX-License-Identifier: Apache-2.0

/**
 * Invitation secrets arrive in the URL fragment, so they do not reach the
 * static host's logs. The console removes the fragment after it reads it and
 * keeps the secret in `sessionStorage` only across the Logto sign-in
 * redirect (docs/apps-ownsand-console-foundation.md).
 */

/** The only browser storage key the console writes itself. */
export const PENDING_INVITATION_KEY = "ownsand.pending-invitation";

/** Fragment parameter that carries the secret: `#secret=<value>`. */
export const SECRET_PARAM = "secret";

/** Builds the invitation link that an admin shares. */
export function invitationLink(origin: string, secret: string): string {
  return `${origin}/invitations/accept#${SECRET_PARAM}=${encodeURIComponent(secret)}`;
}

/** Reads the secret from a fragment such as `#secret=...`. */
export function secretFromFragment(hash: string): string | undefined {
  const params = new URLSearchParams(
    hash.startsWith("#") ? hash.slice(1) : hash,
  );
  const secret = params.get(SECRET_PARAM)?.trim();
  return secret === undefined || secret === "" ? undefined : secret;
}

/**
 * Accepts a pasted invitation link or a bare secret and returns the secret.
 */
export function secretFromInput(input: string): string | undefined {
  const text = input.trim();
  if (text === "") return undefined;
  if (/^https?:\/\//i.test(text)) {
    try {
      return secretFromFragment(new URL(text).hash);
    } catch {
      return undefined;
    }
  }
  if (text.startsWith("#")) return secretFromFragment(text);
  return /\s/.test(text) ? undefined : text;
}

/**
 * Reads the secret from the current address and removes the fragment from
 * the address bar without a navigation.
 */
export function takeSecretFromAddress(
  location: Pick<Location, "hash" | "pathname" | "search">,
  history: Pick<History, "replaceState">,
): string | undefined {
  const secret = secretFromFragment(location.hash);
  if (location.hash !== "") {
    history.replaceState(null, "", location.pathname + location.search);
  }
  return secret;
}

type SessionStore = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function store(): SessionStore | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

/** The secret that waits for the sign-in redirect to return. */
export const pendingInvitation = {
  save(secret: string, storage: SessionStore | undefined = store()): void {
    try {
      storage?.setItem(PENDING_INVITATION_KEY, secret);
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
