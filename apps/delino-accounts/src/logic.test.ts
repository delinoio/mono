// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import {
  checkAccountName,
  checkAlertThreshold,
  checkAmount,
  checkAutoTopUp,
  checkEmail,
  checkTopUpAmount,
} from "./forms.ts";
import {
  addMonths,
  formatDate,
  formatMonth,
  formatQuantity,
  meterLabel,
} from "./format.ts";
import { usd } from "./money.ts";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { BillingAccountRole } from "@delino/accounts-protos/v1/billing_account_pb";
import { Meter } from "@delino/accounts-protos/v1/usage_pb";
import {
  PENDING_INVITATION_KEY,
  pendingInvitation,
  openInvitationFromAddress,
  takeTokenFromAddress,
  tokenFromFragment,
} from "./invitation.ts";
import { can, canManageMember, Permission } from "./roles.ts";
import { parseRoute, RouteId, routePath } from "./router.tsx";
import { uuidV7 } from "./uuid.ts";

describe("routes", () => {
  test.each([
    ["/", { id: RouteId.Home }],
    ["/billing", { id: RouteId.Billing }],
    ["/billing/", { id: RouteId.Billing }],
    ["/billing/invitations/accept", { id: RouteId.BillingInvitation }],
    ["/billing/invitations", { id: RouteId.NotFound }],
    ["/billing/abc", { id: RouteId.BillingAccount, billingAccountId: "abc" }],
    ["/billing/abc/def", { id: RouteId.NotFound }],
    ["/callback", { id: RouteId.Callback }],
    ["/account", { id: RouteId.NotFound }],
    ["/billing/%E0%A4%A", { id: RouteId.NotFound }],
  ])("parses %s", (path, route) => {
    expect(parseRoute(path)).toEqual(route);
  });

  test("builds paths that parse back to the same route", () => {
    for (const route of [
      { id: RouteId.Home },
      { id: RouteId.Billing },
      { id: RouteId.BillingInvitation },
      { id: RouteId.Callback },
      { id: RouteId.BillingAccount, billingAccountId: "a/b c" },
    ] as const) {
      expect(parseRoute(routePath(route))).toEqual(route);
    }
  });
});

describe("roles", () => {
  const { OWNER, ADMIN, VIEWER, UNSPECIFIED } = BillingAccountRole;

  test.each([
    [Permission.DeleteAccount, [OWNER]],
    [Permission.TransferOwnership, [OWNER]],
    [Permission.ManagePayments, [OWNER, ADMIN]],
    [Permission.ManageMembers, [OWNER, ADMIN]],
    [Permission.ReadInvoices, [OWNER, ADMIN]],
    [Permission.Leave, [ADMIN, VIEWER]],
  ])("%s is allowed only for %j", (permission, allowed) => {
    for (const role of [OWNER, ADMIN, VIEWER, UNSPECIFIED]) {
      expect(can(role, permission)).toBe(
        (allowed as BillingAccountRole[]).includes(role),
      );
    }
    expect(can(undefined, permission)).toBe(false);
  });

  test("owners and admins manage admins and viewers, never the owner", () => {
    expect(canManageMember(OWNER, ADMIN)).toBe(true);
    expect(canManageMember(ADMIN, VIEWER)).toBe(true);
    expect(canManageMember(ADMIN, OWNER)).toBe(false);
    expect(canManageMember(OWNER, OWNER)).toBe(false);
    expect(canManageMember(VIEWER, VIEWER)).toBe(false);
  });
});

