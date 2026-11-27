// SPDX-License-Identifier: Apache-2.0
import { generateKeyPairSync, sign } from "node:crypto";
import {
  create,
  fromBinary,
  type MessageInitShape,
  toBinary,
} from "@bufbuild/protobuf";
import {
  AcceptInvitationRequestSchema,
  AcceptInvitationResponseSchema,
  BillingAccountRole,
  CreateInvitationRequestSchema,
  GetBillingAccountRequestSchema,
  GetBillingAccountResponseSchema,
  ListBillingAccountsResponseSchema,
} from "@delino/accounts-protos/v1/billing_account_pb";
import { type Browser, describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { PENDING_INVITATION_KEY } from "../src/invitation.ts";
import {
  API_ORIGIN,
  LOGTO_APP_ID,
  LOGTO_ENDPOINT,
  seedFixtureSession,
} from "./fixture-session.ts";

const INVITATION_PATH = "/billing/invitations/accept";
const FIRST_TOKEN = "bai_fixture_first";
const REPLACEMENT_TOKEN = "bai_fixture_replacement";
const ACCOUNT = {
  billingAccountId: "0193a1b2-0000-7000-8000-000000000001",
  name: "Example account",
  callerRole: BillingAccountRole.VIEWER,
};
const REPLACEMENT_ACCOUNT = {
  ...ACCOUNT,
  billingAccountId: "0193a1b2-0000-7000-8000-000000000002",
  name: "Replacement account",
};

async function stubLogto(browser: Browser) {
  const issuer = `${LOGTO_ENDPOINT}/oidc`;
  const { privateKey, publicKey } = generateKeyPairSync("ec", {
    namedCurve: "secp384r1",
  });
  const now = Math.floor(Date.now() / 1000);
  const header = Buffer.from(JSON.stringify({ alg: "ES384" })).toString(
    "base64url",
  );
  const claims = Buffer.from(
    JSON.stringify({
      iss: issuer,
      aud: LOGTO_APP_ID,
      sub: "invited-fixture-user",
      iat: now,
      exp: now + 3600,
    }),
  ).toString("base64url");
  const unsigned = `${header}.${claims}`;
  const signature = sign("sha384", Buffer.from(unsigned), {
    key: privateKey,
    dsaEncoding: "ieee-p1363",
  }).toString("base64url");
  let markStarted!: () => void;
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  let release!: (status: number) => void;
  const discoveryStatus = new Promise<number>((resolve) => {
    release = resolve;
  });
  let firstDiscovery = true;

  await browser.route(`${issuer}/**`, async (route) => {
    const url = new URL(route.request.url);
    const headers = { "access-control-allow-origin": "*" };
    switch (url.pathname) {
      case "/oidc/.well-known/openid-configuration": {
        let status = 200;
        if (firstDiscovery) {
          firstDiscovery = false;
          markStarted();
          status = await discoveryStatus;
        }
        await route.fulfill({
          status,
          headers,
          json: {
            issuer,
            authorization_endpoint: `${issuer}/auth`,
            token_endpoint: `${issuer}/token`,
            jwks_uri: `${issuer}/jwks`,
          },
        });
        return;
      }
      case "/oidc/auth": {
        const callback = new URL(url.searchParams.get("redirect_uri")!);
        callback.searchParams.set("code", "fixture-code");
        callback.searchParams.set("state", url.searchParams.get("state")!);
        await route.fulfill({
          status: 302,
          headers: { location: callback.href },
        });
        return;
      }
      case "/oidc/token":
        await route.fulfill({
          headers,
          json: {
            id_token: `${unsigned}.${signature}`,
            access_token: "fixture-access-token",
            refresh_token: "fixture-refresh-token",
            token_type: "Bearer",
            expires_in: 3600,
            scope: "accounts:billing",
          },
        });
        return;
      case "/oidc/jwks":
        await route.fulfill({
          headers,
          contentType: "application/json",
          body: JSON.stringify({ keys: [publicKey.export({ format: "jwk" })] }),
        });
        return;
      default:
        await route.abort();
    }
  });

  return { started, release };
}

async function captureAcceptances(
  browser: Browser,
  {
    holdFirst = false,
    firstResponse = { billingAccount: ACCOUNT },
  }: {
    holdFirst?: boolean;
    firstResponse?: MessageInitShape<
      typeof AcceptInvitationResponseSchema
    > | null;
  } = {},
) {
  const accepted: string[] = [];
  const joined: (typeof ACCOUNT)[] = [];
  let listReads = 0;
  let markFirstStarted!: () => void;
  let releaseFirst!: () => void;
  const firstStarted = new Promise<void>((resolve) => {
    markFirstStarted = resolve;
  });
  const firstGate = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });
  await browser.route(`${API_ORIGIN}/**`, async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }

    const method = new URL(route.request.url).pathname.split("/").pop();
    let body: Uint8Array<ArrayBufferLike> = new Uint8Array();
    if (method === "AcceptInvitation") {
      const { invitationToken } = fromBinary(
        AcceptInvitationRequestSchema,
        Buffer.from(route.request.postData ?? ""),
      );
      accepted.push(invitationToken);
      const first = invitationToken === FIRST_TOKEN;
      const account = first ? ACCOUNT : REPLACEMENT_ACCOUNT;
      const response = first ? firstResponse : { billingAccount: account };
      if (first) {
        markFirstStarted();
        if (holdFirst) await firstGate;
      }
      if (response === null) {
        await route.fulfill({
          status: 404,
          headers,
          json: { code: "not_found", message: "invitation not found" },
        });
        return;
      }
      joined.push(account);
      body = toBinary(
        AcceptInvitationResponseSchema,
        create(AcceptInvitationResponseSchema, response),
      );
    } else if (method === "GetBillingAccount") {
      const { billingAccountId } = fromBinary(
        GetBillingAccountRequestSchema,
        Buffer.from(route.request.postData ?? ""),
      );
      const account =
        billingAccountId === REPLACEMENT_ACCOUNT.billingAccountId
          ? REPLACEMENT_ACCOUNT
          : ACCOUNT;
      body = toBinary(
        GetBillingAccountResponseSchema,
        create(GetBillingAccountResponseSchema, { billingAccount: account }),
      );
    } else if (method === "ListBillingAccounts") {
      listReads += 1;
      body = toBinary(
        ListBillingAccountsResponseSchema,
        create(ListBillingAccountsResponseSchema, { billingAccounts: joined }),
      );
    }
    await route.fulfill({ contentType: "application/proto", headers, body });
  });
  return { accepted, firstStarted, releaseFirst, listReads: () => listReads };
}

