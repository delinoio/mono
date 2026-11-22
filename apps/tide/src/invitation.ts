// SPDX-License-Identifier: Apache-2.0

/**
 * Invitation links carry the secret in the URL fragment, so it does not
 * reach the static host's logs. The console reads the fragment, removes it
 * from the address bar and keeps the secret only in component state; it
 * stores nothing in browser storage (docs/apps-tide-console-foundation.md).
 */

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

/** Accepts a pasted invitation link or a bare secret and returns the secret. */
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
