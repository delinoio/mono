# packages-delino-ui foundation contract

## Scope

This contract owns the [`delino-ui`](project-delino-ui.md) package at `packages/delino-ui/`:

- `packages/delino-ui/package.json`: package manifest, subpath exports and pinned dependencies.
- `packages/delino-ui/src/`: component source, the shared stylesheet `styles.css` and unit tests.
- `packages/delino-ui/rstest.config.ts`: unit test configuration.
- `packages/delino-ui/tsconfig.json`: TypeScript configuration for the type check.

## Runtime and Language

- Language: TypeScript with React 19 (`"jsx": "react-jsx"`).
- Component foundation: UI SDK, `@ui-sdk/core` and `@ui-sdk/react`, pinned to an exact version. UI SDK is headless: it renders unstyled parts with `data-slot` and state attributes such as `data-open`, `data-checked` and `data-pending`.
- Test runner: Rstest `@rstest/core` with the `happy-dom` environment and Testing Library.
- Runtime: the browser of the consuming app. Node.js from root `.nvmrc` runs the type check and tests. Rules that correct right-to-left layout use the `:dir()` pseudo-class (Chrome 120, Safari 16.4, Firefox 49 and newer). A consuming app whose build targets older browsers must keep `:dir()`: a build that lowers it to `:lang()` applies those rules only to pages whose `lang` is a right-to-left language.
- Distribution: TypeScript source through `package.json` `exports`. The package has no build script and writes no `dist` directory. Consumers compile it with Rsbuild or Rspress, which transpile workspace sources outside `node_modules`.
- Deviation from repository defaults: TypeScript instead of Go, because the package is consumed by React web apps.

## Users and Operators

- Maintainers of Delino web apps under `apps/` who build user interfaces.
- Users of those apps, who get consistent keyboard, focus and screen reader behavior.

## Interfaces and Contracts

Each export is a stable interface. Exported enums are the stable identifiers for variants and sizes.

- `delino-ui/button`
  - `Button`: a styled UI SDK `Button`. It accepts all UI SDK `Button` props. A Promise returned from `onClick` keeps the button pending (`data-pending`, `aria-busy`) until it settles, and further presses do nothing.
  - `ButtonVariant`: `Primary` (`primary`), `Secondary` (`secondary`), `Danger` (`danger`). The default is `Primary`.
  - `ButtonSize`: `Small` (`small`), `Medium` (`medium`), `Large` (`large`). The default is `Medium`.
  - `fullWidth`: stretches the button to the width of its container.
- `delino-ui/text-field`
  - `TextField`: a labeled text input. Props: `label` (required), `description`, `error`, and all UI SDK `Input` props such as `value`, `defaultValue`, `onValueChange`, `name`, `type`, `required` and `autoFocus`. The field connects the label, description and error to the input.
  - A non-empty `error` marks the field invalid and shows the message, unless the field is disabled: a disabled field shows no error text, so the text and the `aria-describedby` reference to it agree. The error element has a stable id, so the `aria-describedby` reference to it resolves in statically rendered markup, before any client code runs. Without `error`, the field shows the browser's validation message after a failed submit.
  - A `description` of `undefined`, `null`, `false` or `""` means no description: `TextField`, `Textarea`, `PasswordInput`, `NumberInput`, `Slider`, `Switch`, `Checkbox`, `RadioGroup` and `Select` show no description and add no `aria-describedby` reference that has no matching element in statically rendered markup. `Checkbox` renders no description element. The other components render an empty description element with the `hidden` attribute, so the id that the UI SDK predicts before the field binds still resolves, and the package styles give that element `display: none`, so the layout does not change. Any other value renders and is referenced, including the number `0`.
  - When the input is the single critical input of a form, the consumer must pass `autoFocus`. `autoFocus` also makes the input the initial focus target inside `Dialog`, even when another tabbable element such as the close button comes first. `TextField` sets `data-autofocus` on the input for this, because the UI SDK `Dialog` selects its initial focus by DOM attribute and React renders no `autofocus` attribute. A `data-autofocus` passed by the caller takes precedence. Without an `autoFocus` input, the `Dialog` focuses its first tabbable element. The attribute can be removed when the UI SDK reads the React `autoFocus` prop itself.
