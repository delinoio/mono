// SPDX-License-Identifier: Apache-2.0
import { GuestOs } from "@delino/sandrun-protos/v1/common_pb";
import { getVerifiedCodexGuestOses as actual } from "../src/verified-codex-guest-oses.ts";
import { PROFILE_INTEGRATION_FIXTURE } from "./profile-integration-fixture.ts";

export function getVerifiedCodexGuestOses(): readonly GuestOs[] {
  return sessionStorage.getItem(PROFILE_INTEGRATION_FIXTURE) === "codex"
    ? [GuestOs.LINUX]
    : actual();
}