async function openReplacement(browser: Browser) {
  await browser.evaluate((token) => {
    location.hash = `token=${token}`;
    return null;
  }, REPLACEMENT_TOKEN);
  await expect.poll(() => browser.evaluate(() => location.hash)).toBe("");
}

function storedToken(browser: Browser) {
  return browser.evaluate(
    (key) => sessionStorage.getItem(key),
    PENDING_INVITATION_KEY,
  );
}

describe("billing invitation sign-in", () => {
  for (const replacement of [
    "during sign-in",
    "before sign-in",
    "after sign-in fails",
    "none",
  ]) {
    test(`resumes the correct invitation with replacement ${replacement}`, async ({
      app,
      browser,
      screen,
    }) => {
      const logto = await stubLogto(browser);
      const { accepted } = await captureAcceptances(browser);
      const signIn = screen.getByRole("button", "Sign in to accept");
      try {
        await app.open(`${INVITATION_PATH}#token=${FIRST_TOKEN}`);
        if (replacement === "before sign-in") {
          await openReplacement(browser);
          await expect.poll(() => storedToken(browser)).toBe(null);
        }
        await signIn.click();
        await logto.started;
        if (replacement === "during sign-in") {
          await expect.poll(() => storedToken(browser)).toBe(FIRST_TOKEN);
          await openReplacement(browser);
          await expect.poll(() => storedToken(browser)).toBe(REPLACEMENT_TOKEN);
        }
        if (replacement === "after sign-in fails") {
          logto.release(503);
          await expect(
            screen.getByText("Sign-in did not start.", { exact: false }),
          ).toBeVisible();
          await openReplacement(browser);
          await expect.poll(() => storedToken(browser)).toBe(null);
          await signIn.click();
        }
      } finally {
        logto.release(200);
      }

      const accept = screen.getByRole("button", "Accept invitation");
      await expect(accept).toBeVisible();
      await expect
        .poll(() => browser.evaluate(() => location.pathname))
        .toBe(INVITATION_PATH);
      expect(accepted).toEqual([]);

      await browser.reload();
      await expect(accept).toBeVisible();
      expect(accepted).toEqual([]);
      if (replacement === "during sign-in") {
        await app.screenshot(
          "replacement-invitation-after-callback-and-reload",
        );
      }
      await accept.click();
      await expect(
        screen.getByRole(
          "heading",
          replacement === "none" ? ACCOUNT.name : REPLACEMENT_ACCOUNT.name,
          { level: 1 },
        ),
      ).toBeVisible();
      expect(accepted).toEqual([
        replacement === "none" ? FIRST_TOKEN : REPLACEMENT_TOKEN,
      ]);
      await expect.poll(() => storedToken(browser)).toBe(null);
    });
  }
});