- `delino-ui/switch`
  - `Switch`: a labeled on/off setting. Props: `label` (required), `description`, and all UI SDK `Switch` props such as `checked`, `defaultChecked`, `onCheckedChange`, `name` and `disabled`. It renders with role `switch` and a hidden native input for forms. The thumb travels to the inline-end edge, so it mirrors under `dir="rtl"`.
- `delino-ui/dialog`
  - `Dialog`: the modal dialog root. It accepts the UI SDK `Dialog` props except `dismissible`, `modal` and `presentation`, which are removed so that every dialog is modal with a backdrop, closes with the Escape key and is centered. Use `Drawer` for a sheet.
  - `DialogTrigger` and `DialogClose`: buttons styled like `Button`, with `variant` and `size`. The trigger defaults to `Primary`, the close button to `Secondary`. A Promise returned from the `DialogClose` `action` keeps the dialog open until it settles.
  - `DialogContent`: the dialog panel. It also renders the backdrop.
  - `DialogTitle` (level 2 heading, names the dialog), `DialogDescription` and `DialogActions` (button row).
  - `createDialogController` and the `DialogController` type are re-exported from UI SDK for dialogs that open from outside a trigger.
- `delino-ui/textarea`
  - `Textarea`: a labeled multi-line text input. Props: `label` (required), `description`, `error`, and all UI SDK `Textarea` props such as `value`, `defaultValue`, `onValueChange`, `name`, `rows`, `autoResize` and `autoFocus`. It follows the same error and focus rules as `TextField`, including `autoFocus` taking the initial focus inside `Dialog` through `data-autofocus`.
- `delino-ui/checkbox`
  - `Checkbox`: a labeled checkbox. Props: `label` (required), `description`, and all UI SDK `Checkbox` props such as `checked`, `defaultChecked`, `onCheckedChange`, `name`, `value` and `disabled`. Pressing the label toggles it. The description describes the checkbox through `aria-describedby`; a caller `aria-describedby` is added before the description id instead of replacing it. It shows a check while checked and a dash while indeterminate. It wraps the checkbox in a native `label` instead of a UI SDK `Field`, so that it also works inside `CheckboxGroup`, which assigns each checkbox its own id.
  - `CheckboxGroup`: a styled UI SDK `CheckboxGroup` that keeps one list of checked `Checkbox` values. Consumers name it with `aria-label` or `aria-labelledby`.
  - The `CheckedState` type is re-exported from UI SDK.
- `delino-ui/radio-group`
  - `RadioGroup`: a labeled group with one checked value. Props: `label` (required, names the group), `description`, `error`, and all UI SDK `RadioGroup` props such as `value`, `defaultValue`, `onValueChange`, `name`, `required` and `disabled`. It follows the same error rules as `TextField`. Arrow keys move between options.
  - `RadioGroupItem`: one option. Props: `value`, `children` (the visible option name) and the other UI SDK `Radio` props.
- `delino-ui/select`
  - `Select`: a labeled single-value picker. Props: `label` (required), `description`, `error`, `placeholder`, `value`, `defaultValue`, `onValueChange`, `name`, `required`, `disabled` and the other UI SDK `Select` options except `multiple`. `className` applies to the trigger. It follows the same error rules as `TextField`. The value type is `string`. An untyped `multiple` value is ignored at runtime, so `Select` always stays single-value. A long value stays on one line and is truncated with an ellipsis instead of widening the trigger.
  - `SelectOption`: one option. Props: `value`, `children` (the visible option name), `disabled` and the other UI SDK `SelectItem` props. A check marks the selected option.
