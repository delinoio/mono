// SPDX-License-Identifier: Apache-2.0
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { MOCK_IDS } from "../src/mock/mock-api.ts";
import { SandboxTransitionOutcome } from "./sandbox-transition-mock.ts";

const sandboxPage = test.extend<{
  openSandbox(
    sandboxId: string,
    outcome?: SandboxTransitionOutcome,
  ): Promise<void>;
}>({
  openSandbox: async ({ app, browser, screen }, use) => {
    await use(async (sandboxId, outcome) => {
      await app.open("/");
      await screen.getByRole("button", "Sign in with Delino").tap();
      await expect(
        screen.getByRole("link", "Acme Robotics Owner"),
      ).toBeVisible();
      if (outcome !== undefined) {
        await browser.evaluate((value) => {
          sessionStorage.setItem("ownsand-e2e-sandbox-transition", value);
          return true;
        }, outcome);
      }
      await app.open(`/orgs/${MOCK_IDS.acme}/sandboxes/${sandboxId}`);
    });
  },
});

describe("sandbox transitions", () => {
  for (const {
    action,
    nextAction,
    sandboxId,
    initialState,
    finalState,
    completion,
  } of [
    {
      action: "Pause",
      nextAction: "Resume",
      sandboxId: MOCK_IDS.runningSandbox,
      initialState: "Running",
      finalState: "Paused",
      completion: "The sandbox is paused.",
    },
    {
      action: "Resume",
      nextAction: "Pause",
      sandboxId: MOCK_IDS.pausedSandbox,
      initialState: "Paused",
      finalState: "Running",
      completion: "The sandbox is running.",
    },
  ]) {
    const requested = `${action} requested. Check the sandbox status.`;
    const evidence = action.toLowerCase();

    sandboxPage(
      `shows completion when ${action} is confirmed`,
      async ({ app, openSandbox, screen }) => {
        await openSandbox(sandboxId);
        await screen.getByRole("button", action).tap();

        await expect(screen.getByText(completion)).toBeVisible();
        await expect(screen.getByText(finalState)).toBeVisible();
        await expect(screen.getByRole("button", nextAction)).toBeVisible();
        await app.screenshot(`sandbox-${evidence}-confirmed`);
      },
    );

    sandboxPage(
      `shows a request notice when ${action} returns no sandbox`,
      async ({ app, openSandbox, screen }) => {
        await openSandbox(sandboxId, SandboxTransitionOutcome.Missing);
        await screen.getByRole("button", action).tap();

        await expect(screen.getByText(requested)).toBeVisible();
        await expect(screen.getByText(completion)).toBeHidden();
        await expect(screen.getByText(initialState)).toBeVisible();
        await expect(screen.getByRole("button", action)).toBeVisible();
        await app.screenshot(`sandbox-${evidence}-missing`);
      },
    );

    sandboxPage(
      `keeps the existing error when ${action} is rejected`,
      async ({ app, openSandbox, screen }) => {
        await openSandbox(sandboxId, SandboxTransitionOutcome.Rejected);
        await screen.getByRole("button", action).tap();

        await expect(
          screen.getByText(
            "OwnSand is not available right now. Try again in a moment.",
          ),
        ).toBeVisible();
        await expect(screen.getByText(requested)).toBeHidden();
        await expect(screen.getByText(completion)).toBeHidden();
        await expect(screen.getByRole("button", action)).toBeVisible();
        await app.screenshot(`sandbox-${evidence}-rejected`);
      },
    );

    sandboxPage(
      `waits for the response and refresh when ${action} returns the previous state`,
      async ({ app, browser, openSandbox, screen }) => {
        await openSandbox(sandboxId, SandboxTransitionOutcome.Pending);
        const button = screen.getByRole("button", action);
        await button.tap();
        await expect(button).toHaveAttribute("aria-busy", "true");

        async function release(phase: "mutation" | "refresh") {
          await browser.evaluate((heldPhase) => {
            if (
              sessionStorage.getItem("ownsand-e2e-sandbox-held") !== heldPhase
            ) {
              throw new Error(`No held sandbox ${heldPhase}`);
            }
            window.dispatchEvent(
              new Event(`ownsand-e2e-release-sandbox-${heldPhase}`),
            );
            return true;
          }, phase);
        }

        await browser.keyboard.press("Enter");
        await release("mutation");
        await expect
          .poll(() =>
            browser.evaluate(() =>
              sessionStorage.getItem("ownsand-e2e-sandbox-held"),
            ),
          )
          .toBe("refresh");
        await expect(button).toHaveAttribute("aria-busy", "true");
        await expect(screen.getByText(requested)).toBeHidden();
        await browser.keyboard.press("Space");
        await app.screenshot(`sandbox-${evidence}-refreshing`);

        await release("refresh");
        await expect(screen.getByText(requested)).toBeVisible();
        await expect(screen.getByText(completion)).toBeHidden();
        await expect(screen.getByText(initialState)).toBeVisible();
        await expect(button).toBeVisible();
        expect(
          await browser.evaluate(() =>
            sessionStorage.getItem("ownsand-e2e-sandbox-transition-count"),
          ),
        ).toBe("1");
        await app.screenshot(`sandbox-${evidence}-pending`);
      },
    );
  }
});
