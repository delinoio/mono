// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, screen, waitFor } from "@testing-library/react";
import { ConnectError, Code, type Transport } from "@connectrpc/connect";
import { SandboxSourceSchema } from "@delino/ownsand-protos/v1/sandbox_pb";
import { create } from "@bufbuild/protobuf";
import { BaseImage } from "@delino/ownsand-protos/v1/common_pb";
import { shortIdText } from "./components/ui.tsx";
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import { renderConsole, settle } from "./test-support.tsx";

afterEach(() => {
  cleanup();
});

type Source = { case: "snapshotId" | "templateId"; value: string };

/**
 * A mock transport whose `GetSandbox` answers with `source`, so that the test
 * needs no create dialog, and whose `ListTemplates` is counted and can fail
 * or wait for the test. The mock keeps its own sandbox unchanged.
 */
function sourceTransport(
  source: Source | { case: "baseImage"; value: BaseImage },
  options: { failTemplates?: boolean; holdTemplates?: boolean } = {},
) {
  const mock = createMockTransport();
  const state = { listTemplates: 0, release: () => {} };
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      if (method.name === "ListTemplates") {
        state.listTemplates += 1;
        if (options.failTemplates === true) {
          throw new ConnectError("templates unavailable", Code.Unavailable);
        }
        if (options.holdTemplates === true) {
          await new Promise<void>((resolve) => {
            state.release = resolve;
          });
        }
      }
      const response = await mock.unary(method, ...rest);
      const { sandbox } = response.message as {
        sandbox?: { source?: unknown };
      };
      if (method.name === "GetSandbox" && sandbox) {
        sandbox.source = create(SandboxSourceSchema, {
          source: source as never,
        });
      }
      return response;
    },
  };
  return { transport, state };
}

function sandboxPath(): string {
  return `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`;
}

/** The `<dd>` of the Source row. */
async function sourceRow(): Promise<HTMLElement> {
  const term = await screen.findByText("Source");
  return term.nextElementSibling as HTMLElement;
}

describe("sandbox source row", () => {
  test("shows the template name with the template ID in its title", async () => {
    const { transport } = sourceTransport({
      case: "templateId",
      value: MOCK_IDS.template,
    });
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    await waitFor(() =>
      expect(row.textContent).toBe("Template node-24-toolchain"),
    );
    expect(screen.getByText("node-24-toolchain").title).toBe(MOCK_IDS.template);
  });

  test("shows a snapshot as the short ID with the full ID in its title", async () => {
    const { transport, state } = sourceTransport({
      case: "snapshotId",
      value: MOCK_IDS.snapshot,
    });
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    expect(row.textContent).toBe(`Snapshot ${shortIdText(MOCK_IDS.snapshot)}`);
    expect(row.textContent).not.toContain(MOCK_IDS.snapshot);
    expect(screen.getByTitle(MOCK_IDS.snapshot).textContent).toBe(
      shortIdText(MOCK_IDS.snapshot),
    );
    await settle();
    expect(state.listTemplates).toBe(0);
  });

  test("shows the short ID while the templates load, then the name", async () => {
    const { transport, state } = sourceTransport(
      { case: "templateId", value: MOCK_IDS.template },
      { holdTemplates: true },
    );
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    await waitFor(() => expect(state.listTemplates).toBe(1));
    expect(row.textContent).toBe(`Template ${shortIdText(MOCK_IDS.template)}`);
    expect(screen.queryByText(/Loading/)).toBeNull();
    state.release();
    await waitFor(() =>
      expect(row.textContent).toBe("Template node-24-toolchain"),
    );
  });

  test("shows the short ID when the template is not in the list", async () => {
    const missing = "0193a000-0000-7000-8000-00000000ffff";
    const { transport, state } = sourceTransport({
      case: "templateId",
      value: missing,
    });
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    await waitFor(() => expect(state.listTemplates).toBe(1));
    await settle();
    expect(row.textContent).toBe(`Template ${shortIdText(missing)}`);
    expect(screen.getByTitle(missing)).toBeTruthy();
  });

  test("shows the short ID without an error notice when the lookup fails", async () => {
    const { transport, state } = sourceTransport(
      { case: "templateId", value: MOCK_IDS.template },
      { failTemplates: true },
    );
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    await waitFor(() => expect(state.listTemplates).toBe(1));
    await settle();
    expect(row.textContent).toBe(`Template ${shortIdText(MOCK_IDS.template)}`);
    expect(screen.queryByText(/templates unavailable/)).toBeNull();
    expect(screen.queryByRole("button", { name: /try again/i })).toBeNull();
    expect(state.listTemplates).toBe(1);
  });

  test("keeps the base image text and requests no templates", async () => {
    const { transport, state } = sourceTransport({
      case: "baseImage",
      value: BaseImage.LINUX,
    });
    renderConsole(sandboxPath(), { transport });
    const row = await sourceRow();
    expect(row.textContent).toBe("Linux base image");
    expect(row.textContent).not.toContain("Template");
    await settle();
    expect(state.listTemplates).toBe(0);
  });
});