- `delino-ui/tabs`
  - `Tabs`, `TabsList`, `TabsTab` and `TabsPanel`: styled UI SDK tabs parts with all their props. Each tab and its panel use the same `value`. `orientation="vertical"` lays the tab list out as a column beside the panel, with the selection marker on the inline-end edge (it mirrors in right-to-left text). Consumers name `TabsList` with `aria-label`.
- `delino-ui/tooltip`
  - `Tooltip`, `TooltipTrigger` and `TooltipContent`: styled UI SDK tooltip parts with all their props. `TooltipContent` defaults `sideOffset` to 8 pixels. Tooltips hold plain text only, because they cannot receive focus; interactive content goes in a dialog.
- `delino-ui/toast`
  - `ToastRegion`: shows the toast queue at the bottom center of the viewport by default (`side`, `align`). Each toast shows its title, description, action button (`Primary`) and cancel button (`Secondary`). The region keeps a 16px offset from each viewport edge plus the device safe area (`env(safe-area-inset-*)`, which applies when the page sets `viewport-fit=cover`; the centered region and the `start`/`end` width also subtract the horizontal insets so they stay clear of both sides), and `align="start"` and `align="end"` follow the writing direction (logical `inset-inline-*` edges). It accepts the UI SDK `ToastRegion` props except `children`. Apps render it once near the root.
  - `toast` and `createToaster` are re-exported from UI SDK. `toast()` adds a toast from anywhere; `createToaster()` creates a separate queue for a region's `toaster` prop.
  - The `ToastData`, `Toaster` and `ToastType` types are re-exported from UI SDK.
- `delino-ui/alert-dialog`
  - `AlertDialog`: the alert dialog root. It accepts the UI SDK `AlertDialog` props except `dismissible`, which is removed so that every alert dialog closes with the Escape key. A press outside never closes it.
  - `AlertDialogTrigger` and `AlertDialogClose`: buttons styled like `Button`, with `variant` and `size`. The trigger defaults to `Primary`, the close button to `Secondary`. `AlertDialogClose` reports its `value` as the `returnValue` of `onOpenChange`; `value="confirm"` is the agreeing answer. Opening focuses the first close button, so consumers put the least destructive answer first.
  - `AlertDialogContent` (renders the backdrop), `AlertDialogTitle` (level 2 heading, names the dialog), `AlertDialogDescription` and `AlertDialogActions` share the `Dialog` styles.
  - `createAlertDialogController`, `createConfirm` and the `AlertDialogController` type are re-exported from UI SDK.
- `delino-ui/avatar`
  - `Avatar`: a round picture. Props: `alt` (required), `fallback` (required, usually initials), `src`, `size` and the other UI SDK `Avatar` props. An empty or whitespace-only `src` counts as no URL: no image renders and the fallback shows. The fallback shows until the image loads and stays when it fails; it has role `img` and the `alt` text as its name.
  - `AvatarSize`: `Small` (`small`, 24px), `Medium` (`medium`, 40px), `Large` (`large`, 64px). The default is `Medium`.
- `delino-ui/progress`
  - `Progress`: a named progress bar. Props: `label` (required), `showValue` (shows the formatted value, such as `40%`), and all UI SDK `Progress` props such as `value`, `min`, `max` and `format`. A `null` value shows an indeterminate bar.
- `delino-ui/separator`
  - `Separator`: a styled UI SDK `Separator` with all its props, such as `orientation`.
