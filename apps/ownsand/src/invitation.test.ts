// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  INVITATION_PATH,
  PENDING_INVITATION_KEY,
  buildInvitationLink,
  clearPendingSecret,
  clearPendingSecretOutsideFlow,
  isInvitationFlowPath,
  keepSecretForSignIn,
  parseInvitationInput,
  readPendingSecret,
  readSecretFromFragment,
  removeFragment,
  type SecretStorage,
} from "./invitation.ts";

function memoryStorage(): SecretStorage & { items: Map<string, string> } {
  const items = new Map<string, string>();
  return {
    items,
    getItem: (key) => items.get(key) ?? null,
    setItem: (key, value) => {
      items.set(key, value);
    },
    removeItem: (key) => {
      items.delete(key);
    },
  };
}

describe("readSecretFromFragment", () => {
  test("reads the secret parameter of the fragment", () => {
    expect(readSecretFromFragment("#secret=inv_abc123")).toBe("inv_abc123");
    expect(readSecretFromFragment("secret=inv_abc123")).toBe("inv_abc123");
  });

  test("decodes a percent-encoded secret", () => {
    expect(readSecretFromFragment("#secret=a%2Bb%3Dc")).toBe("a+b=c");
  });

  test.each([
    "",
    "#",
    "#other=1",
    "#secret=",
    "#secret=%20%20",
    "#secret=a%20b",
  ])("returns nothing for %j", (hash) => {
    expect(readSecretFromFragment(hash)).toBeUndefined();
  });

  test("rejects an overlong value", () => {
    expect(
      readSecretFromFragment(`#secret=${"a".repeat(513)}`),
    ).toBeUndefined();
  });
});

describe("buildInvitationLink", () => {
  test("puts the secret in the fragment of the acceptance route", () => {
    const link = buildInvitationLink("https://ownsand.delino.io", "a+b/c");
    const url = new URL(link);
    expect(url.origin).toBe("https://ownsand.delino.io");
    expect(url.pathname).toBe(INVITATION_PATH);
    expect(url.search).toBe("");
    expect(readSecretFromFragment(url.hash)).toBe("a+b/c");
  });
});

describe("parseInvitationInput", () => {
  test("accepts a whole invitation link", () => {
    expect(
      parseInvitationInput(
        " https://ownsand.delino.io/invitations/accept#secret=inv_1 ",
      ),
    ).toBe("inv_1");
  });

  test("accepts a bare secret", () => {
    expect(parseInvitationInput("inv_1")).toBe("inv_1");
  });

  test("rejects a link without a secret and an empty value", () => {
    expect(
      parseInvitationInput("https://ownsand.delino.io/invitations/accept"),
    ).toBeUndefined();
    expect(
      parseInvitationInput("https://ownsand.delino.io/invitations/accept#x=1"),
    ).toBeUndefined();
    expect(parseInvitationInput("   ")).toBeUndefined();
  });
});

describe("removeFragment", () => {
  test("replaces the current entry with the path and query only", () => {
    const calls: unknown[][] = [];
    removeFragment(
      { pathname: "/invitations/accept", search: "?x=1" },
      {
        state: { kept: true },
        replaceState: (...args: unknown[]) => {
          calls.push(args);
        },
      },
    );
    expect(calls).toEqual([[{ kept: true }, "", "/invitations/accept?x=1"]]);
  });
});

describe("pending secret storage", () => {
  test("keeps, reads and clears the secret under one key", () => {
    const storage = memoryStorage();
    keepSecretForSignIn(storage, "inv_1");
    expect(storage.items.get(PENDING_INVITATION_KEY)).toBe("inv_1");
    expect(readPendingSecret(storage)).toBe("inv_1");
    clearPendingSecret(storage);
    expect(storage.items.size).toBe(0);
    expect(readPendingSecret(storage)).toBeUndefined();
  });

  test("keeps the secret on the invitation route and the sign-in callback", () => {
    const storage = memoryStorage();
    keepSecretForSignIn(storage, "inv_1");
    clearPendingSecretOutsideFlow(storage, "/callback");
    clearPendingSecretOutsideFlow(storage, INVITATION_PATH);
    expect(readPendingSecret(storage)).toBe("inv_1");
  });

  test.each(["/", "/orgs/o1/sandboxes", "/invitations", "/callback/x"])(
    "deletes the secret when the user navigates to %s",
    (pathname) => {
      const storage = memoryStorage();
      keepSecretForSignIn(storage, "inv_1");
      clearPendingSecretOutsideFlow(storage, pathname);
      expect(storage.items.size).toBe(0);
    },
  );

  test("names the flow paths", () => {
    expect(isInvitationFlowPath(INVITATION_PATH)).toBe(true);
    expect(isInvitationFlowPath("/callback")).toBe(true);
    expect(isInvitationFlowPath("/")).toBe(false);
  });
});
