// SPDX-License-Identifier: Apache-2.0
import {
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { AgentVersionPolicyMode } from "@delino/ownsand-protos/v1/common_pb";
import {
  DeviceService,
  type UpdateAgentVersionPolicyRequest,
} from "@delino/ownsand-protos/v1/device_pb";
import { afterEach, beforeEach, describe, expect, test } from "@rstest/core";
import { QueryClient } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import { toast } from "delino-ui/toast";
import { createMockTransport, MOCK_IDS } from "./mock/mock-api.ts";
import { renderConsole } from "./test-support.tsx";

const SETTINGS = `/orgs/${MOCK_IDS.acme}/settings`;
const WAIT = { timeout: 10_000 };
const PIN = "Pin a version";
const HOLD = "Hold updates";
const DEFAULT = "Follow the default version";
const VERSION_ERROR = "Enter the agent version to pin, for example 1.4.2.";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  toast.dismiss();
});

function controlPlane() {
  const mock = createMockTransport();
  const server = createClient(DeviceService, mock);
  const state = {
    failReadback: false,
    failSaves: false,
    saves: [] as UpdateAgentVersionPolicyRequest[],
  };
  const transport: Transport = {
    ...mock,
    async unary(method, ...rest) {
      if (method.name === "GetAgentVersionPolicy" && state.failReadback) {
        throw new ConnectError("the policy readback failed", Code.Unavailable);
      }
      if (method.name === "UpdateAgentVersionPolicy") {
        state.saves.push(rest[3] as UpdateAgentVersionPolicyRequest);
        if (state.failSaves) {
          throw new ConnectError("the save failed", Code.Unavailable);
        }
      }
      return mock.unary(method, ...rest);
    },
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  const serverChange = (mode: AgentVersionPolicyMode, pinnedVersion = "") =>
    server.updateAgentVersionPolicy({
      organizationId: MOCK_IDS.acme,
      mode,
      pinnedVersion,
    });

  const refresh = async () => {
    await act(async () => {
      await queryClient.invalidateQueries();
    });
  };

  return {
    render: () => renderConsole(SETTINGS, { transport, queryClient }),
    refresh,
    serverChange,
    state,
  };
}

const selected = (label: string) =>
  screen.getByRole("radio", { name: label }).getAttribute("aria-checked");
const input = () => screen.getByLabelText("Pinned version") as HTMLInputElement;
const saveButton = () =>
  screen.getByRole("button", { name: "Save policy" }) as HTMLButtonElement;

async function selectMode(label: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("radio", { name: label }));
  });
}

async function editVersion(version: string) {
  await act(async () => {
    fireEvent.input(input(), { target: { value: version } });
    fireEvent.blur(input());
  });
}

async function save() {
  await act(async () => {
    fireEvent.click(saveButton());
  });
}