- `delino-ui/password-input`
  - `PasswordInput`: a labeled password field with a show/hide button. Props: `label` (required), `description`, `error`, `toggleLabel` (the button's accessible name, default `Show password`), `placeholder`, `autoFocus`, and all UI SDK `PasswordInput` props such as `value`, `defaultValue`, `onValueChange`, `visible`, `autoComplete`, `name`, `required` and `disabled`. The button keeps the same name in both states and reports the state with `aria-pressed`. It follows the same error and focus rules as `TextField`. With `autoFocus`, the component sets `data-autofocus` on its control so the input becomes the initial focus target inside `Dialog`, even when the close button comes first; a `data-autofocus` passed by the caller is ignored, because only the control may be the initial focus target and a caller attribute on the non-tabbable root element would take the focus away from it. A caller `aria-describedby`, `aria-label` or `aria-labelledby` goes to the input, not to the root element, which has no role; the caller `aria-describedby` ids come before the description and error ids.
- `delino-ui/number-input`
  - `NumberInput`: a labeled number field with minus and plus buttons. Props: `label` (required), `description`, `error`, `decrementLabel` and `incrementLabel` (button names, default `Decrease` and `Increase`), `placeholder`, `autoFocus`, and all UI SDK `NumberInput` props such as `value`, `defaultValue`, `onValueChange`, `onValueCommit`, `min`, `max`, `step`, `format`, `name` and `disabled`. An empty input has the value `null`. It follows the same error and focus rules as `TextField`. With `autoFocus`, the component sets `data-autofocus` on its control so the input becomes the initial focus target inside `Dialog`, even when the close button comes first; a `data-autofocus` passed by the caller is ignored, because only the control may be the initial focus target and a caller attribute on the non-tabbable root element would take the focus away from it. A caller `aria-describedby`, `aria-label` or `aria-labelledby` goes to the input, not to the root element, which has no role; the caller `aria-describedby` ids come before the description and error ids.
- `delino-ui/slider`
  - `Slider`: a labeled slider. Props: `label` (required), `description`, `showValue` (shows the formatted value), `thumbLabels` (the accessible names of the thumbs of a range), and all UI SDK `Slider` props such as `value`, `defaultValue`, `onValueChange`, `onValueCommit`, `min`, `max`, `step`, `format`, `name` and `disabled`. A number value, or an array of one value, renders one thumb named by `label`, and `thumbLabels` does not apply to it; an array of more than one value is a range and renders one thumb per value, named by `thumbLabels`.
- `delino-ui/segmented-control`
  - `SegmentedControl` and `SegmentedControlItem`: a row of mutually exclusive choices with a highlight that slides to the checked item. They accept all UI SDK `SegmentedControl` and `SegmentedControlItem` props. Consumers name the control with `aria-label` or a surrounding field label.
- `delino-ui/toggle`
  - `Toggle`: a styled UI SDK `Toggle` with all its props and `size` (`ButtonSize`, default `Medium`). It reports its state with `aria-pressed`.
  - `ToggleGroup`: a styled UI SDK `ToggleGroup` with all its props. Each `Toggle` inside has a `value`; one is pressed at a time unless `multiple` is set. Consumers name it with `aria-label`.
- `delino-ui/accordion`
  - `Accordion`: a styled UI SDK `Accordion` with all its props. The value is an array of open item values; one item opens at a time unless `multiple` is set.
  - `AccordionItem`: one section. Props: `title` (required, the heading text), `headingLevel` (`AccordionHeadingLevel`), `children` (the panel content) and the other UI SDK `AccordionItem` props such as `value` and `disabled`. It renders the heading, trigger button and panel; the panel height animates.
  - `AccordionHeadingLevel`: `H2` (`h2`) to `H6` (`h6`). The default is `H3`.
  - `createAccordionController` and the `AccordionController` type are re-exported from UI SDK.
- `delino-ui/collapsible`
  - `Collapsible`, `CollapsibleTrigger` and `CollapsiblePanel`: styled UI SDK collapsible parts with all their props. The trigger is styled like `Button`, with `variant` (default `Secondary`) and `size`. The panel height animates.
  - `createCollapsibleController` and the `CollapsibleController` type are re-exported from UI SDK.
- `delino-ui/popover`
  - `Popover`: the popover root. It accepts the UI SDK `Popover` props except `dismissible`, which is removed because the content has the dialog role and must close with the Escape key.
  - `PopoverTrigger` and `PopoverClose`: buttons styled like `Button`, with `variant` and `size`. Both default to `Secondary`; the close button defaults to `Small`.
  - `PopoverContent` (defaults `sideOffset` to 8 pixels), `PopoverTitle` (level 2 heading, names the popover) and `PopoverDescription`.
  - `createPopoverController` and the `PopoverController` type are re-exported from UI SDK.
- `delino-ui/menu`
  - `Menu`: a UI SDK `Menu` with all its props. Each `MenuItem` has a `value`, and `onAction` receives it. A Promise returned from `onAction` keeps the item pending and the menu open until it settles.
  - `MenuTrigger`: a button styled like `Button`, with `variant` (default `Secondary`) and `size`.
  - `MenuContent` (defaults `sideOffset` to 8 pixels), `MenuItem` (`destructive` shows the danger color), `MenuGroup`, `MenuGroupLabel` and `MenuSeparator`: styled UI SDK parts with all their props.
  - `createMenuController` and the `MenuController` type are re-exported from UI SDK.
- `delino-ui/drawer`
  - `Drawer`: a sheet that slides in from an edge of the screen. It accepts the UI SDK `Drawer` props, such as `side`, `snapPoints` and `modal`, except `dismissible`, which is removed so that every drawer closes with the Escape key.
  - `DrawerTrigger` and `DrawerClose`: buttons styled like `Button`, with `variant` and `size`. The trigger defaults to `Primary`, the close button to `Secondary`. A Promise returned from the `DrawerClose` `action` keeps the drawer open until it settles.
  - `DrawerContent` (renders the backdrop only for a modal drawer, that is when `modal` is unset or `true`; `modal={false}` and `"trap-focus"` render no backdrop, so the page stays usable and undimmed), `DrawerTitle` (level 2 heading, names the drawer), `DrawerDescription` and `DrawerActions` (button row). UI SDK moves the sheet and fades the backdrop; the styles set only size, surface and color.
  - `createDrawerController` and the `DrawerController` and `DrawerSide` types are re-exported from UI SDK.
- `delino-ui/pagination`
  - `Pagination`: a navigation landmark with previous and next steps, page links and ellipses for long ranges. Props: `aria-label` (required), `previousLabel` and `nextLabel` (step names, default `Previous page` and `Next page`), and all UI SDK `Pagination` props such as `page`, `defaultPage`, `onPageChange`, `pageCount`, `total`, `pageSize`, `siblings`, `boundaries` and `getHref`. With `getHref` the parts are links; without it they are buttons. The current page has `aria-current="page"`. The previous and next icons mirror under `dir="rtl"`.
  - `createPaginationController` and the `PaginationController` type are re-exported from UI SDK.
- `delino-ui/meter`
  - `Meter`: a named measurement bar within a known range, such as storage used. Props: `label` (required), `showValue`, and all UI SDK `Meter` props such as `value`, `min`, `max` and `format`. It shares the `Progress` styles. Use `Progress` for the progress of a task.
- `delino-ui/badge`
  - `Badge`: a short status or count label in a `span`, with all `span` props. It has no UI SDK part because it has no behavior. Its text must carry the meaning; color alone must not.
  - `BadgeVariant`: `Neutral` (`neutral`), `Accent` (`accent`), `Danger` (`danger`). The default is `Neutral`.
- `delino-ui/card`
  - `Card` (a `section`), `CardTitle` (level 3 heading), `CardDescription` and `CardActions` (button row): a static surface that groups related content, with all props of their elements. Consumers name the card with `aria-labelledby` pointing to its title, or with `aria-label`.
- `delino-ui/skeleton`
  - `Skeleton`: a pulsing placeholder for loading content. Props: `width` and `height` (CSS lengths; defaults `100%` and `1em`), `circle` (draws a circle; an omitted `width` or `height` follows the given side through a square `aspect-ratio`, so the placeholder stays round also for a percentage size, and both default to `1em` when both are omitted; give both to draw an ellipse; a circle keeps its size and is not stretched by a flex container, because the stylesheet sets `align-self: flex-start` on it), and the other `span` props except `children`. It is hidden from screen readers; consumers mark the loading region with `aria-busy`. The pulse stops under `prefers-reduced-motion`.
- `delino-ui/styles.css`
  - Declares the cascade layer order `ui-sdk, delino-ui`. UI SDK rules are in `ui-sdk`; package rules are in `delino-ui`. Unlayered app styles override both.
  - Includes the UI SDK rule that hides closed parts with `hidden`, because statically rendered pages do not get it from UI SDK before hydration.
  - Defines design tokens as `--dui-*` custom properties on `:root`, with dark values under `prefers-color-scheme: dark`. Text tokens meet a 4.5:1 contrast ratio, and control borders, focus rings and state indicators meet 3:1 (WCAG 2.1 SC 1.4.3 and SC 1.4.11). Focus rings meet 3:1 against the surface they sit on, including the inverted toast surface in dark mode, where `.dui-toast` uses the light-theme ring `#3182f6` (3.55:1 on `#f9fafb`) instead of the dark ring. The state indicators are the Progress, Meter and Slider fill and the Select option check. They use `--dui-color-accent-graphic` (`#1b64da` in light mode, `#4593fc` in dark mode: 3.54:1 on the dark track and 4.50:1 on the dark highlighted option) instead of `--dui-color-accent`, which stays `#1b64da` in both modes for filled controls with `#ffffff` text (Button, Badge, Switch, Checkbox).
  - Uses `dui-` class names. Every component accepts `className`; the package class comes first.

## Storage

The package has no storage.

## Security

- The package contains no secrets, network calls or browser storage.
- UI SDK adds CSS at runtime. A Content Security Policy without `'unsafe-inline'` in `style-src` must give UI SDK a nonce, as the [UI SDK styling guide](https://ui-sdk.dev/docs/react/styling-and-animation) describes. Current Delino apps set no Content Security Policy.

## Logging

The package does not log. UI SDK components accept a `debug` prop that logs state changes in development builds only; do not leave it in committed code.

## Build and Test

Run from the repository root:

- `pnpm install --frozen-lockfile`
- `pnpm exec turbo run typecheck --filter=delino-ui`: TypeScript type check.
- `pnpm exec turbo run test --filter=delino-ui`: Rstest unit tests for rendering, props, naming, selection, keyboard and dismissal behavior, and for static (server-rendered) markup: description and error id references, empty descriptions, disabled-field errors and caller ARIA routing. `src/components.test.tsx`, `src/disclosure-and-input.test.tsx` and `src/ssr-field-error.test.tsx` hold them.

Root `pnpm test` and `pnpm typecheck` include the package. The package has no `build` or `dev` script. CI does not run its unit tests yet; add them to the workflow and its planner when package checks enter CI.

## Dependencies and Integrations

- `@ui-sdk/core` and `@ui-sdk/react` (Apache-2.0), pinned to the same exact version in `dependencies`. UI SDK is in public beta and its APIs can change before general availability, so upgrade both packages together, read their changelogs and run the unit tests.
- `react` and `react-dom` 19 as peer dependencies.
- `@rstest/core`, `happy-dom`, `@testing-library/react`, `@testing-library/dom`, `typescript` and the React type packages as development dependencies.
- Consumers: no app uses the package yet. An app that starts using it adds `"delino-ui": "workspace:*"` to its dependencies and records the use in its own contract.

## Change Triggers

- Update [project-delino-ui.md](project-delino-ui.md) when exports, component IDs or the styling contract change.
- Update the contracts of consuming apps when an export changes in a way that breaks them.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when the package enters CI.
- Update `packages/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-ui.md](project-delino-ui.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [UI SDK documentation](https://ui-sdk.dev/docs)
- [UI SDK source](https://github.com/vercel-labs/ui)
- [Rstest documentation](https://rstest.rs/)
