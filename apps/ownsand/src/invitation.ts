// SPDX-License-Identifier: Apache-2.0

/**
 * Invitation secret handling.
 *
 * An invitation link has the form `<console origin>/invitations/accept#secret=<secret>`.
 * The secret is in the fragment, so browsers never send it to Cloudflare
 * Pages and it does not appear in request logs. The console removes the
 * fragment from the address bar as soon as it reads it.
 *
 * When the user is not signed in, the fragment does not survive the Logto
 * sign-in redirect, so the secret is kept in `sessionStorage` under
 * `PENDING_INVITATION_KEY` for the redirect only. It is deleted when
 * `AcceptInvitation` returns (success or failure) and when the user navigates
 * to a console path outside the invitation flow.
 */

/** Path of the invitation acceptance route. */
export const INVITATION_PATH = "/invitations/accept";

/** Path of the Logto sign-in callback route. */
export const SIGN_IN_CALLBACK_PATH = "/callback";

/** `sessionStorage` key of a pending invitation secret during sign-in. */
export const PENDING_INVITATION_KEY = "ownsand.pendingInvitationSecret";

/** Fragment parameter that carries the secret. */
const SECRET_PARAMETER = "secret";

/** Upper bound on the accepted secret length, to reject garbage early. */
const MAX_SECRET_LENGTH = 512;

/** Minimal `Storage` surface, so tests can pass an in-memory store. */
export interface SecretStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** Minimal `History` surface used to remove the fragment. */
export interface FragmentHistory {
  replaceState(data: unknown, unused: string, url?: string | URL | null): void;
  readonly state: unknown;
}

function normalizeSecret(value: string | null | undefined): string | undefined {
  if (value === null || value === undefined) {
    return undefined;
  }
  const secret = value.trim();
  if (secret === "" || secret.length > MAX_SECRET_LENGTH) {
    return undefined;
  }
  // Secrets are opaque printable tokens; reject white space and controls.
  if (!/^[\x21-\x7e]+$/.test(secret)) {
    return undefined;
  }
  return secret;
}

/** Reads the secret from a URL fragment such as `#secret=abc`. */
export function readSecretFromFragment(hash: string): string | undefined {
  const fragment = hash.startsWith("#") ? hash.slice(1) : hash;
  if (fragment === "") {
    return undefined;
  }
  return normalizeSecret(new URLSearchParams(fragment).get(SECRET_PARAMETER));
}

/** Builds the invitation link that a new invitation secret is shared with. */
export function buildInvitationLink(origin: string, secret: string): string {
  const fragment = new URLSearchParams({ [SECRET_PARAMETER]: secret });
  return `${origin}${INVITATION_PATH}#${fragment.toString()}`;
}

/**
 * Reads what the user pasted into the invitation entry form: a full
 * invitation link or the bare secret.
 */
export function parseInvitationInput(input: string): string | undefined {
  const value = input.trim();
  if (/^https?:\/\//i.test(value)) {
    // A link carries the secret only in its fragment.
    const hashIndex = value.indexOf("#");
    return hashIndex < 0
      ? undefined
      : readSecretFromFragment(value.slice(hashIndex));
  }
  if (value.startsWith("#")) {
    return readSecretFromFragment(value);
  }
  return normalizeSecret(value);
}

/**
 * Removes the fragment from the address bar without a navigation or a new
 * history entry, so the secret is not left in the visible URL or in history.
 */
export function removeFragment(
  location: Pick<Location, "pathname" | "search">,
  history: FragmentHistory,
): void {
  history.replaceState(history.state, "", location.pathname + location.search);
}

/** Keeps the secret for the sign-in redirect only. */
export function keepSecretForSignIn(
  storage: SecretStorage,
  secret: string,
): void {
  storage.setItem(PENDING_INVITATION_KEY, secret);
}

/** Returns the secret kept for the sign-in redirect, if any. */
export function readPendingSecret(storage: SecretStorage): string | undefined {
  return normalizeSecret(storage.getItem(PENDING_INVITATION_KEY));
}

/** Deletes the secret kept for the sign-in redirect. */
export function clearPendingSecret(storage: SecretStorage): void {
  storage.removeItem(PENDING_INVITATION_KEY);
}

/**
 * True for console paths that belong to the invitation flow: the acceptance
 * route itself and the sign-in callback that returns to it.
 */
export function isInvitationFlowPath(pathname: string): boolean {
  return pathname === INVITATION_PATH || pathname === SIGN_IN_CALLBACK_PATH;
}

/**
 * Applies the navigation rule: a pending secret is deleted when the user
 * reaches a path outside the invitation flow.
 */
export function clearPendingSecretOutsideFlow(
  storage: SecretStorage,
  pathname: string,
): void {
  if (!isInvitationFlowPath(pathname)) {
    clearPendingSecret(storage);
  }
}

/** Returns `window.sessionStorage`, or `undefined` when it is unavailable. */
export function browserSessionStorage(): SecretStorage | undefined {
  try {
    return window.sessionStorage;
  } catch {
    // Storage can be disabled by the browser; the flow then works only for a
    // user who is already signed in.
    return undefined;
  }
}
