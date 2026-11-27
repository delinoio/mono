// SPDX-License-Identifier: Apache-2.0
import { afterEach, describe, expect, test } from "@rstest/core";
import { act, cleanup, render, within } from "@testing-library/react";
import { renderToString } from "react-dom/server";
import { Switch } from "./switch.tsx";
import { Textarea } from "./textarea.tsx";
import { TextField } from "./text-field.tsx";

afterEach(cleanup);

const FIELD_CASES = [
  ["TextField", TextField, "textbox", <input id="custom-control" />],
  ["Textarea", Textarea, "textbox", <textarea id="custom-control" />],
  ["Switch", Switch, "switch", <button id="custom-control" />],
] as const;

const LABEL = "Example setting";
const DESCRIPTION = "Example help";

describe.each(FIELD_CASES)(
  "%s native label",
  (_name, Component, role, custom) => {
    test.each([
      ["a caller ID", "example-control"],
      ["a generated ID", undefined],
    ])(
      "associates the default control with %s before and after hydration",
      async (_case, id) => {
        const field = (
          <Component id={id} label={LABEL} description={DESCRIPTION} />
        );
        const container = document.createElement("div");
        container.innerHTML = renderToString(field);
        document.body.append(container);

        const options = { name: LABEL, description: DESCRIPTION };
        const control = within(container).getByRole(role, options);
        const label = container.querySelector("label");
        expect(control.id).toBeTruthy();
        expect(label?.control).toBe(control);
        if (id !== undefined) {
          expect(control.id).toBe(id);
        }

        render(field, { container, hydrate: true });
        await act(async () => {});

        expect(within(container).getByRole(role, options)).toBe(control);
        expect(label?.control).toBe(control);
      },
    );

    test("uses the custom control's ID after hydration", async () => {
      const field = (
        <Component
          id="example-control"
          label={LABEL}
          description={DESCRIPTION}
          render={custom}
        />
      );
      const container = document.createElement("div");
      container.innerHTML = renderToString(field);
      document.body.append(container);

      const { getByRole } = render(field, { container, hydrate: true });
      await act(async () => {});
      const control = getByRole(role, {
        name: LABEL,
        description: DESCRIPTION,
      });
      expect(control.id).toBe("custom-control");
      expect(container.querySelector("label")?.control).toBe(control);
    });
  },
);