describe("forms", () => {
  test("checks billing account names like the server", () => {
    expect(checkAccountName("  Acme  ")).toEqual({ ok: true, value: "Acme" });
    expect(checkAccountName("é".repeat(100)).ok).toBe(true);
    // 100 code points, 200 UTF-16 units.
    expect(checkAccountName("😀".repeat(100)).ok).toBe(true);
    expect(checkAccountName("😀".repeat(101)).ok).toBe(false);
    expect(checkAccountName("").ok).toBe(false);
    expect(checkAccountName("a\u0007b").ok).toBe(false);
  });

  test("trims the white space of Go's strings.TrimSpace from account names", () => {
    // U+0085 is white space for Go but not for String.prototype.trim.
    expect(checkAccountName("\u0085Acme")).toEqual({
      ok: true,
      value: "Acme",
    });
    expect(checkAccountName("Acme\u0085")).toEqual({
      ok: true,
      value: "Acme",
    });
    expect(checkAccountName("\u0085").ok).toBe(false);
    expect(checkAccountName("\u0085")).toEqual({
      ok: false,
      error: "Enter a name.",
    });
    // Every other member of unicode.IsSpace.
    const spaces =
      "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000";
    expect(checkAccountName(`${spaces}Acme${spaces}`)).toEqual({
      ok: true,
      value: "Acme",
    });
    expect(checkAccountName(spaces)).toEqual({
      ok: false,
      error: "Enter a name.",
    });
    // Inside the name, U+0085 is a control character and still rejected.
    expect(checkAccountName("A\u0085c")).toEqual({
      ok: false,
      error: "Remove control characters from the name.",
    });
    expect(checkAccountName("A\u00a0c")).toEqual({
      ok: true,
      value: "A\u00a0c",
    });
    // U+FEFF is not white space for Go: it is kept, counted, and (as a
    // format character, not a control character) accepted.
    expect(checkAccountName("\ufeffAcme\ufeff")).toEqual({
      ok: true,
      value: "\ufeffAcme\ufeff",
    });
    expect(checkAccountName("\ufeff".repeat(100)).ok).toBe(true);
    expect(checkAccountName("\ufeff".repeat(101)).ok).toBe(false);
    expect(checkAccountName("\ufeff")).toEqual({
      ok: true,
      value: "\ufeff",
    });
  });

  test("checks invitation email addresses", () => {
    expect(checkEmail(" \tA@example.com\u0085")).toEqual({
      ok: true,
      value: "A@example.com",
    });
    for (const email of [
      "first.last+tag@example.co.uk",
      "a@b",
      "o'neil@host_name",
      "用户@例子.测试",
      "a@[192.0.2.1]",
      "a@[2001:db8::1]",
      "a@[::ffff:c000:201]",
      `${"é".repeat(126)}@b`,
    ]) {
      expect(checkEmail(email)).toEqual({ ok: true, value: email });
    }

    for (const email of [
      "nobody",
      "a@",
      "@b",
      "a@b@c",
      "a b@c",
      "a..b@example.test",
      ".a@b",
      "a.@b",
      "a@b..c",
      "a@.b",
      "a@b.",
      "<a@b>",
      "‹a@b›",
      "Someone <a@b>",
      '"a"@b',
      "a@[01.2.3.4]",
      "a@[256.2.3.4]",
      "a@[2001:db8::x]",
      "a@[IPv6:2001:db8::1]",
      "a@[fe80::1%eth0]",
      `${"a".repeat(250)}@b.co`,
      `${"é".repeat(126)}@bb`,
    ]) {
      expect(checkEmail(email)).toEqual({
        ok: false,
        error: "Enter an email address like name@example.com.",
      });
    }
  });

  test("checks the manual top-up limits with bigint micro-USD", () => {
    expect(checkTopUpAmount("25")).toEqual({ ok: true, value: 25_000_000n });
    expect(checkTopUpAmount("10,000.00")).toEqual({
      ok: true,
      value: 10_000_000_000n,
    });
    expect(checkTopUpAmount("24.99").ok).toBe(false);
    expect(checkTopUpAmount("10000.01").ok).toBe(false);
    expect(checkTopUpAmount("30.005").ok).toBe(false);
  });

  test("checks automatic top-up settings when enabled", () => {
    expect(
      checkAutoTopUp({
        enabled: true,
        threshold: "10",
        amount: "25",
        monthlyCap: "25",
      }),
    ).toEqual({
      ok: true,
      value: {
        enabled: true,
        thresholdUsdMicros: 10_000_000n,
        amountUsdMicros: 25_000_000n,
        monthlyCapUsdMicros: 25_000_000n,
      },
    });
    const result = checkAutoTopUp({
      enabled: true,
      threshold: "9.99",
      amount: "24.99",
      monthlyCap: "",
    });
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(Object.keys(result.errors).sort()).toEqual([
        "amount",
        "monthlyCap",
        "threshold",
      ]);
    }
  });

  test("limits an enabled automatic top-up amount to $10,000", () => {
    const withAmount = (amount: string, enabled = true) =>
      checkAutoTopUp({
        enabled,
        threshold: "10",
        amount,
        monthlyCap: "20000",
      });
    expect(withAmount("10000")).toEqual({
      ok: true,
      value: {
        enabled: true,
        thresholdUsdMicros: 10_000_000n,
        amountUsdMicros: 10_000_000_000n,
        monthlyCapUsdMicros: 20_000_000_000n,
      },
    });
    expect(withAmount("10000.01")).toEqual({
      ok: false,
      errors: { amount: "Enter at most $10,000.00." },
    });
    // A disabled setting keeps any whole-cent amount for a later enable.
    expect(withAmount("15000", false)).toEqual({
      ok: true,
      value: {
        enabled: false,
        thresholdUsdMicros: 10_000_000n,
        amountUsdMicros: 15_000_000_000n,
        monthlyCapUsdMicros: 20_000_000_000n,
      },
    });
  });

  test("keeps disabled automatic top-up values with zero for empty fields", () => {
    expect(
      checkAutoTopUp({
        enabled: false,
        threshold: "",
        amount: "5",
        monthlyCap: "",
      }),
    ).toEqual({
      ok: true,
      value: {
        enabled: false,
        thresholdUsdMicros: 0n,
        amountUsdMicros: 5_000_000n,
        monthlyCapUsdMicros: 0n,
      },
    });
    expect(
      checkAutoTopUp({
        enabled: false,
        threshold: "1.001",
        amount: "",
        monthlyCap: "",
      }).ok,
    ).toBe(false);
  });

  test("limits every amount to the largest whole-cent int64 value", () => {
    const atMost = "Enter at most $9,223,372,036,854.77.";
    expect(checkAmount("9223372036854.77")).toEqual({
      ok: true,
      value: 9_223_372_036_854_770_000n,
    });
    expect(checkAmount("9223372036854.78")).toEqual({
      ok: false,
      error: atMost,
    });
    expect(checkAmount("99999999999999")).toEqual({ ok: false, error: atMost });
    expect(checkAlertThreshold("9223372036854.77").ok).toBe(true);
    expect(checkAlertThreshold("9223372036854.78")).toEqual({
      ok: false,
      error: atMost,
    });
    // A smaller limit wins: the top-up amount keeps its $10,000 maximum.
    expect(checkAmount("10001", { max: usd(10_000n) })).toEqual({
      ok: false,
      error: "Enter at most $10,000.00.",
    });
    expect(checkTopUpAmount("10001")).toEqual({
      ok: false,
      error: "Enter at most $10,000.00.",
    });
    // A limit above the int64 bound does not raise it.
    expect(checkAmount("9223372036854.78", { max: 10n ** 19n })).toEqual({
      ok: false,
      error: atMost,
    });
  });

  test("limits every automatic top-up amount to the int64 bound", () => {
    const atMost = "Enter at most $9,223,372,036,854.77.";
    const big = "9223372036854.78";
    expect(
      checkAutoTopUp({
        enabled: true,
        threshold: big,
        amount: "25",
        monthlyCap: big,
      }),
    ).toEqual({
      ok: false,
      errors: { threshold: atMost, monthlyCap: atMost },
    });
    expect(
      checkAutoTopUp({
        enabled: true,
        threshold: "10",
        amount: "10001",
        monthlyCap: "99999999999999",
      }),
    ).toEqual({
      ok: false,
      errors: { amount: "Enter at most $10,000.00.", monthlyCap: atMost },
    });
    // The bound also applies to the kept values of a disabled setting.
    expect(
      checkAutoTopUp({
        enabled: false,
        threshold: "",
        amount: "",
        monthlyCap: big,
      }),
    ).toEqual({ ok: false, errors: { monthlyCap: atMost } });
    expect(
      checkAutoTopUp({
        enabled: false,
        threshold: "9223372036854.77",
        amount: "",
        monthlyCap: "9223372036854.77",
      }),
    ).toEqual({
      ok: true,
      value: {
        enabled: false,
        thresholdUsdMicros: 9_223_372_036_854_770_000n,
        amountUsdMicros: 0n,
        monthlyCapUsdMicros: 9_223_372_036_854_770_000n,
      },
    });
  });

  test("accepts an alert threshold of $0 and whole cents only", () => {
    expect(checkAlertThreshold("0")).toEqual({ ok: true, value: 0n });
    expect(checkAlertThreshold("10.5")).toEqual({
      ok: true,
      value: 10_500_000n,
    });
    expect(checkAlertThreshold("-1").ok).toBe(false);
    expect(checkAlertThreshold("0.001").ok).toBe(false);
  });
});

