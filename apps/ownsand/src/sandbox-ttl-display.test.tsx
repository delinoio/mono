// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { cleanup, render, screen } from "@testing-library/react";
import { type Transport } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";
import { type Sandbox } from "@delino/ownsand-protos/v1/sandbox_pb";
import { Console } from "./App.tsx";
import { type Auth, AuthContextProvider } from "./auth.tsx";
import { AuthMode } from "./config.ts";
import { createMockTransport, MOCK_IDS, MOCK_USER } from "./mock/mock-api.ts";

const auth: Auth = {
  isLoading: false,
  isAuthenticated: true,
  userId: MOCK_USER,
  signIn: async () => {},
  signInFailed: false,
  signOut: async () => {},
  getAccessToken: async () => "token",
};

afterEach(() => {
  cleanup();
});

/** Reads the text of the `<dd>` that follows the `<dt>` named `term`. */
function detailText(term: string): string | null | undefined {
  return screen.getByText(term).nextElementSibling?.textContent;
}

describe("sandbox TTL rows", () => {
  test("show a positive subsecond TTL as a positive duration", async () => {
    const mock = createMockTransport();
    const transport: Transport = {
      ...mock,
      async unary(method, ...rest) {
        const response = await mock.unary(method, ...rest);
        // The response is a copy, so the mock's own sandbox keeps its TTLs.
        const { sandbox } = response.message as { sandbox?: Sandbox };
        if (method.name === "GetSandbox" && sandbox) {
          sandbox.idleTtl = { seconds: 0n, nanos: 500_000_000 } as never;
          sandbox.maxTtl = { seconds: 0n, nanos: 1 } as never;
        }
        return response;
      },
    };
    window.history.replaceState(
      null,
      "",
      `/orgs/${MOCK_IDS.acme}/sandboxes/${MOCK_IDS.runningSandbox}`,
    );
    render(
      <AuthContextProvider value={auth}>
        <Console
          config={{
            apiOrigin: "https://api.test",
            authMode: AuthMode.Mock,
            logto: undefined,
          }}
          transport={transport}
          queryClient={
            new QueryClient({
              defaultOptions: { queries: { retry: false } },
            })
          }
        />
      </AuthContextProvider>,
    );
    expect(await screen.findByText("Idle TTL")).toBeTruthy();
    expect(detailText("Idle TTL")).toBe("500 ms");
    expect(detailText("Maximum TTL")).toBe("<1 ms");
  });
});