describe("billing invitation acceptance", () => {
  for (const eventFirst of [false, true]) {
    test(`keeps the replacement invitation when hashchange runs ${eventFirst ? "before" : "after"} the first success`, async ({
      app,
      browser,
      screen,
    }) => {
      await seedFixtureSession(browser, app.baseUrl);
      const fixture = await captureAcceptances(browser, { holdFirst: true });
      const accept = screen.getByRole("button", "Accept invitation");
      const replacementHash = `#token=${REPLACEMENT_TOKEN}`;
      try {
        await app.open(`${INVITATION_PATH}#token=${FIRST_TOKEN}`);
        await accept.click();
        await fixture.firstStarted;
        await expect(accept).toBeDisabled();
        if (!eventFirst) await app.screenshot("first-invitation-pending");
        await browser.evaluate((hash) => {
          history.replaceState(null, "", hash);
          return null;
        }, replacementHash);
        const dispatchHashChange = () =>
          browser.evaluate(() => {
            window.dispatchEvent(new HashChangeEvent("hashchange"));
            return null;
          });
        if (eventFirst) await dispatchHashChange();
        fixture.releaseFirst();
        await expect(accept).toBeEnabled();
        expect(await browser.evaluate(() => location.pathname)).toBe(
          INVITATION_PATH,
        );
        if (!eventFirst) {
          expect(await browser.evaluate(() => location.hash)).toBe(
            replacementHash,
          );
          await dispatchHashChange();
        }
        await expect.poll(() => browser.evaluate(() => location.hash)).toBe("");
        await expect(accept).toBeEnabled();
        if (!eventFirst) await app.screenshot("replacement-invitation-ready");
        await accept.click();
        await expect(
          screen.getByRole("heading", REPLACEMENT_ACCOUNT.name, { level: 1 }),
        ).toBeVisible();
        expect(fixture.accepted).toEqual([FIRST_TOKEN, REPLACEMENT_TOKEN]);
      } finally {
        fixture.releaseFirst();
      }
    });
  }

  for (const [description, hash] of [
    ["empty fragment", ""],
    ["tokenless fragment", "#anchor"],
    ["same-token fragment", `#token=${FIRST_TOKEN}`],
  ]) {
    test(`opens the accepted account with history replacement (${description})`, async ({
      app,
      browser,
      screen,
    }) => {
      await seedFixtureSession(browser, app.baseUrl);
      const fixture = await captureAcceptances(browser, { holdFirst: true });
      try {
        await app.open(`${INVITATION_PATH}#token=${FIRST_TOKEN}`);
        const accept = screen.getByRole("button", "Accept invitation");
        const historyLength = await browser.evaluate(() => history.length);
        await accept.click();
        await fixture.firstStarted;
        await browser.evaluate((path) => {
          history.replaceState(null, "", path);
          return null;
        }, INVITATION_PATH + hash);
        if (!hash) await app.screenshot("current-invitation-pending");
        fixture.releaseFirst();
        await expect(
          screen.getByRole("heading", ACCOUNT.name, { level: 1 }),
        ).toBeVisible();
        expect(await browser.evaluate(() => location.pathname)).toBe(
          `/billing/${ACCOUNT.billingAccountId}`,
        );
        expect(await browser.evaluate(() => history.length)).toBe(
          historyLength,
        );
        expect(fixture.accepted).toEqual([FIRST_TOKEN]);
        if (!hash) await app.screenshot("current-invitation-account");
      } finally {
        fixture.releaseFirst();
      }
    });
  }

  for (const [description, firstResponse] of [
    ["omitted account", {}],
    ["empty account", { billingAccount: {} }],
    ["whitespace ID", { billingAccount: { billingAccountId: " \t " } }],
  ] as const) {
    test(`recovers a successful invitation with a missing routing ID (${description})`, async ({
      app,
      browser,
      screen,
    }) => {
      await seedFixtureSession(browser, app.baseUrl);
      const fixture = await captureAcceptances(browser, { firstResponse });
      await app.open("/billing");
      await expect(screen.getByText("No billing accounts yet")).toBeVisible();
      expect(fixture.listReads()).toBe(1);
      // Enter through the sign-in return path without reloading the query client.
      await browser.evaluate(
        ({ key, token, path }) => {
          sessionStorage.setItem(key, token);
          history.pushState(null, "", path);
          window.dispatchEvent(new PopStateEvent("popstate"));
          return null;
        },
        {
          key: PENDING_INVITATION_KEY,
          token: FIRST_TOKEN,
          path: INVITATION_PATH,
        },
      );
      const accept = screen.getByRole("button", "Accept invitation");
      await expect(accept).toBeVisible();
      const screenshotName = description.replace(" ", "-").toLowerCase();
      await app.screenshot(`before-${screenshotName}-recovery`);
      await accept.click();
      await expect(screen.getByRole("main").getByRole("alert")).toHaveText(
        "You may have joined the billing account.",
      );
      await expect(accept).toHaveCount(0);
      expect(await browser.evaluate(() => location.pathname)).toBe(
        INVITATION_PATH,
      );
      expect(await storedToken(browser)).toBe(null);
      expect(fixture.accepted).toEqual([FIRST_TOKEN]);
      expect(fixture.listReads()).toBe(1);
      await app.screenshot(`after-${screenshotName}-recovery`);
      await screen.getByRole("link", "Go to your billing accounts").click();
      await expect(
        screen.getByRole("link", /Example account.*Viewer/),
      ).toBeVisible();
      expect(await browser.evaluate(() => location.pathname)).toBe("/billing");
      expect(fixture.listReads()).toBe(2);
    });
  }

  for (const succeeds of [true, false]) {
    const outcome = succeeds ? "success" : "failure";
    test(`preserves the open billing list route after a late acceptance ${outcome}`, async ({
      app,
      browser,
      screen,
    }) => {
      await seedFixtureSession(browser, app.baseUrl);
      const fixture = await captureAcceptances(browser, {
        holdFirst: true,
        firstResponse: succeeds ? { billingAccount: ACCOUNT } : null,
      });
      try {
        await app.open(`${INVITATION_PATH}#token=${FIRST_TOKEN}`);
        await screen.getByRole("button", "Accept invitation").click();
        await fixture.firstStarted;
        await screen.getByRole("link", "Not now").click();
        const empty = screen.getByText("No billing accounts yet");
        await expect(empty).toBeVisible();
        expect(fixture.listReads()).toBe(1);
        const entry = await browser.evaluate(() => [
          location.href,
          history.length,
        ]);
        await app.screenshot(`billing-list-before-acceptance-${outcome}`);
        fixture.releaseFirst();
        if (succeeds) {
          await expect(
            screen.getByRole("link", /Example account.*Viewer/),
          ).toBeVisible();
        } else {
          await expect
            .poll(() =>
              browser.evaluate(() =>
                performance
                  .getEntriesByType("resource")
                  .some((entry) => entry.name.endsWith("/AcceptInvitation")),
              ),
            )
            .toBe(true);
          await expect(empty).toBeVisible();
        }
        expect(fixture.listReads()).toBe(succeeds ? 2 : 1);
        expect(fixture.accepted).toEqual([FIRST_TOKEN]);
        expect(
          await browser.evaluate(() => [location.href, history.length]),
        ).toEqual(entry);
        await app.screenshot(`billing-list-after-acceptance-${outcome}`);
      } finally {
        fixture.releaseFirst();
      }
    });
  }
});