describe("format", () => {
  // Guards the TZ pin in rstest.config.ts: fixtures use UTC-midnight
  // timestamps that `formatDate` renders in the process time zone.
  test("runs in UTC", () => {
    expect(new Date().getTimezoneOffset()).toBe(0);
    expect(
      formatDate(timestampFromDate(new Date("2026-12-01T00:00:00Z"))),
    ).toBe("Dec 1, 2026");
  });

  test("moves and names UTC months", () => {
    expect(addMonths({ year: 2026, month: 1 }, -1)).toEqual({
      year: 2025,
      month: 12,
    });
    expect(addMonths({ year: 2026, month: 12 }, 1)).toEqual({
      year: 2027,
      month: 1,
    });
    expect(formatMonth({ year: 2026, month: 11 })).toBe("November 2026");
  });

  test("formats quantities with the unit of the meter", () => {
    expect(formatQuantity(1_048_576n, Meter.OWNSAND_RELAY_EGRESS)).toBe(
      "1,048,576 bytes",
    );
    expect(formatQuantity(5n, undefined)).toBe("5");
    expect(meterLabel("ownsand.class_b_rpc")).toBe("Class B API calls");
    expect(meterLabel("other.meter")).toBe("other.meter");
  });
});

describe("invitation token", () => {
  test("reads the token from the fragment and removes the fragment", () => {
    const replaced: string[] = [];
    const token = takeTokenFromAddress(
      {
        hash: "#token=bai_abc",
        pathname: "/billing/invitations/accept",
        search: "",
      },
      { replaceState: (_d, _t, url) => void replaced.push(String(url)) },
    );
    expect(token).toBe("bai_abc");
    expect(replaced).toEqual(["/billing/invitations/accept"]);
    expect(tokenFromFragment("#other=1")).toBeUndefined();
    expect(tokenFromFragment("#token=")).toBeUndefined();
  });

  test("drops the stored token only when the address carries a new one", () => {
    const history = { replaceState: () => {} };
    const at = (hash: string) => ({
      hash,
      pathname: "/billing/invitations/accept",
      search: "",
    });
    window.sessionStorage.clear();
    pendingInvitation.save("bai_old");
    expect(openInvitationFromAddress(at(""), history)).toBeUndefined();
    expect(pendingInvitation.read()).toBe("bai_old");
    expect(openInvitationFromAddress(at("#other=1"), history)).toBeUndefined();
    expect(pendingInvitation.read()).toBe("bai_old");
    expect(openInvitationFromAddress(at("#token="), history)).toBeUndefined();
    expect(pendingInvitation.read()).toBe("bai_old");
    expect(openInvitationFromAddress(at("#token=bai_new"), history)).toBe(
      "bai_new",
    );
    expect(pendingInvitation.read()).toBeUndefined();
    window.sessionStorage.clear();
  });

  test("stores the pending token under one key and clears it", () => {
    const map = new Map<string, string>();
    const storage = {
      getItem: (k: string) => map.get(k) ?? null,
      setItem: (k: string, v: string) => void map.set(k, v),
      removeItem: (k: string) => void map.delete(k),
    };
    pendingInvitation.save("bai_x", storage);
    expect([...map.keys()]).toEqual([PENDING_INVITATION_KEY]);
    expect(pendingInvitation.read(storage)).toBe("bai_x");
    pendingInvitation.clear(storage);
    expect(pendingInvitation.read(storage)).toBeUndefined();
  });
});

describe("uuidV7", () => {
  test("encodes the time and the version", () => {
    const id = uuidV7(0x0193a1b2c3d4);
    expect(id).toMatch(
      /^0193a1b2-c3d4-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
  });
});
