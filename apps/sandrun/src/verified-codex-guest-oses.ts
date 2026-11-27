// SPDX-License-Identifier: Apache-2.0
import type { GuestOs } from "@delino/sandrun-protos/v1/common_pb";

// Keep this list in sync with profile.VerifiedCodexGuestOSes in sandrun-api.
const VERIFIED_CODEX_GUEST_OSES: readonly GuestOs[] = [];

export function getVerifiedCodexGuestOses(): readonly GuestOs[] {
  return VERIFIED_CODEX_GUEST_OSES;
}
