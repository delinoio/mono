// SPDX-License-Identifier: Apache-2.0
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { create } from "@bufbuild/protobuf";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { Code, ConnectError, type Transport } from "@connectrpc/connect";
import { ListOrganizationsResponseSchema } from "@delino/ownsand-protos/v1/organization_pb";
import { ErrorNotice } from "./components/ui.tsx";
import { MOCK_IDS, createMockTransport } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

const UNKNOWN_ORG = "0193a000-0000-7000-8000-0000000000ff";
const UNKNOWN_ID = "0193a000-0000-7000-8000-0000000000fe";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
});

/** A transport that fails the named method with the given code. */
function failing(method: string, code: Code): Transport {
  const mock = createMockTransport();
  return {
    ...mock,
    async unary(m, ...rest) {
      if (m.name === method) throw new ConnectError("it failed", code);
      return mock.unary(m, ...rest);
    },
  };
}

describe("ErrorNotice retry", () => {
  test.each([
    [Code.NotFound, false],
    [Code.InvalidArgument, false],
    [Code.Unavailable, true],
    [Code.PermissionDenied, true],
    [Code.Internal, true],
  ])("code %i offers Try again: %s", (code, offered) => {
    render(
      <ErrorNotice error={new ConnectError("x", code)} onRetry={() => {}} />,
    );
    expect(!!screen.queryByRole("button", { name: "Try again" })).toBe(offered);
  });

  test("renders its children after the buttons", () => {
    render(
      <ErrorNotice
        error={new ConnectError("x", Code.Unavailable)}
        onRetry={() => {}}
      >
        <a href="/">Elsewhere</a>
      </ErrorNotice>,
    );
    expect(screen.getByRole("link", { name: "Elsewhere" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });
});

describe("unknown resources", () => {
  test("an unknown device shows no Try again", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/devices/${UNKNOWN_ID}`);
    await screen.findByText(/It was not found/);
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  });

  test("an unknown sandbox shows no Try again", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes/${UNKNOWN_ID}`);
    await screen.findByText(/It was not found/);
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  });
});

describe("organization failure", () => {
  test("an unknown organization links to the organization list and shows the placeholder", async () => {
    renderConsole(`/orgs/${UNKNOWN_ORG}/sandboxes`);
    const link = await screen.findByRole("link", {
      name: "Go to your organizations",
    });
    expect(link.getAttribute("href")).toBe("/");
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    const trigger = await screen.findByRole("combobox", {
      name: "Organization",
    });
    expect(trigger.textContent).toContain("Choose an organization");
    expect(trigger.textContent).not.toContain(UNKNOWN_ORG);
  });

  test("the link leaves the failed organization", async () => {
    renderConsole(`/orgs/${UNKNOWN_ORG}/sandboxes`);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("link", { name: "Go to your organizations" }),
      );
    });
    expect(window.location.pathname).toBe("/");
    expect(
      await screen.findByRole("heading", { name: "Organizations", level: 1 }),
    ).toBeTruthy();
  });

  test("choosing an organization from the placeholder keeps the section", async () => {
    renderConsole(`/orgs/${UNKNOWN_ORG}/snapshots`);
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("option", { name: "Northwind Labs" }),
      );
    });
    await settle(30);
    expect(window.location.pathname).toBe(
      `/orgs/${MOCK_IDS.northwind}/snapshots`,
    );
  });

  test("a listed organization shows its name", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`);
    const trigger = await screen.findByRole("combobox", {
      name: "Organization",
    });
    await settle(30);
    expect(trigger.textContent).toContain("Acme Robotics");
    expect(trigger.textContent).not.toContain("Choose an organization");
  });

  test("a transient failure keeps Try again and adds the link", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: failing("GetOrganization", Code.Unavailable),
    });
    expect(
      await screen.findByRole("button", { name: "Try again" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Go to your organizations" }),
    ).toBeTruthy();
  });

  test("a permission failure links to the organization list", async () => {
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, {
      transport: failing("GetOrganization", Code.PermissionDenied),
    });
    expect(
      await screen.findByRole("link", { name: "Go to your organizations" }),
    ).toBeTruthy();
  });

  test("a failed organization continuation keeps the route ID in the switcher", async () => {
    // The first page lacks the route organization and a later page fails, so
    // the organization may exist on a page that did not load.
    const mock = createMockTransport();
    const pageTokens: string[] = [];
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        if (method.name !== "ListOrganizations") {
          return mock.unary(method, ...rest);
        }
        const token = (rest[3] as { pageToken: string }).pageToken;
        pageTokens.push(token);
        if (token !== "") throw new ConnectError("page", Code.Unavailable);
        const res = await mock.unary(method, ...rest);
        return {
          ...res,
          message: create(ListOrganizationsResponseSchema, {
            organizations: [],
            nextPageToken: "page-2",
          }),
        } as unknown as typeof res;
      },
    };
    renderConsole(`/orgs/${MOCK_IDS.acme}/sandboxes`, { transport });
    await act(async () => {
      fireEvent.click(
        await screen.findByRole("combobox", { name: "Organization" }),
      );
    });
    expect(
      await screen.findByRole("option", {
        name: "Some organizations did not load. Try again",
      }),
    ).toBeTruthy();
    expect(pageTokens).toEqual(["", "page-2"]);
    const trigger = screen.getByRole("combobox", {
      name: "Organization",
    });
    expect(trigger.textContent).not.toContain("Choose an organization");
  });
});
