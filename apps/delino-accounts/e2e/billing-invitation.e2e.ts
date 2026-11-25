// SPDX-License-Identifier: Apache-2.0
import { generateKeyPairSync, sign } from "node:crypto";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  AcceptInvitationRequestSchema,
  AcceptInvitationResponseSchema,
  BillingAccountRole,
  CreateInvitationRequestSchema,
  GetBillingAccountResponseSchema,
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

async function captureAcceptances(browser: Browser) {
  const accepted: string[] = [];
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
    let body: Uint8Array = new Uint8Array();
    if (method === "AcceptInvitation") {
      accepted.push(
        fromBinary(
          AcceptInvitationRequestSchema,
          Buffer.from(route.request.postData ?? ""),
        ).invitationToken,
      );
      body = toBinary(
        AcceptInvitationResponseSchema,
        create(AcceptInvitationResponseSchema, { billingAccount: ACCOUNT }),
      );
    } else if (method === "GetBillingAccount") {
      body = toBinary(
        GetBillingAccountResponseSchema,
        create(GetBillingAccountResponseSchema, { billingAccount: ACCOUNT }),
      );
    }
    await route.fulfill({ contentType: "application/proto", headers, body });
  });
  return accepted;
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
      const accepted = await captureAcceptances(browser);
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
        screen.getByRole("heading", ACCOUNT.name, { level: 1 }),
      ).toBeVisible();
      expect(accepted).toEqual([
        replacement === "none" ? FIRST_TOKEN : REPLACEMENT_TOKEN,
      ]);
      await expect.poll(() => storedToken(browser)).toBe(null);
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
