// SPDX-License-Identifier: Apache-2.0
import { UuidV7Generator } from "tide-sdk";

const ids = new UuidV7Generator();

/** A new UUID v7, for the identifiers of the mock API. */
export function uuidV7(): string {
  return ids.next();
}
