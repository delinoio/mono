// SPDX-License-Identifier: Apache-2.0
import { Select, SelectOption } from "./select.tsx";

export function StringSelectOptions() {
  // @ts-expect-error SelectOption requires a string value.
  void (<SelectOption value={7}>Seven</SelectOption>);
  // @ts-expect-error Object values are not supported.
  void (<SelectOption value={{ id: "7" }}>Seven</SelectOption>);
  // @ts-expect-error The value prop is required.
  void (<SelectOption>Seven</SelectOption>);

  return (
    <Select
      label="Plan"
      onValueChange={(value) => {
        const selected: string | null = value;
        // @ts-expect-error The callback value can be null.
        value.toUpperCase();
        selected?.toUpperCase();
      }}
    >
      <SelectOption value="7">{7}</SelectOption>
      <SelectOption value="8">{8n}</SelectOption>
    </Select>
  );
}
