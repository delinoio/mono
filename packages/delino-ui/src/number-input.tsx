// SPDX-License-Identifier: Apache-2.0
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@ui-sdk/react/field";
import {
  isPartialNumber,
  numberFormatter,
  parseNumber,
} from "@ui-sdk/core/number";
import {
  NumberInputControl,
  NumberInputDecrement,
  NumberInputGroup,
  NumberInputIncrement,
  NumberInput as NumberInputRoot,
} from "@ui-sdk/react/number-input";
import { useEffect, useImperativeHandle, useRef } from "react";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";
import { useFieldAutoFocus } from "./panel-autofocus.ts";

export interface NumberInputDraft {
  /** Whether the text currently contains a fractional value. */
  hasFraction: () => boolean;
  /** The typed value below `min` when `preserveBelowMin` is enabled. */
  valueBelowMin: () => number | null;
  /** Whether the draft has a fraction or fails enabled bounds validation. */
  isInvalid: () => boolean;
}

export type NumberInputProps = Omit<
  React.ComponentProps<typeof NumberInputRoot>,
  "children"
> & {
  /** The visible name of the field. */
  label: React.ReactNode;
  /** Help text shown below the input. */
  description?: React.ReactNode;
  /**
   * An error message to show. A non-empty message marks the field as
   * invalid. Without one, the field shows the browser's message after a
   * failed submit.
   */
  error?: React.ReactNode;
  /** The accessible name of the minus button. The default is "Decrease". */
  decrementLabel?: string;
  /** The accessible name of the plus button. The default is "Increase". */
  incrementLabel?: string;
  /** Shown while the input is empty. */
  placeholder?: string;
  /** Focuses the input when it is shown. */
  autoFocus?: boolean;
  /**
   * Gives the form access to the typed draft in a plain decimal field.
   * When supplied, Enter and blur keep fractions for the form to validate.
   */
  draftRef?: React.Ref<NumberInputDraft>;
  /** With `draftRef`, keeps typed values below `min` for form validation. */
  preserveBelowMin?: boolean;
  /**
   * Preserves nonempty drafts outside the bounds or without a numeric value.
   * Use with draftRef for a plain decimal field that requires whole numbers.
   */
  validateDraftBounds?: boolean;
  /** Reports the validity of the text each time the user types or clears it. */
  onDraftChange?: (invalid: boolean) => void;
};

function draftLocale(input: HTMLInputElement, locale: string | undefined) {
  return (
    locale ??
    (input.closest("[lang]")?.getAttribute("lang") || navigator.language)
  );
}

function hasFraction(text: string, value: number | null, locale: string) {
  if (value !== null && !Number.isInteger(value)) return true;

  // Parsing can round a fractional draft to an integer. Check the typed
  // digits after the decimal separator before allowing that integer through.
  const formatter = numberFormatter(locale);
  const separator = formatter
    .formatToParts(0.1)
    .find((part) => part.type === "decimal")?.value;
  if (!separator) return false;
  const [, fraction] = text.split(separator);
  if (!fraction) return false;
  return Boolean(
    parseNumber(fraction.replaceAll(formatter.format(0), ""), locale),
  );
}

function valueBelowMin(
  input: HTMLInputElement,
  locale: string,
  min: number | undefined,
) {
  if (min === undefined) return null;
  const value = parseNumber(input.value, locale);
  return value !== null && value < min ? value : null;
}

/**
 * A labeled number field with minus and plus buttons, built from UI SDK
 * `Field` and `NumberInput`. Typing, arrow keys and the buttons change the
 * value; Enter or leaving the input clamps it to `min` and `max` and snaps
 * it to `step` unless draft validation preserves the text. Forms can use
 * `draftRef` and `preserveBelowMin` to validate fractional and below-minimum
 * drafts. An empty input has the value `null`.
 */
