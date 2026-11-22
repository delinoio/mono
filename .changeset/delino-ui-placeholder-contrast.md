---
"delino-ui": patch
---

Draw the placeholder text of `TextField`, `Textarea`, `NumberInput` and `PasswordInput` with `--dui-color-text-muted` at full opacity, so it keeps a contrast ratio of at least 4.5:1 on the field surface in light and dark mode. The browser default placeholder color was only 3.60:1 on the dark surface.
