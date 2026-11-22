# Project: delino-ui

## Goal

Provide one shared set of React UI components for Delino web apps, so that each app does not rebuild buttons, form controls, dialogs, navigation and feedback components with its own behavior and accessibility rules. The components are built on the headless [UI SDK](https://ui-sdk.dev) from Vercel Labs, which supplies keyboard navigation, focus management, dismissal and ARIA. This package adds the Delino visual style.

## Project ID

`delino-ui`

## Domain Ownership Map

- `packages/delino-ui/`: workspace package `delino-ui` with React components, a shared stylesheet and unit tests.

The project is a library, not a deployable. It has no `apps`, `cmds`, `servers`, `crates` or `protos` paths.

## Domain Contract Documents

- [packages-delino-ui-foundation.md](packages-delino-ui-foundation.md)

## Cross-Domain Invariants

- Component IDs, which are also the package subpath exports:

| Component ID        | Export                        | UI SDK parts used                                                                                                                    |
| ------------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `accordion`         | `delino-ui/accordion`         | `Accordion`, `AccordionItem`, `AccordionHeader`, `AccordionTrigger`, `AccordionPanel`                                                |
| `alert-dialog`      | `delino-ui/alert-dialog`      | `AlertDialog` and its trigger, backdrop, content, title, description and close parts                                                 |
| `avatar`            | `delino-ui/avatar`            | `Avatar`, `AvatarImage`, `AvatarFallback`                                                                                            |
| `badge`             | `delino-ui/badge`             | None. Static text with no behavior.                                                                                                  |
| `button`            | `delino-ui/button`            | `Button`                                                                                                                             |
| `card`              | `delino-ui/card`              | None. Static container with no behavior.                                                                                             |
| `checkbox`          | `delino-ui/checkbox`          | `Checkbox`, `CheckboxIndicator`, `CheckboxGroup`                                                                                     |
| `collapsible`       | `delino-ui/collapsible`       | `Collapsible`, `CollapsibleTrigger`, `CollapsiblePanel`                                                                              |
| `dialog`            | `delino-ui/dialog`            | `Dialog` and its trigger, backdrop, content, title, description and close parts                                                      |
| `drawer`            | `delino-ui/drawer`            | `Drawer` and its trigger, backdrop, content, title, description and close parts                                                      |
| `menu`              | `delino-ui/menu`              | `Menu`, `MenuTrigger`, `MenuContent`, `MenuItem`, `MenuGroup`, `MenuGroupLabel`, `MenuSeparator`                                     |
| `meter`             | `delino-ui/meter`             | `Meter`, `MeterLabel`, `MeterValue`, `MeterTrack`, `MeterIndicator`                                                                  |
| `number-input`      | `delino-ui/number-input`      | `Field` parts, `NumberInput`, `NumberInputGroup`, `NumberInputControl`, `NumberInputDecrement`, `NumberInputIncrement`               |
| `pagination`        | `delino-ui/pagination`        | `Pagination`, `PaginationList`, `PaginationItem`, `PaginationLink`, `PaginationPrevious`, `PaginationNext`, `PaginationEllipsis`     |
| `password-input`    | `delino-ui/password-input`    | `Field` parts, `PasswordInput`, `PasswordInputControl`, `PasswordInputToggle`                                                        |
| `popover`           | `delino-ui/popover`           | `Popover` and its trigger, content, title, description and close parts                                                               |
| `progress`          | `delino-ui/progress`          | `Progress`, `ProgressLabel`, `ProgressValue`, `ProgressTrack`, `ProgressIndicator`                                                   |
| `radio-group`       | `delino-ui/radio-group`       | `Field`, `FieldLabel`, `FieldDescription`, `FieldError`, `RadioGroup`, `Radio`, `RadioIndicator`                                     |
| `segmented-control` | `delino-ui/segmented-control` | `SegmentedControl`, `SegmentedControlIndicator`, `SegmentedControlItem`                                                              |
| `select`            | `delino-ui/select`            | `Field` parts, `Select` and its trigger, value, content, item, item text and indicator                                               |
| `separator`         | `delino-ui/separator`         | `Separator`                                                                                                                          |
| `skeleton`          | `delino-ui/skeleton`          | None. Static placeholder with no behavior.                                                                                           |
| `slider`            | `delino-ui/slider`            | `Field`, `FieldLabel`, `FieldDescription`, `Slider`, `SliderValue`, `SliderControl`, `SliderTrack`, `SliderIndicator`, `SliderThumb` |
| `switch`            | `delino-ui/switch`            | `Field`, `FieldLabel`, `FieldDescription`, `Switch`, `SwitchThumb`                                                                   |
| `tabs`              | `delino-ui/tabs`              | `Tabs`, `TabsList`, `TabsTab`, `TabsPanel`                                                                                           |
| `text-field`        | `delino-ui/text-field`        | `Field`, `FieldLabel`, `FieldDescription`, `FieldError`, `Input`                                                                     |
| `textarea`          | `delino-ui/textarea`          | `Field`, `FieldLabel`, `FieldDescription`, `FieldError`, `Textarea`                                                                  |
| `toast`             | `delino-ui/toast`             | `ToastRegion`, `Toast`, `ToastTitle`, `ToastDescription`, `ToastAction`, `ToastClose`                                                |
| `toggle`            | `delino-ui/toggle`            | `Toggle`, `ToggleGroup`                                                                                                              |
| `tooltip`           | `delino-ui/tooltip`           | `Tooltip`, `TooltipTrigger`, `TooltipContent`                                                                                        |
| `styles`            | `delino-ui/styles.css`        | None. Design tokens and component styles.                                                                                            |

- Consumers import components only from the exports above. They must not import files under `packages/delino-ui/src/` by path.
- Apps that use the components must import `delino-ui/styles.css` before their own stylesheet.
- Behavior, focus handling and ARIA come from UI SDK. This package must not reimplement them; it only composes UI SDK parts and styles them. Components without behavior (`badge`, `card` and `skeleton`) render plain elements.
- The package follows the [frontend design rules](../AGENTS.md#frontend-design-rules): dialogs, alert dialogs, drawers and popovers always close with the Escape key, and the single critical input of a form receives focus when the form is shown.
- The package ships TypeScript source and has no build step, so it never writes a `dist` directory. Consumers compile the source with their own Rsbuild or Rspress build.
- Deviation from repository defaults: the primary language is TypeScript, not Go, because the consumers are React web apps.

## Change Policy

- Update [packages-delino-ui-foundation.md](packages-delino-ui-foundation.md) and this index together when exports, component IDs, the UI SDK version or the styling contract change.
- Update the owning app contract when an app starts or stops using the package.
- Update the [documentation catalog](README.md) when contracts are added, renamed or removed.
- Update `packages/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).
- Record implementation status, validation results and unresolved limits in pull requests, Linear issues and CI logs/artifacts.

## References

- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [project-template.md](project-template.md)
- [UI SDK documentation](https://ui-sdk.dev/docs)