test("malformed invitation emails stay in the dialog without an RPC", async ({
  app,
  browser,
  screen,
}) => {
  const billingAccountId = "0193a1b2-0000-7000-8000-000000000001";
  await seedFixtureSession(browser, app.baseUrl);
  const invitationRequests: string[] = [];
  await browser.route(`${API_ORIGIN}/**`, async (route) => {
    const headers = {
      "access-control-allow-origin": "*",
      "access-control-allow-methods": "POST",
      "access-control-allow-headers": "*",
    };
    if (route.request.method === "OPTIONS") {
      await route.fulfill({ status: 204, headers });
      return;
    }
    const method = new URL(route.request.url).pathname.split("/").pop();
    if (method === "CreateInvitation") {
      invitationRequests.push(route.request.postData ?? "");
    }
    const body =
      method === "GetBillingAccount"
        ? toBinary(
            GetBillingAccountResponseSchema,
            create(GetBillingAccountResponseSchema, {
              billingAccount: {
                billingAccountId,
                name: "Example account",
                callerRole: BillingAccountRole.OWNER,
              },
            }),
          )
        : new Uint8Array();
    await route.fulfill({ contentType: "application/proto", headers, body });
  });

  await app.open(`/billing/${billingAccountId}`);
  await screen.getByRole("tab", "Members").click();
  await screen.getByRole("button", "Invite by email").click();
  const dialog = screen.getByRole("dialog", "Invite by email");
  const email = dialog.getByRole("textbox", "Email");
  const send = dialog.getByRole("button", "Send invitation");
  for (const [address, screenshot] of [
    ["a..b@example.test", "invitation-email-double-dot"],
    ["<a@b>", "invitation-email-angle-brackets"],
    ["‹a@b›", "invitation-email-angle-quotes"],
  ] as const) {
    await email.fill(address);
    await send.click();
    await expect(dialog).toBeVisible();
    await expect(email).toHaveAttribute("aria-invalid", "true");
    await expect(
      dialog.getByText("Enter an email address like name@example.com."),
    ).toBeVisible();
    expect(invitationRequests).toHaveLength(0);
    await app.screenshot(screenshot);
  }

  await email.fill("a.b@example.test");
  await send.click();
  await expect(dialog).toBeHidden();
  expect(invitationRequests).toHaveLength(1);
  // The route API exposes the protobuf body as a string. This request is ASCII.
  const sent = fromBinary(
    CreateInvitationRequestSchema,
    new TextEncoder().encode(invitationRequests[0]),
  );
  expect(sent.email).toBe("a.b@example.test");
});
