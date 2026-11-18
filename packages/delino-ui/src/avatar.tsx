// SPDX-License-Identifier: Apache-2.0
import {
  Avatar as AvatarRoot,
  AvatarFallback,
  AvatarImage,
} from "@ui-sdk/react/avatar";
import type * as React from "react";
import { mergeClassName } from "./class-name.ts";

/** The diameter of an avatar. */
export enum AvatarSize {
  /** 24px, for dense lists. */
  Small = "small",
  /** 40px. */
  Medium = "medium",
  /** 64px, for profile headers. */
  Large = "large",
}

export type AvatarProps = Omit<
  React.ComponentProps<typeof AvatarRoot>,
  "children"
> & {
  /** The image URL. Without one, with a blank one, or when it fails to load, the fallback shows. */
  src?: string;
  /** The image's text alternative, usually the person's name. */
  alt: string;
  /** What shows until the image loads, usually initials. */
  fallback: React.ReactNode;
  size?: AvatarSize;
};

/**
 * A round picture of a person or an organization, built from UI SDK
 * `Avatar`. The fallback shows until the image loads and stays when it fails.
 */
export function Avatar({
  src,
  alt,
  fallback,
  size = AvatarSize.Medium,
  className,
  ...props
}: AvatarProps) {
  return (
    <AvatarRoot
      data-size={size}
      className={mergeClassName("dui-avatar", className)}
      {...props}
    >
      {/* The type allows only a string, but untyped callers can pass null or
          another value; treat anything that is not a string as no URL. The
          original value is passed through untrimmed; only the check ignores
          blanks. */}
      {typeof src === "string" && src.trim() !== "" ? (
        <AvatarImage className="dui-avatar-image" src={src} alt={alt} />
      ) : null}
      {/* The fallback stands in for the image, so it carries the same name. */}
      <AvatarFallback
        className="dui-avatar-fallback"
        role="img"
        aria-label={alt}
      >
        {fallback}
      </AvatarFallback>
    </AvatarRoot>
  );
}