describe("agent version policy during a refresh", () => {
  test("keeps both edited values, sends them on save and clears the saved draft", async () => {
    const { render, refresh, serverChange, state } = controlPlane();
    render();
    await screen.findByRole("radio", { name: DEFAULT }, WAIT);
    await selectMode(PIN);
    await editVersion(" 1.4.2 ");

    await serverChange(AgentVersionPolicyMode.HOLD);
    await refresh();
    await screen.findByText(/^Last changed /, {}, WAIT);
    expect(selected(PIN)).toBe("true");
    expect(input().value).toBe(" 1.4.2 ");
    expect(state.saves).toHaveLength(0);

    await save();
    await screen.findByText("The agent version policy is saved.", {}, WAIT);
    expect(state.saves).toHaveLength(1);
    expect(state.saves[0]).toMatchObject({
      mode: AgentVersionPolicyMode.PIN,
      pinnedVersion: "1.4.2",
    });
    await waitFor(() => expect(input().value).toBe("1.4.2"), WAIT);
    expect(saveButton().disabled).toBe(false);

    await serverChange(AgentVersionPolicyMode.PIN, "2.0.0");
    await refresh();
    await waitFor(() => expect(input().value).toBe("2.0.0"), WAIT);
    await serverChange(AgentVersionPolicyMode.HOLD);
    await refresh();
    await waitFor(() => expect(selected(HOLD)).toBe("true"), WAIT);
  });

  test("follows successive server policies while the form is untouched", async () => {
    const { render, refresh, serverChange } = controlPlane();
    render();
    await screen.findByRole("radio", { name: DEFAULT }, WAIT);

    await serverChange(AgentVersionPolicyMode.PIN, "2.0.0");
    await refresh();
    await waitFor(() => {
      expect(selected(PIN)).toBe("true");
      expect(input().value).toBe("2.0.0");
    }, WAIT);

    await serverChange(AgentVersionPolicyMode.FOLLOW_DEFAULT);
    await refresh();
    await waitFor(() => {
      expect(selected(DEFAULT)).toBe("true");
      expect(screen.queryByLabelText("Pinned version")).toBeNull();
    }, WAIT);
  });

  test("keeps a mode edit and updates the untouched version and its validation", async () => {
    const { render, refresh, serverChange, state } = controlPlane();
    const invalidVersionInput = () =>
      screen.queryByRole("textbox", {
        name: "Pinned version",
        description: (description) => description.includes(VERSION_ERROR),
      });
    render();
    await screen.findByRole("radio", { name: DEFAULT }, WAIT);
    await selectMode(PIN);
    await save();
    expect(invalidVersionInput()).toBe(input());
    expect(state.saves).toHaveLength(0);

    await serverChange(AgentVersionPolicyMode.HOLD);
    await refresh();
    await screen.findByText(/^Last changed /, {}, WAIT);
    expect(selected(PIN)).toBe("true");
    expect(input().value).toBe("");
    expect(invalidVersionInput()).toBe(input());

    await serverChange(AgentVersionPolicyMode.PIN, "2.0.0");
    await refresh();
    await waitFor(() => expect(input().value).toBe("2.0.0"), WAIT);
    expect(invalidVersionInput()).toBeNull();

    await editVersion("");
    expect(invalidVersionInput()).toBeNull();
    await save();
    expect(invalidVersionInput()).toBe(input());
    await editVersion("1.4.2");
    expect(invalidVersionInput()).toBeNull();
  });

  test("follows an untouched mode and keeps an edited version when pinning returns", async () => {
    const { render, refresh, serverChange } = controlPlane();
    await serverChange(AgentVersionPolicyMode.PIN, "2.0.0");
    render();
    await screen.findByLabelText("Pinned version", {}, WAIT);
    await editVersion("1.4.2");

    await serverChange(AgentVersionPolicyMode.HOLD);
    await refresh();
    await waitFor(() => {
      expect(selected(HOLD)).toBe("true");
      expect(screen.queryByLabelText("Pinned version")).toBeNull();
    }, WAIT);

    await serverChange(AgentVersionPolicyMode.PIN, "3.0.0");
    await refresh();
    await waitFor(() => {
      expect(selected(PIN)).toBe("true");
      expect(input().value).toBe("1.4.2");
    }, WAIT);
  });

  test("shows the confirmed policy when a successful save has a failed readback", async () => {
    const { render, state } = controlPlane();
    render();
    await screen.findByRole("radio", { name: DEFAULT }, WAIT);
    await selectMode(PIN);
    await editVersion(" 1.4.2 ");

    state.failReadback = true;
    await save();
    await screen.findByText("The agent version policy is saved.", {}, WAIT);
    expect(screen.getByText(/The last refresh failed/)).toBeTruthy();
    expect(selected(PIN)).toBe("true");
    expect(input().value).toBe("1.4.2");
    expect(saveButton().disabled).toBe(false);
  });

  test("keeps the draft through a failed save and another refresh for a retry", async () => {
    const { render, refresh, serverChange, state } = controlPlane();
    render();
    await screen.findByRole("radio", { name: DEFAULT }, WAIT);
    await selectMode(PIN);
    await editVersion("1.4.2");

    state.failSaves = true;
    await save();
    await screen.findByText(
      "OwnSand is not available right now. Try again in a moment.",
      {},
      WAIT,
    );
    await waitFor(() => expect(saveButton().disabled).toBe(false), WAIT);
    expect(input().value).toBe("1.4.2");

    await serverChange(AgentVersionPolicyMode.HOLD);
    await refresh();
    await screen.findByText(/^Last changed /, {}, WAIT);
    expect(selected(PIN)).toBe("true");
    expect(input().value).toBe("1.4.2");

    state.failSaves = false;
    await save();
    await screen.findByText("The agent version policy is saved.", {}, WAIT);
    expect(state.saves).toHaveLength(2);
    for (const request of state.saves) {
      expect(request).toMatchObject({
        mode: AgentVersionPolicyMode.PIN,
        pinnedVersion: "1.4.2",
      });
    }
  });
});