export function NumberInput({
  label,
  description,
  error,
  decrementLabel = "Decrease",
  incrementLabel = "Increase",
  placeholder,
  autoFocus,
  className,
  disabled,
  draftRef,
  preserveBelowMin,
  validateDraftBounds = false,
  onDraftChange,
  ...props
}: NumberInputProps) {
  // A caller `data-autofocus` would be spread onto the non-tabbable root
  // `<div>`. The UI SDK Dialog takes the first `[data-autofocus]` match in
  // document order and does not try later matches, so a root match comes
  // before the control and makes the Dialog fall back to its first tabbable
  // element. Drop the attribute so only the control, which sets it from
  // `autoFocus`, can be the initial focus target. The prop types do not
  // declare it, so the cast reaches the key. Remove this when the UI SDK
  // Dialog skips non-tabbable `[data-autofocus]` matches.
  //
  // Caller `aria-describedby`, `aria-label` and `aria-labelledby` go to the
  // control too: the root `<div>` has no role, so they would name or describe
  // nothing there. The UI SDK merges the caller `aria-describedby` ahead of
  // the description and error ids on the control. All other caller props stay
  // on the root.
  const {
    "data-autofocus": _dataAutofocus,
    "aria-describedby": ariaDescribedBy,
    "aria-label": ariaLabel,
    "aria-labelledby": ariaLabelledBy,
    ...rootProps
  } = props as typeof props & {
    "data-autofocus"?: unknown;
  };
  const { autoFocus: forwardedAutoFocus, marker } = useFieldAutoFocus(
    autoFocus,
    disabled,
  );
  const inputRef = useRef<HTMLInputElement>(null);
  const { min, max, locale, format } = rootProps;
  const draftMin =
    draftRef && (preserveBelowMin || validateDraftBounds) ? min : undefined;
  useEffect(() => {
    const input = inputRef.current;
    if (!input || draftMin === undefined || draftMin < 0) return;

    // The SDK blocks a minus sign at a nonnegative minimum. Let negative
    // drafts reach its input handler so a form can report them. Remove when
    // the SDK supports validating typed values before clamping.
    const allowNegativeDraft = (event: InputEvent) => {
      if (
        event.defaultPrevented ||
        event.isComposing ||
        !event.inputType.startsWith("insert") ||
        event.data === null
      ) {
        return;
      }
      const start = input.selectionStart ?? input.value.length;
      const end = input.selectionEnd ?? start;
      const next =
        input.value.slice(0, start) + event.data + input.value.slice(end);
      const inputLocale = draftLocale(input, locale);
      if (
        isPartialNumber(next, inputLocale, format) &&
        !isPartialNumber(next, inputLocale, format, { allowNegative: false })
      ) {
        event.stopImmediatePropagation();
      }
    };
    input.addEventListener("beforeinput", allowNegativeDraft, true);
    return () =>
      input.removeEventListener("beforeinput", allowNegativeDraft, true);
  }, [draftMin, format, locale]);
  function checkDraft(
    input: HTMLInputElement,
    checkBounds = validateDraftBounds,
  ) {
    const inputLocale = draftLocale(input, locale);
    const text = input.value;
    const value = parseNumber(text, inputLocale);
    return (
      (checkBounds &&
        text.trim() !== "" &&
        (value === null ||
          value < (min ?? -Infinity) ||
          value > (max ?? Infinity))) ||
      hasFraction(text, value, inputLocale)
    );
  }
  useImperativeHandle(
    draftRef,
    () => ({
      hasFraction: () =>
        inputRef.current !== null && checkDraft(inputRef.current, false),
      valueBelowMin: () => {
        const input = inputRef.current;
        return input
          ? valueBelowMin(input, draftLocale(input, locale), draftMin)
          : null;
      },
      isInvalid: () =>
        inputRef.current !== null && checkDraft(inputRef.current),
    }),
    [draftMin, locale],
  );
  const keepDraft = (input: HTMLInputElement) =>
    valueBelowMin(input, draftLocale(input, locale), draftMin) !== null ||
    checkDraft(input);
  const validateDraft = draftRef || onDraftChange || validateDraftBounds;
  const hasError =
    error !== undefined && error !== null && error !== false && error !== "";
  const hasDescription =
    description !== undefined &&
    description !== null &&
    description !== false &&
    description !== "";

  return (
    <Field
      className="dui-text-field"
      disabled={disabled}
      invalid={hasError ? true : undefined}
    >
      <FieldLabel className="dui-text-field-label">{label}</FieldLabel>
      <NumberInputRoot
        className={mergeClassName("dui-number-input", className)}
        disabled={disabled}
        {...rootProps}
      >
        <NumberInputGroup className="dui-number-input-group">
          <NumberInputDecrement
            className="dui-number-input-button"
            aria-label={decrementLabel}
          >
            <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
              <path
                d="M3.5 8h9"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          </NumberInputDecrement>
          <NumberInputControl
            className="dui-number-input-control"
            placeholder={placeholder}
            autoFocus={forwardedAutoFocus}
            aria-describedby={ariaDescribedBy}
            aria-label={ariaLabel}
            aria-labelledby={ariaLabelledBy}
            data-autofocus={marker}
            // The UI SDK snaps typed text before the form submits on Enter or
            // after blur. Skip those SDK handlers while the draft is invalid.
            // The browser still submits on Enter and the SDK still handles steps.
            // Remove when the UI SDK supports validating unsnapped typed values.
            ref={draftRef ? inputRef : undefined}
            render={
              validateDraft
                ? ({ onInput, onKeyDown, onBlur, ...control }) => (
                    <input
                      {...control}
                      onInput={(event) => {
                        const invalid = onDraftChange
                          ? checkDraft(event.currentTarget)
                          : false;
                        onInput?.(event);
                        onDraftChange?.(invalid);
                      }}
                      onKeyDown={(event) => {
                        if (
                          event.key !== "Enter" ||
                          !keepDraft(event.currentTarget)
                        ) {
                          onKeyDown?.(event);
                        }
                      }}
                      onBlur={(event) => {
                        if (!keepDraft(event.currentTarget)) {
                          onBlur?.(event);
                        }
                      }}
                    />
                  )
                : undefined
            }
          />
          <NumberInputIncrement
            className="dui-number-input-button"
            aria-label={incrementLabel}
          >
            <svg aria-hidden="true" viewBox="0 0 16 16" fill="none">
              <path
                d="M3.5 8h9M8 3.5v9"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          </NumberInputIncrement>
        </NumberInputGroup>
      </NumberInputRoot>
      {/* Workaround: the UI SDK Field predicts the description id in
          aria-describedby before the field binds on the client, so static
          markup would point at a missing element. Always render the
          description and hide it when empty (display: none from the package
          styles). Remove when the UI SDK stops predicting an id for an
          unregistered description. */}
      <FieldDescription
        className="dui-text-field-description"
        hidden={!hasDescription}
      >
        {hasDescription ? description : null}
      </FieldDescription>
      {hasError ? (
        <FieldError className="dui-text-field-error">{error}</FieldError>
      ) : (
        <FieldError className="dui-text-field-error" />
      )}
    </Field>
  );
}
